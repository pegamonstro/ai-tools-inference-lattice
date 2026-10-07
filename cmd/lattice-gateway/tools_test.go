package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Ollama answers a tool call with arguments as a JSON object; OpenAI clients
// expect a JSON string. Translating that, and synthesising the call id Ollama
// does not provide, is what makes an agent's tool loop work at all.
func TestToolCallsAreTranslatedToOpenAIShape(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]interface{}
		json.NewDecoder(r.Body).Decode(&got)
		if _, ok := got["tools"]; !ok {
			t.Error("tools were not forwarded to Ollama")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"hermes3:8b","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Paris"}}}]},"prompt_eval_count":5,"eval_count":7}`))
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	res, err := p.Execute(context.Background(), Request{
		Model:    "hermes3:8b",
		Messages: []interface{}{map[string]interface{}{"role": "user", "content": "weather?"}},
		Tools:    []interface{}{map[string]interface{}{"type": "function"}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	msg := res.Choices[0].Message
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.Type != "function" {
		t.Errorf("type = %q, want function", tc.Type)
	}
	if tc.Function.Name != "get_weather" {
		t.Errorf("name = %q", tc.Function.Name)
	}
	// The arguments must be a JSON *string*, not an object.
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments are not a JSON string: %q (%v)", tc.Function.Arguments, err)
	}
	if args["city"] != "Paris" {
		t.Errorf("arguments lost the payload: %v", args)
	}
	if tc.ID == "" {
		t.Error("tool call has no id — OpenAI clients key their reply on it")
	}

	if got := res.Choices[0].FinishReason; got != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", got)
	}
}

// A plain answer must not grow a tool_calls field: omitempty, not an empty array.
func TestPlainAnswerOmitsToolCalls(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"hermes3:8b","message":{"role":"assistant","content":"hello"},"prompt_eval_count":1,"eval_count":2}`))
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	res, err := p.Execute(context.Background(), Request{Model: "hermes3:8b", Messages: []interface{}{}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	b, _ := json.Marshal(res.Choices[0].Message)
	if strings.Contains(string(b), "tool_calls") {
		t.Errorf("a plain answer serialised tool_calls: %s", b)
	}
	if got := res.Choices[0].FinishReason; got != "stop" {
		t.Errorf("finish_reason = %q, want stop", got)
	}
}

// Tool results come back as ordinary messages, so the gateway must not reshape
// them; this pins that a `tool` role message reaches Ollama untouched.
func TestToolMessagesAreForwardedUnchanged(t *testing.T) {
	var seen []interface{}
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]interface{}
		json.NewDecoder(r.Body).Decode(&got)
		seen, _ = got["messages"].([]interface{})
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"hermes3:8b","message":{"role":"assistant","content":"ok"},"prompt_eval_count":1,"eval_count":1}`))
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	_, err := p.Execute(context.Background(), Request{
		Model: "hermes3:8b",
		Messages: []interface{}{
			map[string]interface{}{"role": "assistant", "content": "", "tool_calls": []interface{}{}},
			map[string]interface{}{"role": "tool", "content": `{"temp":18}`, "tool_call_id": "call_0"},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("Ollama saw %d messages, want 2", len(seen))
	}
	second, _ := seen[1].(map[string]interface{})
	if second["role"] != "tool" || second["tool_call_id"] != "call_0" {
		t.Errorf("tool message was reshaped: %v", second)
	}
}

// Ollama 0.34's native /api/chat parses replayed history tool-call arguments
// directly and rejects OpenAI's JSON-encoded-string convention with 400 "Value
// looks like object, but can't find closing '}' symbol", so an agent's tool
// loop breaks on every request after its first tool call. The gateway must
// re-emit arguments as objects on the way out, the mirror image of
// TestToolCallsAreTranslatedToOpenAIShape on the way in.
func TestHistoryToolCallArgumentsAreSentAsOllamaObjects(t *testing.T) {
	var seen []interface{}
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]interface{}
		json.NewDecoder(r.Body).Decode(&got)
		seen, _ = got["messages"].([]interface{})
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"granite4:3b","message":{"role":"assistant","content":"ok"},"prompt_eval_count":1,"eval_count":1}`))
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	_, err := p.Execute(context.Background(), Request{
		Model: "granite4:3b",
		Messages: []interface{}{
			map[string]interface{}{"role": "system", "content": "sys"},
			map[string]interface{}{"role": "assistant", "content": "note", "tool_calls": []interface{}{
				map[string]interface{}{
					"id":   "call_l4nkj368",
					"type": "function",
					"function": map[string]interface{}{
						"name": "write_file",
						// JSON-encoded string with escapes, the OpenAI wire shape.
						"arguments": "{\"content\":\"# Civilisation\\n\\nBody text\",\"path\":\"/home/x/README.md\"}",
					},
				},
			}},
			map[string]interface{}{"role": "tool", "content": "written", "tool_call_id": "call_l4nkj368"},
			map[string]interface{}{
				"role": "assistant", "content": "bad args",
				"tool_calls": []interface{}{
					map[string]interface{}{"id": "call_x", "type": "function",
						"function": map[string]interface{}{"name": "write_file", "arguments": "{not json"}},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(seen) != 4 {
		t.Fatalf("Ollama saw %d messages, want 4", len(seen))
	}
	m1 := seen[1].(map[string]interface{})
	m2 := seen[2].(map[string]interface{})
	m3 := seen[3].(map[string]interface{})
	if m1["role"] != "assistant" || m2["role"] != "tool" {
		t.Errorf("pass-through roles changed: %v %v", m1["role"], m2["role"])
	}
	if m2["tool_call_id"] != "call_l4nkj368" {
		t.Errorf("tool message id dropped: %v", m2)
	}
	calls := m1["tool_calls"].([]interface{})
	fn0 := calls[0].(map[string]interface{})["function"].(map[string]interface{})
	decoded, _ := json.Marshal(fn0["arguments"])
	var args0 map[string]interface{}
	if err := json.Unmarshal(decoded, &args0); err != nil {
		t.Fatalf("arguments were not a JSON object: %s (%v)", err, fn0["arguments"])
	}
	if !reflect.DeepEqual(args0, map[string]interface{}{
		"content": "# Civilisation\n\nBody text", "path": "/home/x/README.md",
	}) {
		t.Errorf("arguments %#v, want the original object", args0)
	}
	fn1 := m3["tool_calls"].([]interface{})[0].(map[string]interface{})["function"].(map[string]interface{})
	decodedBad, _ := json.Marshal(fn1["arguments"])
	var argsBad map[string]interface{}
	json.Unmarshal(decodedBad, &argsBad)
	if len(argsBad) != 0 {
		t.Errorf("invalid arguments became %#v, want empty object", fn1["arguments"])
	}
}

// Ollama streams tool calls as message.tool_calls events, with arguments as a
// JSON object. A streaming agent turn that asks for a tool previously came back
// empty because only content/thinking/reasoning were decoded; the stream path
// must translate tool calls exactly like the unary path and finish with
// "tool_calls" so the client's loop branches to the tool instead of ending.
func TestStreamingToolCallsAreTranslatedToOpenAIDeltas(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := `{"model":"granite4:3b","created_at":"t1","message":{"role":"assistant","content":""},"done":false}
` + `{"model":"granite4:3b","created_at":"t2","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"list_files","arguments":{"path":"/tmp"}}}]},"done":false}
` + `{"model":"granite4:3b","created_at":"t3","message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":11,"eval_count":3}`
		w.Write([]byte(body))
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	rec := httptest.NewRecorder()
	res, committed, err := p.ExecuteStream(context.Background(), Request{
		Model:    "granite4:3b",
		Messages: []interface{}{map[string]interface{}{"role": "user", "content": "list /tmp"}},
		Tools:    []interface{}{map[string]interface{}{"type": "function"}},
		Routing:  Routing{RequestID: "test-stream-tool"},
	}, rec)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if !committed {
		t.Fatal("stream was not committed")
	}

	sse := rec.Body.String()
	var toolFrame map[string]interface{}
	for _, line := range strings.Split(sse, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var chunk map[string]interface{}
		json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk)
		ch, ok := chunk["choices"].([]interface{})
		if !ok || len(ch) == 0 {
			continue
		}
		delta := ch[0].(map[string]interface{})["delta"].(map[string]interface{})
		if _, ok := delta["tool_calls"]; ok {
			toolFrame = chunk
		}
	}
	if toolFrame == nil {
		t.Fatalf("no SSE frame carried a tool_calls delta; stream was:\n%s", sse)
	}
	d := toolFrame["choices"].([]interface{})[0].(map[string]interface{})["delta"].(map[string]interface{})
	calls := d["tool_calls"].([]interface{})
	if len(calls) != 1 {
		t.Fatalf("delta carries %d tool calls, want 1", len(calls))
	}
	c0 := calls[0].(map[string]interface{})
	if c0["id"] == "" || c0["type"] != "function" {
		t.Errorf("tool call delta missing id/type: %v", c0)
	}
	fn := c0["function"].(map[string]interface{})
	if fn["name"] != "list_files" {
		t.Errorf("name = %v", fn["name"])
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("streamed arguments are not a JSON string: %q (%v)", fn["arguments"], err)
	}
	if args["path"] != "/tmp" {
		t.Errorf("arguments lost the payload: %v", args)
	}

	// The loop branches on finish_reason; an assembled Response must match too.
	if got := res.Choices[0].FinishReason; got != "tool_calls" {
		t.Errorf("assembled finish_reason = %q, want tool_calls", got)
	}
	if len(res.Choices[0].Message.ToolCalls) != 1 {
		t.Errorf("assembled Response lost the tool call: %#v", res.Choices[0].Message)
	}
}

// Streaming a plain-text answer must be untouched by the tool-call work: one
// content delta plus the stop frame.
func TestStreamingPlainTextStillStreams(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := `{"model":"granite4:3b","created_at":"t1","message":{"role":"assistant","content":"hel"},"done":false}
` + `{"model":"granite4:3b","created_at":"t2","message":{"role":"assistant","content":"lo"},"done":true,"prompt_eval_count":4,"eval_count":2}`
		w.Write([]byte(body))
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	rec := httptest.NewRecorder()
	res, _, err := p.ExecuteStream(context.Background(), Request{
		Model:    "granite4:3b",
		Messages: []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	}, rec)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if res.Choices[0].Message.Content != "hello" {
		t.Errorf("assembled content = %q, want hello", res.Choices[0].Message.Content)
	}
	if !strings.Contains(rec.Body.String(), `"content":"hel"`) ||
		!strings.Contains(rec.Body.String(), `"content":"lo"`) {
		t.Errorf("content deltas missing from SSE:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) ||
		!strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("terminal frames missing:\n%s", rec.Body.String())
	}
}

// TestNoToolCallsLeavesHistoryUntouched pins the pass-through path: a history
// without tool calls must serialize exactly as the client sent it.
func TestNoToolCallsLeavesHistoryUntouched(t *testing.T) {
	var seen []interface{}
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]interface{}
		json.NewDecoder(r.Body).Decode(&got)
		seen, _ = got["messages"].([]interface{})
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"granite4:3b","message":{"role":"assistant","content":"ok"},"prompt_eval_count":1,"eval_count":1}`))
	}))
	defer ollama.Close()

	p := &OllamaProvider{Endpoint: ollama.URL}
	_, err := p.Execute(context.Background(), Request{
		Model: "granite4:3b",
		Messages: []interface{}{
			map[string]interface{}{"role": "system", "content": "sys"},
			map[string]interface{}{"role": "user", "content": "hi"},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(seen) != 2 || seen[0].(map[string]interface{})["role"] != "system" ||
		seen[1].(map[string]interface{})["content"] != "hi" {
		t.Errorf("history was reshaped: %#v", seen)
	}
}
