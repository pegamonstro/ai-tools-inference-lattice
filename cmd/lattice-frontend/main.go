package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pegamonstro/ai-tools-inference-lattice/pkg/latticeconfig"
)

type Routing struct {
	Privacy        string                 `json:"privacy"`
	LatencyClass   string                 `json:"latency_class"`
	Parallelism    int                    `json:"parallelism"`
	RequestID      string                 `json:"request_id"`
	ProviderParams map[string]interface{} `json:"provider_params"`
}

type Request struct {
	Model    string        `json:"model"`
	Messages []interface{} `json:"messages"`
	Stream   bool          `json:"stream"`
	Routing  Routing       `json:"routing"`
}

type Decision struct {
	Target    string `json:"target"`
	Endpoint  string `json:"endpoint"`
	ModelName string `json:"model_name"`
}

// buildProxyBody forwards the client's own body with only two rewrites: the
// resolved model name, and the routing envelope (which belongs to the local
// translation layer only — a cloud endpoint speaks plain OpenAI and rejects it).
//
// It deliberately does not enumerate the fields it forwards. Enumerating is what
// broke this: the previous version rebuilt the body from model, messages and
// stream, so `tools` was dropped and an agent lost the ability to call anything.
func buildProxyBody(raw []byte, decision Decision, requestID string, providerParams map[string]interface{}) ([]byte, error) {
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if body == nil {
		return nil, fmt.Errorf("request body is not a JSON object")
	}

	body["model"] = decision.ModelName

	// The client's own routing envelope is never forwarded: on the local path it
	// is replaced by the one the frontend issues, and on the cloud path it must
	// not appear at all.
	delete(body, "routing")
	if decision.Target == "mac-gateway" {
		routing := map[string]interface{}{"request_id": requestID}
		if providerParams != nil {
			routing["provider_params"] = providerParams
		}
		body["routing"] = routing
	}

	return json.Marshal(body)
}

type Telemetry struct {
	RequestID     string  `json:"request_id"`
	TotalTime     float64 `json:"total_time_s"`
	ExecutionTime float64 `json:"execution_time_s"`
	Target        string  `json:"target"`
	Model         string  `json:"model,omitempty"`
	Error         string  `json:"error,omitempty"`
}

var controlURL = latticeconfig.Env("LATTICE_CONTROL_URL", "http://127.0.0.1:8082/route")

var telemetryPath = latticeconfig.Env("LATTICE_FRONTEND_TELEMETRY", "/var/log/lattice/telemetry-frontend.jsonl")

var capabilitiesURL = latticeconfig.Env("LATTICE_CONTROL_CAPABILITIES_URL", "http://127.0.0.1:8082/capabilities")

// handleModels serves the client-facing namespace. It reports the capability
// aliases because that is the namespace clients are expected to use, and the
// real context ceiling because a limit the client cannot see is a limit it will
// discover by being truncated.
func handleModels(w http.ResponseWriter, r *http.Request) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(capabilitiesURL)
	if err != nil {
		http.Error(w, "Control plane unavailable", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "Control plane: "+resp.Status, http.StatusServiceUnavailable)
		return
	}

	var caps struct {
		ContextLength int `json:"context_length"`
		Capabilities  []struct {
			ID string `json:"id"`
		} `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		http.Error(w, "Invalid capabilities from control plane", http.StatusInternalServerError)
		return
	}

	data := make([]map[string]interface{}, 0, len(caps.Capabilities))
	for _, c := range caps.Capabilities {
		data = append(data, map[string]interface{}{
			"id":             c.ID,
			"object":         "model",
			"created":        time.Now().Unix(),
			"owned_by":       "lattice",
			"context_length": caps.ContextLength,
		})
	}

	// Explicit: Go sniffs this as text/plain without the header, and strict
	// OpenAI clients reject that.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func logTelemetry(t Telemetry) {
	f, err := os.OpenFile(telemetryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Telemetry error: %v\n", err)
		return
	}
	defer f.Close()
	b, _ := json.Marshal(t)
	f.Write(append(b, '\n'))
}

// resolveRequestID uses the client's id when it supplied one, and otherwise
// mints one. An agent runtime has never heard of Lattice, so requiring the field
// meant its runs reached the Bee screen with a blank correlation key. The
// generated id is time-based on purpose: it sorts, so the display reads
// chronologically without parsing.
func resolveRequestID(r Routing) string {
	if r.RequestID != "" {
		return r.RequestID
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	tTotalStart := time.Now()

	var req Request
	var tExecStart time.Time

	// The line is deferred, not written after proxying, because the proxy aborts
	// the handler when its write to the client fails — a client that disconnects
	// mid-response used to take the frontend line with the unwind, leaving a
	// control line with no twin. Every exit path now draws exactly one line,
	// refusal included: a refusal is the event the routing policy exists to
	// produce, and it used to reach no stream at all.
	tele := Telemetry{}
	defer func() {
		// Keyed even when the body never parsed: a line the display cannot group
		// is a line it cannot show, and an unparsable body is worth showing.
		if tele.RequestID == "" {
			tele.RequestID = resolveRequestID(Routing{})
		}
		tele.TotalTime = time.Since(tTotalStart).Seconds()
		if !tExecStart.IsZero() {
			tele.ExecutionTime = time.Since(tExecStart).Seconds()
		}
		logTelemetry(tele)
	}()

	bodyBytes, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		tele.Error = err.Error()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Assigned before the control call so control logs the same id the gateway
	// and the frontend will use.
	req.Routing.RequestID = resolveRequestID(req.Routing)
	tele.RequestID = req.Routing.RequestID
	// The requested name, until the decision replaces it with the resolved one:
	// a request that never got a decision is still identified by what it asked
	// for.
	tele.Model = req.Model

	// 1. Ask Control Plane for Decision
	decisionReq, _ := json.Marshal(req)

	// HARDENING: Add timeout to Control Plane call
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(controlURL, "application/json", bytes.NewBuffer(decisionReq))
	if err != nil {
		tele.Error = "control plane unavailable or timed out"
		http.Error(w, "Control plane unavailable or timed out", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Propagate the control plane's error status and message (e.g. 503
		// "No healthy local gateway found") instead of masking it as a 500.
		body, _ := io.ReadAll(resp.Body)
		tele.Error = strings.TrimSpace(string(body))
		http.Error(w, "Control plane: "+string(body), resp.StatusCode)
		return
	}

	var decision Decision
	if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
		tele.Error = "invalid decision from control plane"
		http.Error(w, "Invalid decision from control plane", http.StatusInternalServerError)
		return
	}

	fmt.Printf("Routed [%s] to %s (%s)\n", req.Routing.RequestID, decision.Target, decision.Endpoint)
	tele.Target = decision.Target
	tele.Model = decision.ModelName

	// 2. Rewrite request for Target
	finalBodyBytes, err := buildProxyBody(bodyBytes, decision, req.Routing.RequestID, req.Routing.ProviderParams)
	if err != nil {
		tele.Error = err.Error()
		http.Error(w, "Malformed request body", http.StatusBadRequest)
		return
	}

	// 3. Proxy to Target
	tExecStart = time.Now()
	targetURL, _ := url.Parse(decision.Endpoint)
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	r.URL.Path = "/v1/chat/completions"
	r.Body = io.NopCloser(bytes.NewBuffer(finalBodyBytes))
	r.ContentLength = int64(len(finalBodyBytes))

	w.Header().Set("X-Request-Id", req.Routing.RequestID)

	proxy.ServeHTTP(w, r)
}

func main() {
	http.HandleFunc("/v1/chat/completions", handleChat)
	http.HandleFunc("/v1/models", handleModels)
	http.HandleFunc("/health", handleHealth)
	addr := latticeconfig.Env("LATTICE_FRONTEND_ADDR", ":8080")
	fmt.Printf("Lattice Frontend listening on %s...\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
