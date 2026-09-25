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

var (
	controlURL = "http://localhost:8082/route"
)

func handleChat(w http.ResponseWriter, r *http.Request) {
	bodyBytes, _ := io.ReadAll(r.Body)
	var req Request
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 1. Ask Control Plane for Decision
	decisionReq, _ := json.Marshal(req)
	resp, err := http.Post(controlURL, "application/json", bytes.NewBuffer(decisionReq))
	if err != nil {
		http.Error(w, "Control plane unavailable", http.StatusServiceUnavailable)
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
	req.Model = decision.ModelName
	newBody, _ := json.Marshal(req)

	// 3. Proxy to Target
	targetURL, _ := url.Parse(decision.Endpoint)
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// Override the request to hit /v1/chat/completions on the target
	r.URL.Path = "/v1/chat/completions"
	r.Body = io.NopCloser(bytes.NewBuffer(newBody))
	r.ContentLength = int64(len(newBody))

	proxy.ServeHTTP(w, r)
}

func main() {
	http.HandleFunc("/v1/chat/completions", handleChat)
	fmt.Println("Lattice Frontend listening on :8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
