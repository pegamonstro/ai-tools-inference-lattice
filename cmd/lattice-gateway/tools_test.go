package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	res, err := p.Execute(Request{
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
	res, err := p.Execute(Request{Model: "hermes3:8b", Messages: []interface{}{}})
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
	_, err := p.Execute(Request{
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
