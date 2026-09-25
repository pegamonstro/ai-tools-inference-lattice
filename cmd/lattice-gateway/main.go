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
	localSemaphore = make(chan struct{}, 2)
	ollamaURL      = "http://localhost:11434"
)

func proxyToOllama(targetURL string, w http.ResponseWriter, r *http.Request) {
	remote, _ := url.Parse(targetURL)
	proxy := httputil.NewSingleHostReverseProxy(remote)
	proxy.ServeHTTP(w, r)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	// Simple health check: is Ollama responsive?
	resp, err := http.Get(ollamaURL + "/api/tags")
	if err != nil || resp.StatusCode != http.StatusOK {
		http.Error(w, "Ollama unhealthy", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func handleInference(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var req map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	routing, ok := req["routing"].(map[string]interface{})
	if !ok {
		http.Error(w, "missing routing metadata", http.StatusBadRequest)
		return
	}

	fmt.Printf("Gateway receiving request [%v] for model %v\n", routing["request_id"], req["model"])

	select {
	case localSemaphore <- struct{}{}:
		defer func() { <-localSemaphore }()
	default:
		fmt.Println("  Memory pressure: localSema full. Rejecting to avoid swap.")
		http.Error(w, "Local memory pressure: request rejected to avoid swap", http.StatusTooManyRequests)
		return
	}

	delete(req, "routing")
	newBodyBytes, _ := json.Marshal(req)
	fmt.Printf("Proxying Body: %s\n", string(newBodyBytes))
	r.Body = io.NopCloser(bytes.NewBuffer(newBodyBytes))
	r.ContentLength = int64(len(newBodyBytes))

	proxyToOllama(ollamaURL, w, r)
}

func main() {
	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/v1/chat/completions", handleInference)
	fmt.Println("Lattice Gateway listening on :8081...")
	log.Fatal(http.ListenAndServe(":8081", nil))
}
