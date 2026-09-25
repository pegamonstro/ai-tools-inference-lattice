package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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
	Routing  Routing       `json:"routing"`
}

type Response struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
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

// --- Dynamic Memory Budgeter ---

type MemoryBudgeter struct {
	maxRAMBytes uint64
	safeMargin  uint64 // bytes to keep free to avoid swap
}

func NewMemoryBudgeter() *MemoryBudgeter {
	// Get total RAM via sysctl
	out, _ := exec.Command("sysctl", "-n", "hw.memsize").Output()
	mem, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)

	return &MemoryBudgeter{
		maxRAMBytes: mem,
		safeMargin:  4 * 1024 * 1024 * 1024, // Keep 4GB free
	}
}

func (mb *MemoryBudgeter) CanAccommodate() bool {
	// Get current free memory via vm_stat
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return false
	}

	// vm_stat outputs pages. Page size is usually 4096 bytes on Mac.
	// We look for "Pages free" and "Pages inactive"
	lines := strings.Split(string(out), "\n")
	var freePages, inactivePages uint64

	for _, line := range lines {
		if strings.Contains(line, "Pages free") {
			fmt.Sscanf(line, "Pages free: %d", &freePages)
		} else if strings.Contains(line, "Pages inactive") {
			fmt.Sscanf(line, "Pages inactive: %d", &inactivePages)
		}
	}

	availableBytes := (freePages + inactivePages) * 4096
	return availableBytes > mb.safeMargin
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

	ollamaReq := map[string]interface{}{
		"model":    req.Model,
		"messages": req.Messages,
		"stream":   false,
	}

	// Translate inference.v1 provider_params into Ollama-native options.
	// reasoning_effort maps to a context-window size (larger context lets the
	// model reason over more tokens); max_budget caps output length.
	options := map[string]interface{}{}
	if re, ok := req.Routing.ProviderParams["reasoning_effort"].(string); ok {
		switch re {
		case "low":
			options["num_ctx"] = 2048
		case "high":
			options["num_ctx"] = 32768
		default:
			options["num_ctx"] = 8192
		}
	}
	if mb, ok := req.Routing.ProviderParams["max_budget"].(float64); ok {
		ollamaReq["max_tokens"] = int(mb)
	}
	if len(options) > 0 {
		ollamaReq["options"] = options
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
	providers     = make(map[string]Provider)
	providerMutex sync.RWMutex
	budgeter      *MemoryBudgeter
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

	// Dynamic Memory Check
	if !budgeter.CanAccommodate() {
		fmt.Println("  Memory pressure: available RAM below safety margin. Rejecting to avoid swap.")
		http.Error(w, "Local memory pressure: available RAM below safety margin", http.StatusTooManyRequests)
		return
	}

	providerMutex.RLock()
	provider, ok := providers["ollama"]
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

func handleHealth(w http.ResponseWriter, r *http.Request) {
	// Health check: is Ollama responsive AND is memory okay?
	if !budgeter.CanAccommodate() {
		http.Error(w, "Memory pressure high", http.StatusServiceUnavailable)
		return
	}

	resp, err := http.Get("http://localhost:11434/api/tags")
	if err != nil || resp.StatusCode != http.StatusOK {
		http.Error(w, "Ollama unhealthy", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func main() {
	budgeter = NewMemoryBudgeter()

	providerMutex.Lock()
	providers["ollama"] = &OllamaProvider{Endpoint: "http://localhost:11434"}
	providerMutex.Unlock()

	http.HandleFunc("/v1/chat/completions", handleInference)
	http.HandleFunc("/health", handleHealth)
	fmt.Printf("Lattice Gateway listening on :8081 (Dynamic Memory Budgeting active)\n")
	log.Fatal(http.ListenAndServe(":8081", nil))
}
