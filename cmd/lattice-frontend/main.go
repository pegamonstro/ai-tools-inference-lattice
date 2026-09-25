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
	"time"
)

type Routing struct {
	Privacy      string `json:"privacy"`
	LatencyClass string `json:"latency_class"`
	Parallelism  int    `json:"parallelism"`
	RequestID    string `json:"request_id"`
}

type Request struct {
	Model    string        `json:"model"`
	Messages []interface{} `json:"messages"`
	Routing  Routing       `json:"routing"`
}

type Decision struct {
	Target    string `json:"target"`
	Endpoint  string `json:"endpoint"`
	ModelName string `json:"model_name"`
}

type Telemetry struct {
	RequestID     string  `json:"request_id"`
	TotalTime     float64 `json:"total_time_s"`
	ExecutionTime float64 `json:"execution_time_s"`
	Target        string  `json:"target"`
}

var (
	controlURL = "http://127.0.0.1:8082/route"
)

func logTelemetry(t Telemetry) {
	f, err := os.OpenFile("telemetry-frontend.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Telemetry error: %v\n", err)
		return
	}
	defer f.Close()
	b, _ := json.Marshal(t)
	f.Write(append(b, '\n'))
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	tTotalStart := time.Now()
	bodyBytes, _ := io.ReadAll(r.Body)
	var req Request
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 1. Ask Control Plane for Decision
	decisionReq, _ := json.Marshal(req)

	// HARDENING: Add timeout to Control Plane call
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(controlURL, "application/json", bytes.NewBuffer(decisionReq))
	if err != nil {
		http.Error(w, "Control plane unavailable or timed out", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	var decision Decision
	if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
		http.Error(w, "Invalid decision from control plane", http.StatusInternalServerError)
		return
	}

	fmt.Printf("Routed [%s] to %s (%s)\n", req.Routing.RequestID, decision.Target, decision.Endpoint)

	// 2. Rewrite request for Target
	proxyBody := make(map[string]interface{})
	proxyBody["model"] = decision.ModelName
	proxyBody["messages"] = req.Messages
	finalBodyBytes, _ := json.Marshal(proxyBody)

	// 3. Proxy to Target
	tExecStart := time.Now()
	targetURL, _ := url.Parse(decision.Endpoint)
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	r.URL.Path = "/v1/chat/completions"
	r.Body = io.NopCloser(bytes.NewBuffer(finalBodyBytes))
	r.ContentLength = int64(len(finalBodyBytes))

	proxy.ServeHTTP(w, r)

	executionTime := time.Since(tExecStart).Seconds()
	totalTime := time.Since(tTotalStart).Seconds()

	logTelemetry(Telemetry{
		RequestID:     req.Routing.RequestID,
		TotalTime:     totalTime,
		ExecutionTime: executionTime,
		Target:        decision.Target,
	})
}

func main() {
	http.HandleFunc("/v1/chat/completions", handleChat)
	fmt.Println("Lattice Frontend listening on :8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
