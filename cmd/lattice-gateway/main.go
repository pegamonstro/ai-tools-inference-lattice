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
	"sync"
)

// --- Protocol Types ---

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

type Response struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	Created  int64  `json:"created"`
	Model    string `json:"model"`
	Choices  []Choice `json:"choices"`
	Usage    Usage    `json:"usage"`
}

type Choice struct {
	Index    int    `json:"index"`
	Message  Message `json:"message"`
	FinishReason string `json:"finish_reason"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// --- Provider Abstraction ---

type Provider interface {
	Execute(req Request) (*Response, error)
	Name() string
}

type OllamaProvider struct {
	Endpoint string
}

func (p *OllamaProvider) Name() string {
	return "ollama"
}

func (p *OllamaProvider) Execute(req Request) (*Response, error) {
	targetURL, _ := url.Parse(p.Endpoint + "/v1/chat/completions")

	// Prepare Ollama request
	ollamaReq := map[string]interface{}{
		"model":    req.Model,
		"messages": req.Messages,
		"stream":   false,
	}
	body, _ := json.Marshal(ollamaReq)

	httpReq, _ := http.NewRequest("POST", targetURL.String(), bytes.NewBuffer(body))
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var res Response
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return &res, nil
}

// --- Gateway Logic ---

var (
	localSemaphore = make(chan struct{}, 2)
	providers      = make(map[string]Provider)
	providerMutex  sync.RWMutex
)

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

	fmt.Printf("Gateway executing request [%s] for model %s\n", req.Routing.RequestID, req.Model)

	select {
	case localSemaphore <- struct{}{}:
		defer func() { <-localSemaphore }()
	default:
		fmt.Println("  Memory pressure: localSema full. Rejecting to avoid swap.")
		http.Error(w, "Local memory pressure: request rejected to avoid swap", http.StatusTooManyRequests)
		return
	}

	providerMutex.RLock()
	provider, ok := providers["ollama"] // Default to ollama for now
	providerMutex.RUnlock()

	if !ok {
		http.Error(w, "No suitable provider found", http.StatusInternalServerError)
		return
	}

	res, err := provider.Execute(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(res)
}

func main() {
	// Register providers
	providerMutex.Lock()
	providers["ollama"] = &OllamaProvider{Endpoint: "http://localhost:11434"}
	providerMutex.Unlock()

	http.HandleFunc("/v1/chat/completions", handleInference)
	fmt.Println("Lattice Gateway listening on :8081 (with Provider Abstraction)...")
	log.Fatal(http.ListenAndServe(":8081", nil))
}
