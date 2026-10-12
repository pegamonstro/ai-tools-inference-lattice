package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
