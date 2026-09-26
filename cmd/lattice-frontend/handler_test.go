package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The frontend rebuilds nothing: it forwards the client's own body with only
// model and routing rewritten. Rebuilding dropped `tools` silently, which is
// fatal for an agent, so these cases pin the fields a client actually sends.
func TestBuildProxyBodyPreservesClientFields(t *testing.T) {
	raw := []byte(`{
		"model": "local-coder",
		"messages": [{"role": "user", "content": "hi"}],
		"stream": true,
		"temperature": 0.2,
		"max_tokens": 64,
		"stop": ["\n\n"],
		"tools": [{"type": "function", "function": {"name": "get_weather"}}],
		"tool_choice": "auto"
	}`)
	decision := Decision{Target: "mac-gateway", Endpoint: "http://example:8081", ModelName: "hermes3:8b"}

	out, err := buildProxyBody(raw, decision, "rid-1", map[string]interface{}{"reasoning_effort": "low"})
	if err != nil {
		t.Fatalf("buildProxyBody: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}

	if got["model"] != "hermes3:8b" {
		t.Errorf("model = %v, want the resolved name", got["model"])
	}
	for _, field := range []string{"messages", "stream", "temperature", "max_tokens", "stop", "tools", "tool_choice"} {
		if _, ok := got[field]; !ok {
			t.Errorf("%s was dropped — the frontend must not enumerate fields", field)
		}
	}
	if got["temperature"] != 0.2 {
		t.Errorf("temperature = %v, want 0.2", got["temperature"])
	}

	routing, ok := got["routing"].(map[string]interface{})
	if !ok {
		t.Fatalf("routing missing on the local path: %v", got["routing"])
	}
	if routing["request_id"] != "rid-1" {
		t.Errorf("routing.request_id = %v, want rid-1", routing["request_id"])
	}
	if params, ok := routing["provider_params"].(map[string]interface{}); !ok || params["reasoning_effort"] != "low" {
		t.Errorf("routing.provider_params = %v, want reasoning_effort low", routing["provider_params"])
	}
}

// A client-supplied routing envelope must never reach the cloud: cloud endpoints
// speak plain OpenAI and reject the extension object.
func TestBuildProxyBodyStripsRoutingOnCloudPath(t *testing.T) {
	raw := []byte(`{
		"model": "local-brain",
		"messages": [{"role": "user", "content": "hi"}],
		"routing": {"privacy": "CLOUD_ALLOWED", "request_id": "client-chose-this"}
	}`)
	decision := Decision{Target: "ollama-cloud-primary", Endpoint: "http://example:11434", ModelName: "gemma4:31b-cloud"}

	out, err := buildProxyBody(raw, decision, "rid-2", nil)
	if err != nil {
		t.Fatalf("buildProxyBody: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if _, ok := got["routing"]; ok {
		t.Errorf("routing leaked to the cloud path: %v", got["routing"])
	}
	if got["model"] != "gemma4:31b-cloud" {
		t.Errorf("model = %v, want the resolved name", got["model"])
	}
}

// A body that is not a JSON object cannot be rewritten; the caller must be told
// rather than sending the target something malformed.
func TestBuildProxyBodyRejectsNonObject(t *testing.T) {
	if _, err := buildProxyBody([]byte(`"just a string"`), Decision{ModelName: "x"}, "rid", nil); err == nil {
		t.Error("expected an error for a non-object body")
	}
}

func TestResolveRequestIDKeepsAClientSuppliedID(t *testing.T) {
	if got := resolveRequestID(Routing{RequestID: "client-id"}); got != "client-id" {
		t.Errorf("got %q, want the client's own id", got)
	}
}

func TestResolveRequestIDGeneratesWhenAbsent(t *testing.T) {
	got := resolveRequestID(Routing{})
	if got == "" {
		t.Fatal("generated id is empty — telemetry would be uncorrelatable")
	}
	if other := resolveRequestID(Routing{}); other == got {
		t.Errorf("two generated ids collided (%q) — they must be unique per request", got)
	}
}

// The id must be set before the control call, not merely before the proxy call:
// control is posted the typed request, so an id assigned later is one control
// never logged and the run is correlatable in two planes out of three.
func TestHandleChatCorrelatesAllThreePlanes(t *testing.T) {
	var sawControlID, sawTargetID string

	// Declared before the control handler that closes over it: the closure runs
	// at request time, but the name must already be in scope for it to compile.
	var target *httptest.Server

	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got Request
		json.NewDecoder(r.Body).Decode(&got)
		sawControlID = got.Routing.RequestID
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Decision{Target: "mac-gateway", Endpoint: target.URL, ModelName: "hermes3:8b"})
	}))
	defer control.Close()

	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]interface{}
		json.NewDecoder(r.Body).Decode(&got)
		if routing, ok := got["routing"].(map[string]interface{}); ok {
			sawTargetID, _ = routing["request_id"].(string)
		}
		if _, ok := got["tools"]; !ok {
			t.Error("tools did not survive the frontend")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-x","choices":[]}`))
	}))
	defer target.Close()

	oldControl, oldTelemetry := controlURL, telemetryPath
	controlURL = control.URL
	telemetryPath = t.TempDir() + "/telemetry-frontend.jsonl"
	defer func() { controlURL, telemetryPath = oldControl, oldTelemetry }()

	body := `{"model":"local-coder","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleChat(rec, req)

	header := rec.Header().Get("X-Request-Id")
	if header == "" {
		t.Fatal("no X-Request-Id header on the response")
	}
	if sawControlID != header {
		t.Errorf("control saw %q, response advertised %q", sawControlID, header)
	}
	if sawTargetID != header {
		t.Errorf("target saw %q, response advertised %q", sawTargetID, header)
	}

	logged, err := os.ReadFile(telemetryPath)
	if err != nil || !strings.Contains(string(logged), header) {
		t.Errorf("frontend telemetry does not carry %q: %v %s", header, err, logged)
	}
}
