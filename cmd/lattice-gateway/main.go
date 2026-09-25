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

var (
	// Memory-Aware Concurrency Gate:
	// Conserve RAM to avoid swap thrashing.
	localSemaphore = make(chan struct{}, 2)
	ollamaURL      = "http://localhost:11434"
)

func proxyToOllama(targetURL string, w http.ResponseWriter, r *http.Request) {
	remote, _ := url.Parse(targetURL)
	proxy := httputil.NewSingleHostReverseProxy(remote)
	proxy.ServeHTTP(w, r)
}

func handleInference(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var req Request
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fmt.Printf("Gateway receiving request [%s] for model %s\n", req.Routing.RequestID, req.Model)

	select {
	case localSemaphore <- struct{}{}:
		defer func() { <-localSemaphore }()
	default:
		fmt.Println("  Memory pressure: localSema full. Rejecting to avoid swap.")
		http.Error(w, "Local memory pressure: request rejected to avoid swap", http.StatusTooManyRequests)
		return
	}

	// Reset body for the proxy
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	proxyToOllama(ollamaURL, w, r)
}

func main() {
	http.HandleFunc("/v1/chat/completions", handleInference)
	fmt.Println("Lattice Gateway listening on :8081...")
	log.Fatal(http.ListenAndServe(":8081", nil))
}
