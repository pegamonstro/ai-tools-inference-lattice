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
	pageSize    uint64
}

func NewMemoryBudgeter() *MemoryBudgeter {
	// Get total RAM via sysctl
	out, _ := exec.Command("sysctl", "-n", "hw.memsize").Output()
	mem, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)

	// Get the VM page size (4096 on Intel, 16384 on Apple Silicon) so the
	// vm_stat page counts below are converted to bytes correctly.
	pageOut, _ := exec.Command("sysctl", "-n", "vm.pagesize").Output()
	pageSize, _ := strconv.ParseUint(strings.TrimSpace(string(pageOut)), 10, 64)
	if pageSize == 0 {
		pageSize = 4096
	}

	// Safety margin (bytes) kept free to avoid swap. Configurable in MiB via
	// LATTICE_GATEWAY_MEMORY_MARGIN_MB; defaults to 1536 (1.5 GiB).
	marginMB := 1536
	if v := latticeconfig.Env("LATTICE_GATEWAY_MEMORY_MARGIN_MB", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			marginMB = n
		}
	}

	return &MemoryBudgeter{
		maxRAMBytes: mem,
		safeMargin:  uint64(marginMB) * 1024 * 1024,
		pageSize:    pageSize,
	}
}

func (mb *MemoryBudgeter) CanAccommodate() bool {
	// Get current free memory via vm_stat
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return false
	}

	// vm_stat outputs page counts; multiply by the actual page size.
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

	availableBytes := (freePages + inactivePages) * mb.pageSize
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
	// Native /api/chat endpoint, not /v1/chat/completions: Ollama's OpenAI
	// compat layer silently drops the "options" object, so num_ctx/kv_cache_type
	// would never take effect there. The native endpoint honors options and
	// returns the same message content, which we map back to OpenAI shape below.
	targetURL, _ := url.Parse(p.Endpoint + "/api/chat")

	// max_budget caps output length (default 4096).
	maxTokens := 4096
	if mb, ok := req.Routing.ProviderParams["max_budget"].(float64); ok {
		maxTokens = int(mb)
	}

	// Context window is sized dynamically to the prompt: short prompts keep a
	// small KV cache (low memory), while large agent prompts grow it. reasoning_effort
	// raises the ceiling, and a configurable cap prevents the 131072-token default
	// from bloating memory.
	ollamaReq := map[string]interface{}{
		"model":    req.Model,
		"messages": req.Messages,
		"stream":   false,
		"options": map[string]interface{}{
			"num_ctx":       contextWindow(req.Messages, maxTokens, req.Routing.ProviderParams),
			"num_predict":   maxTokens,
			"kv_cache_type": kvCacheType,
		},
	}

	body, _ := json.Marshal(ollamaReq)

	httpReq, _ := http.NewRequest("POST", targetURL.String(), bytes.NewBuffer(body))
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var native struct {
		Model   string `json:"model"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&native); err != nil {
		return nil, err
	}

	return &Response{
		ID:      "chatcmpl-" + req.Routing.RequestID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   native.Model,
		Choices: []Choice{{
			Index:        0,
			Message:      Message{Role: native.Message.Role, Content: native.Message.Content},
			FinishReason: "stop",
		}},
		Usage: Usage{
			PromptTokens:     native.PromptEvalCount,
			CompletionTokens: native.EvalCount,
			TotalTokens:      native.PromptEvalCount + native.EvalCount,
		},
	}, nil
}

// maxContext is the hard ceiling on num_ctx, configurable via
// LATTICE_GATEWAY_MAX_CONTEXT. It exists so agents that legitimately need a
// large window can raise it, while the default (32768) still caps the KV cache
// well below Ollama's 131072-token default.
var maxContext = maxContextTokens()

func maxContextTokens() int {
	if v := latticeconfig.Env("LATTICE_GATEWAY_MAX_CONTEXT", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 32768
}

// kvCacheType quantizes Ollama's KV cache (q8_0 vs the f16 default), roughly
// halving context-window memory for a small recall-quality tradeoff.
var kvCacheType = kvCacheTypeFromEnv()

func kvCacheTypeFromEnv() string {
	if v := latticeconfig.Env("LATTICE_GATEWAY_KV_CACHE", ""); v != "" {
		return v
	}
	return "q8_0"
}

// contextWindow sizes the Ollama context to the prompt: it starts at 2048 and
// doubles until it fits the prompt + output + margin. reasoning_effort sets the
// ceiling (low=4096, default=8192, high=maxContext), so "high" lets an agent
// reason over more tokens without forcing every request to pay for them.
func contextWindow(messages []interface{}, maxTokens int, providerParams map[string]interface{}) int {
	// Default ceiling is the configured max (32768): large agent prompts must be
	// allowed to grow, otherwise Ollama truncates them. reasoning_effort=low is
	// the only knob that deliberately restricts it (for memory-sensitive calls).
	ceiling := maxContext
	if re, ok := providerParams["reasoning_effort"].(string); ok {
		switch re {
		case "low":
			ceiling = 4096
		case "high":
			ceiling = maxContext
		}
	}

	needed := estimateTokens(messages) + maxTokens + 256 // +margin for framing/overhead

	ctx := 2048
	for ctx < needed && ctx < ceiling {
		ctx *= 2
	}
	if ctx > ceiling {
		ctx = ceiling
	}
	if ctx > maxContext {
		ctx = maxContext
	}
	return ctx
}

// estimateTokens gives a rough prompt size in tokens. Ollama exposes no stable
// standalone tokenizer in its OpenAI-compat API, so we use ~4 chars/token — close
// enough for sizing a context bucket, not for exact accounting.
func estimateTokens(messages []interface{}) int {
	total := 0
	for _, m := range messages {
		if mm, ok := m.(map[string]interface{}); ok {
			if c, ok := mm["content"].(string); ok {
				total += (len(c) + 3) / 4
			}
		}
	}
	return total
}

// --- Gateway Logic ---

var (
	providers     = make(map[string]Provider)
	providerMutex sync.RWMutex
	budgeter      *MemoryBudgeter
	ollamaURL     string

	// inferenceSlots serializes local inference so only one model is resident
	// at a time; the memory budgeter can't stop two models loading concurrently.
	inferenceSlots = make(chan struct{}, 1)
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

	inferenceSlots <- struct{}{}
	res, err := provider.Execute(req)
	<-inferenceSlots

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

	resp, err := http.Get(ollamaURL + "/api/tags")
	if err != nil || resp.StatusCode != http.StatusOK {
		http.Error(w, "Ollama unhealthy", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func main() {
	budgeter = NewMemoryBudgeter()
	ollamaURL = latticeconfig.Env("LATTICE_OLLAMA_URL", "http://localhost:11434")

	providerMutex.Lock()
	providers["ollama"] = &OllamaProvider{Endpoint: ollamaURL}
	providerMutex.Unlock()

	http.HandleFunc("/v1/chat/completions", handleInference)
	http.HandleFunc("/health", handleHealth)
	addr := latticeconfig.Env("LATTICE_GATEWAY_ADDR", ":8081")
	fmt.Printf("Lattice Gateway listening on %s (Dynamic Memory Budgeting active)\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
