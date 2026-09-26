package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
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
	Model      string        `json:"model"`
	Messages   []interface{} `json:"messages"`
	Stream     bool          `json:"stream"`
	Tools      []interface{} `json:"tools,omitempty"`
	ToolChoice interface{}   `json:"tool_choice,omitempty"`
	Routing    Routing       `json:"routing"`
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

// ToolFunction is named rather than inlined so the response-building code below
// can construct one without restating its tags, which are part of the type.
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall is the OpenAI shape. Ollama returns the same information with
// arguments as a JSON object, which is translated in Execute.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Telemetry is one JSONL line per inference request, written to disk for the
// Bee-terminal feeder to tail and relay to the log screen.
type Telemetry struct {
	RequestID        string  `json:"request_id"`
	Model            string  `json:"model"`
	ContextWindow    int     `json:"context_window"`
	Elapsed          float64 `json:"elapsed_s"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	Error            string  `json:"error,omitempty"`
}

// TelemetryEvent tags a buffered event with a monotonic sequence so a remote
// consumer can pull only what it has not seen yet, and with the identity of the
// process that issued that sequence.
//
// seq alone is ambiguous across a restart: this counter is in memory, so it
// begins again at 1, and the new incarnation's "seq 1" is indistinguishable
// from the one a consumer may already have received from the previous
// incarnation. Boot is what makes the difference visible, and it rides on each
// event so a consumer can persist it alongside the cursor.
type TelemetryEvent struct {
	Seq  int64  `json:"seq"`
	Boot string `json:"boot"`
	Telemetry
}

// The gateway runs on the Mac, which shares no filesystem with the RPi4 control
// plane, so recent events are also buffered in memory and served over HTTP at
// /telemetry. The control plane pulls them into the local telemetry stream that
// the Bee feeder tails.
const telemetryRingCap = 256

var (
	telemetryRing  []TelemetryEvent
	telemetrySeq   int64
	telemetryMutex sync.Mutex

	// telemetryBoot identifies this process's incarnation of the stream. It is
	// assigned once at startup, so it changes on every restart — which is
	// exactly when telemetrySeq starts over.
	telemetryBoot = strconv.FormatInt(time.Now().UnixNano(), 36)
)

func logTelemetry(t Telemetry) {
	telemetryMutex.Lock()
	telemetrySeq++
	telemetryRing = append(telemetryRing, TelemetryEvent{Seq: telemetrySeq, Boot: telemetryBoot, Telemetry: t})
	if len(telemetryRing) > telemetryRingCap {
		telemetryRing = telemetryRing[len(telemetryRing)-telemetryRingCap:]
	}
	telemetryMutex.Unlock()

	path := latticeconfig.Env("LATTICE_GATEWAY_TELEMETRY", "telemetry-gateway.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Telemetry error: %v\n", err)
		return
	}
	defer f.Close()
	b, _ := json.Marshal(t)
	f.Write(append(b, '\n'))
}

// handleTelemetry serves buffered events with seq greater than ?since, so a
// polling consumer advances a cursor rather than re-reading the whole buffer.
//
// It also reports oldest_seq, the earliest event the ring still holds. Without
// it the consumer cannot distinguish "nothing new" from "there is a hole": a
// cursor left behind by an eviction, or by a restart of this process resetting
// the in-memory seq, would look identical to a quiet stream and the events in
// between would vanish with no trace.
func handleTelemetry(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)

	telemetryMutex.Lock()
	seq := telemetrySeq
	oldest := int64(0)
	if len(telemetryRing) > 0 {
		oldest = telemetryRing[0].Seq
	}
	events := make([]TelemetryEvent, 0, len(telemetryRing))
	for _, e := range telemetryRing {
		if e.Seq > since {
			events = append(events, e)
		}
	}
	telemetryMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"seq":        seq,
		"oldest_seq": oldest,
		"boot":       telemetryBoot,
		"events":     events,
	})
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

// StreamingProvider is implemented by providers that can emit an OpenAI-compatible
// SSE stream. handleInference type-asserts to it only when the client set stream:
// true, so providers without streaming keep working unary.
type StreamingProvider interface {
	ExecuteStream(req Request, w http.ResponseWriter) (res *Response, committed bool, err error)
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

	maxTokens := resolveMaxTokens(req)

	// Context window is sized dynamically to the prompt: short prompts keep a
	// small KV cache (low memory), while large agent prompts grow it. reasoning_effort
	// raises the ceiling, and a configurable cap prevents the 131072-token default
	// from bloating memory.
	ollamaReq := map[string]interface{}{
		"model":    req.Model,
		"messages": req.Messages,
		"tools":    req.Tools,
		"stream":   false,
		"options": map[string]interface{}{
			"num_ctx":       contextWindow(req.Messages, maxTokens, req.Routing.ProviderParams),
			"num_predict":   maxTokens,
			"kv_cache_type": kvCacheType,
		},
	}

	if len(req.Tools) == 0 {
		// Ollama rejects tools: null; the key must simply be absent.
		delete(ollamaReq, "tools")
	}
	if req.ToolChoice != nil {
		ollamaReq["tool_choice"] = req.ToolChoice
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
			// Ollama's arguments are an object; OpenAI's are a string. Decoding
			// as RawMessage and re-encoding below performs that translation
			// without guessing at the inner shape.
			ToolCalls []struct {
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&native); err != nil {
		return nil, err
	}

	toolCalls := make([]ToolCall, 0, len(native.Message.ToolCalls))
	for i, tc := range native.Message.ToolCalls {
		args := string(tc.Function.Arguments)
		if args == "" || args == "null" {
			args = "{}"
		}
		toolCalls = append(toolCalls, ToolCall{
			// Ollama issues no call id. OpenAI clients key the tool result on
			// one, so it is synthesized deterministically per position.
			ID:       fmt.Sprintf("call_%d", i),
			Type:     "function",
			Function: ToolFunction{Name: tc.Function.Name, Arguments: args},
		})
	}

	finish := "stop"
	if len(toolCalls) > 0 {
		// The client's tool loop branches on this; "stop" with tool_calls
		// present makes an agent end its turn instead of calling the tool.
		finish = "tool_calls"
	}

	return &Response{
		ID:      "chatcmpl-" + req.Routing.RequestID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   native.Model,
		Choices: []Choice{{
			Index:        0,
			Message:      Message{Role: native.Message.Role, Content: native.Message.Content, ToolCalls: toolCalls},
			FinishReason: finish,
		}},
		Usage: Usage{
			PromptTokens:     native.PromptEvalCount,
			CompletionTokens: native.EvalCount,
			TotalTokens:      native.PromptEvalCount + native.EvalCount,
		},
	}, nil
}

// ExecuteStream runs the same request as Execute but streams the result to w as
// OpenAI-compatible SSE, translating Ollama's newline-delimited JSON events into
// chat.completion.chunk frames. It returns the assembled Response so the caller
// can log telemetry identically to the unary path.
//
// committed reports whether the SSE status and headers were already sent. Before
// that point the caller can still surface a failure as an HTTP error; after it the
// response is a 200 and the only thing left is to stop writing.
func (p *OllamaProvider) ExecuteStream(req Request, w http.ResponseWriter) (*Response, bool, error) {
	targetURL, _ := url.Parse(p.Endpoint + "/api/chat")
	maxTokens := resolveMaxTokens(req)

	ollamaReq := map[string]interface{}{
		"model":    req.Model,
		"messages": req.Messages,
		"tools":    req.Tools,
		"stream":   true,
		"options": map[string]interface{}{
			"num_ctx":       contextWindow(req.Messages, maxTokens, req.Routing.ProviderParams),
			"num_predict":   maxTokens,
			"kv_cache_type": kvCacheType,
		},
	}
	if len(req.Tools) == 0 {
		// Ollama rejects tools: null; the key must simply be absent.
		delete(ollamaReq, "tools")
	}
	if req.ToolChoice != nil {
		ollamaReq["tool_choice"] = req.ToolChoice
	}
	body, _ := json.Marshal(ollamaReq)

	httpReq, _ := http.NewRequest("POST", targetURL.String(), bytes.NewBuffer(body))
	httpReq.Header.Set("Content-Type", "application/json")

	// Unlike the unary path this client has no fixed timeout: a streamed answer can
	// legitimately run long, and the caller's request context governs cancellation.
	resp, err := (&http.Client{}).Do(httpReq)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	// Ollama accepted the request, so from here the reply is committed to SSE and no
	// longer expressible as an HTTP error status.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	id := "chatcmpl-" + req.Routing.RequestID
	created := time.Now().Unix()
	model := req.Model
	promptTokens, completionTokens := 0, 0
	var content strings.Builder

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Model   string `json:"model"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			PromptEvalCount int  `json:"prompt_eval_count"`
			EvalCount       int  `json:"eval_count"`
			Done            bool `json:"done"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Model != "" {
			model = ev.Model
		}
		if ev.Done {
			// Token counts only arrive with the terminal event in streaming mode.
			promptTokens, completionTokens = ev.PromptEvalCount, ev.EvalCount
			break
		}
		if ev.Message.Content == "" {
			continue
		}
		content.WriteString(ev.Message.Content)
		writeSSEChunk(w, id, created, model, ev.Message.Content, "")
		if flusher != nil {
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, true, err
	}

	// Terminate the stream cleanly even if Ollama's done event never arrived, so a
	// streaming client always sees a finish_reason and [DONE] rather than a hang.
	writeSSEChunk(w, id, created, model, "", "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}

	return &Response{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []Choice{{
			Index:        0,
			Message:      Message{Role: "assistant", Content: content.String()},
			FinishReason: "stop",
		}},
		Usage: Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}, true, nil
}

// writeSSEChunk emits one chat.completion.chunk event. finish is empty for a
// content delta and "stop" for the terminal frame, which carries an empty delta.
func writeSSEChunk(w io.Writer, id string, created int64, model, content, finish string) {
	delta := map[string]string{"content": content}
	var finishReason interface{}
	if finish != "" {
		delta = map[string]string{}
		finishReason = finish
	}
	chunk := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
	b, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

// maxContext is the hard ceiling on num_ctx, configurable via
// LATTICE_GATEWAY_MAX_CONTEXT. The default is 65536 to match what agent
// runtimes carry by default, but a larger ceiling is a larger KV cache, so
// this value is only trusted once a swap measurement says it is affordable.
var maxContext = maxContextTokens()

func maxContextTokens() int {
	if v := latticeconfig.Env("LATTICE_GATEWAY_MAX_CONTEXT", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	// 65536 matches what agent runtimes carry by default. It is a memory
	// decision as much as a formatting one: a larger ceiling is a larger KV
	// cache, so this value is only trusted once the swap measurement below
	// says it is affordable.
	return 65536
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

// resolveMaxTokens caps output length via the max_budget provider param
// (default 4096). Shared by Execute and the telemetry path so both agree.
func resolveMaxTokens(req Request) int {
	maxTokens := 4096
	if mb, ok := req.Routing.ProviderParams["max_budget"].(float64); ok {
		maxTokens = int(mb)
	}
	return maxTokens
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

	t0 := time.Now()
	maxTokens := resolveMaxTokens(req)
	ctxWindow := contextWindow(req.Messages, maxTokens, req.Routing.ProviderParams)

	fmt.Printf("Gateway executing request [%s] for model %s\n", req.Routing.RequestID, req.Model)

	// Dynamic Memory Check
	if !budgeter.CanAccommodate() {
		fmt.Println("  Memory pressure: available RAM below safety margin. Rejecting to avoid swap.")
		logTelemetry(Telemetry{
			RequestID:     req.Routing.RequestID,
			Model:         req.Model,
			ContextWindow: ctxWindow,
			Elapsed:       time.Since(t0).Seconds(),
			Error:         "memory_pressure",
		})
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

	// Stream when the client asked for it and the provider supports it: most real
	// chat clients stream by default and would otherwise render an empty reply.
	if req.Stream {
		if sp, ok := provider.(StreamingProvider); ok {
			inferenceSlots <- struct{}{}
			res, committed, err := sp.ExecuteStream(req, w)
			<-inferenceSlots

			te := Telemetry{
				RequestID:     req.Routing.RequestID,
				Model:         req.Model,
				ContextWindow: ctxWindow,
				Elapsed:       time.Since(t0).Seconds(),
			}
			if err != nil {
				te.Error = err.Error()
				logTelemetry(te)
				// Only a failure before the first byte can still become a status code.
				if !committed {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
				return
			}
			te.PromptTokens = res.Usage.PromptTokens
			te.CompletionTokens = res.Usage.CompletionTokens
			logTelemetry(te)
			return
		}
	}

	inferenceSlots <- struct{}{}
	res, err := provider.Execute(req)
	<-inferenceSlots

	if err != nil {
		logTelemetry(Telemetry{
			RequestID:     req.Routing.RequestID,
			Model:         req.Model,
			ContextWindow: ctxWindow,
			Elapsed:       time.Since(t0).Seconds(),
			Error:         err.Error(),
		})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	logTelemetry(Telemetry{
		RequestID:        req.Routing.RequestID,
		Model:            req.Model,
		ContextWindow:    ctxWindow,
		Elapsed:          time.Since(t0).Seconds(),
		PromptTokens:     res.Usage.PromptTokens,
		CompletionTokens: res.Usage.CompletionTokens,
	})

	// Set this explicitly: without it Go sniffs the JSON body as text/plain, which
	// strict OpenAI clients reject.
	w.Header().Set("Content-Type", "application/json")
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

	// The ceiling is reported rather than configured twice: the gateway is the
	// only process that knows what a context window costs in KV cache here, so
	// it is the authority on the number and the control plane relays it.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "ok",
		"max_context": maxContext,
	})
}

func main() {
	budgeter = NewMemoryBudgeter()
	ollamaURL = latticeconfig.Env("LATTICE_OLLAMA_URL", "http://localhost:11434")

	providerMutex.Lock()
	providers["ollama"] = &OllamaProvider{Endpoint: ollamaURL}
	providerMutex.Unlock()

	http.HandleFunc("/v1/chat/completions", handleInference)
	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/telemetry", handleTelemetry)
	addr := latticeconfig.Env("LATTICE_GATEWAY_ADDR", ":8081")
	fmt.Printf("Lattice Gateway listening on %s (Dynamic Memory Budgeting active)\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
