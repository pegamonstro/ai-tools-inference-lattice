package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// intPtr returns a pointer to n, for the Request.MaxTokens field whose nil-ness
// distinguishes an absent max_tokens from an explicit zero.
func intPtr(n int) *int { return &n }

// A budget-truncated completion must not look like a complete one: Ollama's
// done_reason "length" has to survive as the OpenAI finish_reason "length".
func TestUnaryDoneReasonLengthMapsToFinishReason(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"cut"},"done_reason":"length","prompt_eval_count":1,"eval_count":32}`))
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	res, err := p.Execute(context.Background(), Request{
		Model:    "m",
		Messages: []interface{}{map[string]interface{}{"role": "user", "content": "go"}},
		Routing:  Routing{RequestID: "test-finish-length"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := res.Choices[0].FinishReason; got != "length" {
		t.Errorf("finish_reason = %q, want length", got)
	}
}

// The unary mapping table: only "length" is special, and a tool call always
// wins so the client's tool loop branches instead of ending the turn.
func TestUnaryDoneReasonMapping(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "length",
			body: `{"model":"m","message":{"role":"assistant","content":"cut"},"done_reason":"length","prompt_eval_count":1,"eval_count":32}`,
			want: "length",
		},
		{
			name: "length is case and space insensitive",
			body: `{"model":"m","message":{"role":"assistant","content":"cut"},"done_reason":"  LENGTH ","prompt_eval_count":1,"eval_count":32}`,
			want: "length",
		},
		{
			name: "stop",
			body: `{"model":"m","message":{"role":"assistant","content":"done"},"done_reason":"stop","prompt_eval_count":1,"eval_count":2}`,
			want: "stop",
		},
		{
			name: "absent done_reason",
			body: `{"model":"m","message":{"role":"assistant","content":"done"},"prompt_eval_count":1,"eval_count":2}`,
			want: "stop",
		},
		{
			name: "unknown done_reason is a stop",
			body: `{"model":"m","message":{"role":"assistant","content":"done"},"done_reason":"load","prompt_eval_count":1,"eval_count":2}`,
			want: "stop",
		},
		{
			name: "tool calls beat length",
			body: `{"model":"m","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Paris"}}}]},"done_reason":"length","prompt_eval_count":1,"eval_count":2}`,
			want: "tool_calls",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			defer ollama.Close()

			p := &OllamaProvider{Endpoint: ollama.URL}
			res, err := p.Execute(context.Background(), Request{
				Model:    "m",
				Messages: []interface{}{map[string]interface{}{"role": "user", "content": "go"}},
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got := res.Choices[0].FinishReason; got != tc.want {
				t.Errorf("finish_reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// streamingOllama serves newline-delimited Ollama JSON events.
func streamingOllama(t *testing.T, body string) *OllamaProvider {
	t.Helper()
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(ollama.Close)
	return &OllamaProvider{Endpoint: ollama.URL}
}

// terminalFinishReasons returns the finish_reason of every terminal data frame
// (a chunk whose choices[0].finish_reason is non-null).
func terminalFinishReasons(t *testing.T, sse string) []string {
	t.Helper()
	var got []string
	for _, line := range strings.Split(sse, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("bad SSE chunk %q: %v", line, err)
		}
		choices, _ := chunk["choices"].([]interface{})
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]interface{})
		if fr, ok := choice["finish_reason"].(string); ok {
			got = append(got, fr)
		}
	}
	return got
}

// The terminal SSE frame and the assembled Response must both report "length"
// so a streaming client can tell a truncated answer from a complete one.
func TestStreamingDoneReasonLengthReachesTerminalFrameAndResponse(t *testing.T) {
	body := `{"model":"granite4:3b","message":{"role":"assistant","content":"cut"},"done":false}
` + `{"model":"granite4:3b","message":{"role":"assistant","content":""},"done":true,"done_reason":"length","prompt_eval_count":1,"eval_count":32}`
	p := streamingOllama(t, body)

	rec := httptest.NewRecorder()
	res, committed, err := p.ExecuteStream(context.Background(), Request{
		Model:    "granite4:3b",
		Messages: []interface{}{map[string]interface{}{"role": "user", "content": "go"}},
		Routing:  Routing{RequestID: "test-stream-length"},
	}, rec)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if !committed {
		t.Fatal("stream was not committed")
	}

	sse := rec.Body.String()
	if !strings.Contains(sse, `"finish_reason":"length"`) {
		t.Errorf("terminal SSE frame is missing finish_reason length:\n%s", sse)
	}
	reasons := terminalFinishReasons(t, sse)
	if len(reasons) != 1 || reasons[0] != "length" {
		t.Errorf("terminal finish_reasons = %v, want exactly [length]", reasons)
	}
	if got := res.Choices[0].FinishReason; got != "length" {
		t.Errorf("assembled finish_reason = %q, want length", got)
	}
	if res.Choices[0].Message.Content != "cut" {
		t.Errorf("assembled content = %q, want cut", res.Choices[0].Message.Content)
	}
	if !strings.Contains(sse, "[DONE]") {
		t.Errorf("stream did not terminate with [DONE]:\n%s", sse)
	}
}

// A stream whose terminal event carries no done_reason (or "stop") must keep
// the old behaviour, and a streamed tool call still wins over done_reason.
func TestStreamingDoneReasonStopAndToolCallPrecedence(t *testing.T) {
	t.Run("absent done_reason is stop", func(t *testing.T) {
		body := `{"model":"granite4:3b","message":{"role":"assistant","content":"hi"},"done":false}
` + `{"model":"granite4:3b","message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":1,"eval_count":1}`
		p := streamingOllama(t, body)

		rec := httptest.NewRecorder()
		res, _, err := p.ExecuteStream(context.Background(), Request{
			Model:    "granite4:3b",
			Messages: []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		}, rec)
		if err != nil {
			t.Fatalf("ExecuteStream: %v", err)
		}
		if got := res.Choices[0].FinishReason; got != "stop" {
			t.Errorf("assembled finish_reason = %q, want stop", got)
		}
		if reasons := terminalFinishReasons(t, rec.Body.String()); len(reasons) != 1 || reasons[0] != "stop" {
			t.Errorf("terminal finish_reasons = %v, want [stop]", reasons)
		}
	})

	t.Run("tool_calls beats length", func(t *testing.T) {
		body := `{"model":"granite4:3b","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"list_files","arguments":{"path":"/tmp"}}}]},"done":false}
` + `{"model":"granite4:3b","message":{"role":"assistant","content":""},"done":true,"done_reason":"length","prompt_eval_count":1,"eval_count":3}`
		p := streamingOllama(t, body)

		rec := httptest.NewRecorder()
		res, _, err := p.ExecuteStream(context.Background(), Request{
			Model:    "granite4:3b",
			Messages: []interface{}{map[string]interface{}{"role": "user", "content": "list"}},
			Tools:    []interface{}{map[string]interface{}{"type": "function"}},
		}, rec)
		if err != nil {
			t.Fatalf("ExecuteStream: %v", err)
		}
		if got := res.Choices[0].FinishReason; got != "tool_calls" {
			t.Errorf("assembled finish_reason = %q, want tool_calls", got)
		}
		if reasons := terminalFinishReasons(t, rec.Body.String()); len(reasons) != 1 || reasons[0] != "tool_calls" {
			t.Errorf("terminal finish_reasons = %v, want [tool_calls]", reasons)
		}
	})
}

// resolveMaxTokens precedence: max_budget wins, then a positive client
// max_tokens, then the 4096 default. A non-positive client value is not a cap.
func TestResolveMaxTokensPrecedence(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		want int
	}{
		{
			name: "max_budget beats client max_tokens",
			req: Request{
				MaxTokens: intPtr(321),
				Routing:   Routing{ProviderParams: map[string]interface{}{"max_budget": float64(777)}},
			},
			want: 777,
		},
		{
			name: "max_budget as int",
			req: Request{
				Routing: Routing{ProviderParams: map[string]interface{}{"max_budget": 777}},
			},
			want: 777,
		},
		{
			name: "max_budget as json.Number",
			req: Request{
				Routing: Routing{ProviderParams: map[string]interface{}{"max_budget": json.Number("777")}},
			},
			want: 777,
		},
		{
			name: "client max_tokens without max_budget",
			req:  Request{MaxTokens: intPtr(321)},
			want: 321,
		},
		{
			name: "empty request falls back to the default",
			req:  Request{},
			want: 4096,
		},
		{
			name: "explicit zero is not a cap",
			req:  Request{MaxTokens: intPtr(0)},
			want: 4096,
		},
		{
			name: "negative client value is not a cap",
			req:  Request{MaxTokens: intPtr(-5)},
			want: 4096,
		},
		{
			name: "invalid client value does not beat max_budget",
			req: Request{
				MaxTokens: intPtr(0),
				Routing:   Routing{ProviderParams: map[string]interface{}{"max_budget": float64(777)}},
			},
			want: 777,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveMaxTokens(tc.req); got != tc.want {
				t.Errorf("resolveMaxTokens = %d, want %d", got, tc.want)
			}
		})
	}
}

// The field must be wired to the wire format, not just to the struct: a client
// body carrying max_tokens has to decode and become the cap.
func TestClientMaxTokensDecodesAndBecomesTheCap(t *testing.T) {
	var req Request
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[],"max_tokens":555}`), &req); err != nil {
		t.Fatalf("decode Request: %v", err)
	}
	if req.MaxTokens == nil {
		t.Fatal("max_tokens did not decode into Request.MaxTokens")
	}
	if *req.MaxTokens != 555 {
		t.Errorf("Request.MaxTokens = %d, want 555", *req.MaxTokens)
	}
	if got := resolveMaxTokens(req); got != 555 {
		t.Errorf("resolveMaxTokens = %d, want 555", got)
	}

	// An absent field must stay nil so it cannot be mistaken for an explicit 0.
	var absent Request
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[]}`), &absent); err != nil {
		t.Fatalf("decode Request: %v", err)
	}
	if absent.MaxTokens != nil {
		t.Errorf("absent max_tokens decoded to %d, want nil", *absent.MaxTokens)
	}
	if got := resolveMaxTokens(absent); got != 4096 {
		t.Errorf("resolveMaxTokens = %d, want 4096", got)
	}
}

// --- budget-clamp inference ---------------------------------------------------
//
// The live regression: Ollama answered done_reason "stop" even though generation
// stopped at the num_predict we sent, so finish_reason never reached the client
// as "length". The budget itself is therefore the second truncation signal: a
// reply that generated at least the cap we sent is a clamp, whatever the
// provider calls it.

// ollamaClampMock answers a unary /api/chat with eval_count = num_predict + delta
// and the given done_reason, and hands back the num_predict it observed so the
// test can assert the cap was actually sent.
func ollamaClampMock(t *testing.T, doneReason string, delta int) (*OllamaProvider, <-chan int) {
	t.Helper()
	predictCh := make(chan int, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sent struct {
			Options map[string]interface{} `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("decode ollama request: %v", err)
		}
		predict := 0
		if v, ok := sent.Options["num_predict"].(float64); ok {
			predict = int(v)
		}
		predictCh <- predict

		resp := map[string]interface{}{
			"model":             "m",
			"message":           map[string]interface{}{"role": "assistant", "content": "cut"},
			"prompt_eval_count": 1,
			"eval_count":        predict + delta,
		}
		if doneReason != "" {
			resp["done_reason"] = doneReason
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return &OllamaProvider{Endpoint: srv.URL}, predictCh
}

// ollamaStreamClampMock is the streaming twin: a content event then a terminal
// event whose eval_count is num_predict + delta and whose done_reason is given.
func ollamaStreamClampMock(t *testing.T, doneReason string, delta int) *OllamaProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sent struct {
			Options map[string]interface{} `json:"options"`
		}
		_ = json.NewDecoder(r.Body).Decode(&sent)
		predict := 0
		if v, ok := sent.Options["num_predict"].(float64); ok {
			predict = int(v)
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]interface{}{
			"model":   "granite4:3b",
			"message": map[string]interface{}{"role": "assistant", "content": "cut"},
			"done":    false,
		})
		term := map[string]interface{}{
			"model":             "granite4:3b",
			"message":           map[string]interface{}{"role": "assistant", "content": ""},
			"done":              true,
			"prompt_eval_count": 1,
			"eval_count":        predict + delta,
		}
		if doneReason != "" {
			term["done_reason"] = doneReason
		}
		_ = enc.Encode(term)
	}))
	t.Cleanup(srv.Close)
	return &OllamaProvider{Endpoint: srv.URL}
}

func TestUnaryBudgetClampInferredAsLength(t *testing.T) {
	cases := []struct {
		name       string
		doneReason string
		delta      int
		want       string
	}{
		{name: "stop at the cap is length", doneReason: "stop", delta: 0, want: "length"},
		{name: "absent at the cap is length", doneReason: "", delta: 0, want: "length"},
		{name: "unknown reason at the cap is length", doneReason: "load", delta: 0, want: "length"},
		{name: "stop below the cap stays stop", doneReason: "stop", delta: -5, want: "stop"},
		{name: "absent below the cap stays stop", doneReason: "", delta: -5, want: "stop"},
		{name: "explicit length below the cap stays length", doneReason: "length", delta: -5, want: "length"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, predictCh := ollamaClampMock(t, tc.doneReason, tc.delta)
			res, err := p.Execute(context.Background(), Request{
				Model:    "m",
				Messages: []interface{}{map[string]interface{}{"role": "user", "content": "go"}},
				Routing:  Routing{ProviderParams: map[string]interface{}{"max_budget": float64(8)}},
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if predict := <-predictCh; predict != 8 {
				t.Fatalf("num_predict sent = %d, want 8", predict)
			}
			if got := res.Choices[0].FinishReason; got != tc.want {
				t.Errorf("finish_reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// A tool call still beats an inferred budget clamp: the client's tool loop must
// branch rather than the turn ending on a truncation heuristic.
func TestToolCallsBeatInferredLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model": "m",
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": "",
				"tool_calls": []interface{}{map[string]interface{}{
					"function": map[string]interface{}{"name": "get_weather", "arguments": map[string]interface{}{"city": "Paris"}},
				}},
			},
			"done_reason":       "stop",
			"prompt_eval_count": 1,
			"eval_count":        8,
		})
	}))
	defer srv.Close()

	p := &OllamaProvider{Endpoint: srv.URL}
	res, err := p.Execute(context.Background(), Request{
		Model:    "m",
		Messages: []interface{}{map[string]interface{}{"role": "user", "content": "go"}},
		Routing:  Routing{ProviderParams: map[string]interface{}{"max_budget": float64(8)}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := res.Choices[0].FinishReason; got != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", got)
	}
}

func TestStreamingBudgetClampInferredAsLength(t *testing.T) {
	cases := []struct {
		name       string
		doneReason string
		delta      int
		want       string
	}{
		{name: "stop at the cap is length", doneReason: "stop", delta: 0, want: "length"},
		{name: "absent at the cap is length", doneReason: "", delta: 0, want: "length"},
		{name: "stop below the cap stays stop", doneReason: "stop", delta: -5, want: "stop"},
		{name: "absent below the cap stays stop", doneReason: "", delta: -5, want: "stop"},
		{name: "explicit length below the cap stays length", doneReason: "length", delta: -5, want: "length"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ollamaStreamClampMock(t, tc.doneReason, tc.delta)
			rec := httptest.NewRecorder()
			res, committed, err := p.ExecuteStream(context.Background(), Request{
				Model:    "granite4:3b",
				Messages: []interface{}{map[string]interface{}{"role": "user", "content": "go"}},
				Routing:  Routing{ProviderParams: map[string]interface{}{"max_budget": float64(8)}},
			}, rec)
			if err != nil {
				t.Fatalf("ExecuteStream: %v", err)
			}
			if !committed {
				t.Fatal("stream was not committed")
			}
			if reasons := terminalFinishReasons(t, rec.Body.String()); len(reasons) != 1 || reasons[0] != tc.want {
				t.Errorf("terminal finish_reasons = %v, want [%s]", reasons, tc.want)
			}
			if got := res.Choices[0].FinishReason; got != tc.want {
				t.Errorf("assembled finish_reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- MLX / OpenAI-compatible provider -----------------------------------------

// MLX stream used to hardcode "stop" on the terminal frame and the assembled
// Response. The upstream finish_reason must now survive, and a "stop" the server
// put on a reply that consumed the whole max_tokens cap must infer as "length".
func TestMLXStreamFinishReasonComesFromUpstreamOrCap(t *testing.T) {
	cases := []struct {
		name       string
		upstream   string
		completion int
		want       string
	}{
		{name: "upstream length survives", upstream: "length", completion: 2, want: "length"},
		{name: "upstream stop at the cap is length", upstream: "stop", completion: 8, want: "length"},
		{name: "upstream stop below the cap is stop", upstream: "stop", completion: 2, want: "stop"},
		{name: "upstream tool_calls survives", upstream: "tool_calls", completion: 2, want: "tool_calls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, `data: {"id":"x","model":"mlx","choices":[{"index":0,"delta":{"content":"cut"},"finish_reason":null}]}`+"\n\n")
				io.WriteString(w, `data: {"id":"x","model":"mlx","choices":[{"index":0,"delta":{},"finish_reason":"`+tc.upstream+`"}],"usage":{"prompt_tokens":1,"completion_tokens":`+strconv.Itoa(tc.completion)+`,"total_tokens":9}}`+"\n\n")
				io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer srv.Close()

			p := &MLXProvider{Endpoint: srv.URL}
			rec := httptest.NewRecorder()
			res, committed, err := p.ExecuteStream(context.Background(), Request{
				Model:    "mlx",
				Messages: []interface{}{map[string]interface{}{"role": "user", "content": "go"}},
				Routing:  Routing{ProviderParams: map[string]interface{}{"max_budget": float64(8)}},
			}, rec)
			if err != nil {
				t.Fatalf("ExecuteStream: %v", err)
			}
			if !committed {
				t.Fatal("stream was not committed")
			}
			if reasons := terminalFinishReasons(t, rec.Body.String()); len(reasons) != 1 || reasons[0] != tc.want {
				t.Errorf("terminal finish_reasons = %v, want [%s]", reasons, tc.want)
			}
			if got := res.Choices[0].FinishReason; got != tc.want {
				t.Errorf("assembled finish_reason = %q, want %q", got, tc.want)
			}
			if got := res.Choices[0].Message.Content; got != "cut" {
				t.Errorf("assembled content = %q, want cut", got)
			}
		})
	}
}

// The MLX unary path decodes the upstream OpenAI Response directly; it must not
// turn a clamped reply into a clean "stop" either.
func TestMLXUnaryFinishReasonFromUpstreamOrCap(t *testing.T) {
	cases := []struct {
		name       string
		upstream   string
		completion int
		want       string
	}{
		{name: "upstream length survives", upstream: "length", completion: 2, want: "length"},
		{name: "upstream stop at the cap is length", upstream: "stop", completion: 8, want: "length"},
		{name: "upstream stop below the cap is stop", upstream: "stop", completion: 2, want: "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"id":      "x",
					"object":  "chat.completion",
					"model":   "mlx",
					"choices": []interface{}{map[string]interface{}{"index": 0, "message": map[string]interface{}{"role": "assistant", "content": "cut"}, "finish_reason": tc.upstream}},
					"usage":   map[string]interface{}{"prompt_tokens": 1, "completion_tokens": tc.completion, "total_tokens": 9},
				})
			}))
			defer srv.Close()

			p := &MLXProvider{Endpoint: srv.URL}
			res, err := p.Execute(context.Background(), Request{
				Model:    "mlx",
				Messages: []interface{}{map[string]interface{}{"role": "user", "content": "go"}},
				Routing:  Routing{ProviderParams: map[string]interface{}{"max_budget": float64(8)}},
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got := res.Choices[0].FinishReason; got != tc.want {
				t.Errorf("finish_reason = %q, want %q", got, tc.want)
			}
		})
	}
}
