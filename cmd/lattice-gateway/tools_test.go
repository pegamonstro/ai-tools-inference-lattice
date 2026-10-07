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
