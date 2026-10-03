package main

import (
	"bufio"
	"bytes"
	"context"
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

// ImageRequest is the OpenAI Images API request. It is a closed shape on
// purpose: the mflux sidecar honours only prompt, size and count, so the
// request carries exactly those and the edit source image. An unsupported
// OpenAI field (quality, style) never reaches a type that would swallow it.
type ImageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
	// Image is the edit source, base64 (data URL or raw). The Mac cannot read
	// the Pi's filesystem, so the bytes must ride the request.
	Image string `json:"image"`
	// Mask is carried only so a request that sends one fails loudly instead of
	// being silently dropped: the mflux sidecar has no mask support.
	Mask string `json:"mask"`
}

type ImageResponse struct {
	Created int64           `json:"created"`
	Data    []ImageDataItem `json:"data"`
}

type ImageDataItem struct {
	B64JSON string `json:"b64_json"`
}

// TranscriptionRequest is the OpenAI /v1/audio/transcriptions request, adapted to
// the lattice's JSON proxy: the audio rides as base64 in `file` (the Mac cannot
// read the Pi's filesystem, so bytes must ride the request, exactly like images).
type TranscriptionRequest struct {
	Model    string `json:"model"`
	File     string `json:"file"`
	Language string `json:"language"`
}

type TranscriptionResponse struct {
	Text string `json:"text"`
}

// SynthesisRequest is the OpenAI /v1/audio/speech request. Input carries the text
// to speak; Voice selects the kokoro voice, defaulting in the sidecar. Format is
// carried but the sidecar returns WAV only.
type SynthesisRequest struct {
	Model  string `json:"model"`
	Input  string `json:"input"`
	Voice  string `json:"voice"`
	Format string `json:"response_format"`
}

type SynthesisResponse struct {
	Audio      string `json:"audio"`
	Format     string `json:"format"`
	SampleRate int    `json:"sample_rate"`
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
	// Locality is stamped in logTelemetry, not by callers: this process is the
	// local executor, so the value is constant, and setting it in one place makes
	// it true by construction at every call site.
	Locality string `json:"locality"`
	Error    string `json:"error,omitempty"`
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
	// Constant, and set here rather than by callers so no call site can forget
	// it. The gateway has no target field and does not gain one: its provenance
	// is this process, and locality is the one shared dimension the three streams
	// need.
	t.Locality = "local"

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

// freeBytes reads the host's free + inactive memory from vm_stat, converted by
// page size. Extracted from CanAccommodate so a caller can ask the same question
// against a different margin: the image model is far larger than a chat model,
// so its margin is a different number, not a different check.
func (mb *MemoryBudgeter) freeBytes() uint64 {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0
	}
	lines := strings.Split(string(out), "\n")
	var freePages, inactivePages uint64
	for _, line := range lines {
		if strings.Contains(line, "Pages free") {
			fmt.Sscanf(line, "Pages free: %d", &freePages)
		} else if strings.Contains(line, "Pages inactive") {
			fmt.Sscanf(line, "Pages inactive: %d", &inactivePages)
		}
	}
	return (freePages + inactivePages) * mb.pageSize
}

func (mb *MemoryBudgeter) CanAccommodate() bool {
	return mb.freeBytes() > mb.safeMargin
}

// CanAccommodateWith is CanAccommodate against a caller-supplied margin. The
// image handler uses it with the image margin, which is an order of magnitude
// larger than the chat margin because the diffusion model is ~9 GB resident.
func (mb *MemoryBudgeter) CanAccommodateWith(marginBytes uint64) bool {
	return mb.freeBytes() > marginBytes
}

// --- Provider Abstraction ---

type Provider interface {
	Execute(ctx context.Context, req Request) (*Response, error)
	Name() string
}

// StreamingProvider is implemented by providers that can emit an OpenAI-compatible
// SSE stream. handleInference type-asserts to it only when the client set stream:
// true, so providers without streaming keep working unary.
type StreamingProvider interface {
	ExecuteStream(ctx context.Context, req Request, w http.ResponseWriter) (res *Response, committed bool, err error)
}

type OllamaProvider struct {
	Endpoint string

	// transportOnce guards transport. Every call must go through the same
	// transport: a transport built per request cannot reuse connections, and
	// because a zero-value transport never expires its idle ones, each streamed
	// answer strands a connection that is never closed again.
	transportOnce sync.Once
	transport     *http.Transport
}

// httpTransport is the provider's one transport, built on first use so it picks
// up the configured timeout. Cloning http.DefaultTransport keeps its dial and
// TLS timeouts, its proxy handling and its cap on idle connections; only the
// header bound differs, because a streamed answer legitimately outlives any
// total request timeout.
func (p *OllamaProvider) httpTransport() *http.Transport {
	p.transportOnce.Do(func() {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = ollamaTimeout()
		p.transport = t
	})
	return p.transport
}

func (p *OllamaProvider) Name() string {
	return "ollama"
}

func (p *OllamaProvider) Execute(ctx context.Context, req Request) (*Response, error) {
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
			"num_ctx":       contextWindow(req.Model, req.Messages, maxTokens, req.Routing.ProviderParams),
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

	// Bound to the caller's context so a client that disconnects releases the
	// inference instead of leaving it to run to completion for nobody.
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", targetURL.String(), bytes.NewBuffer(body))
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Transport: p.httpTransport(), Timeout: ollamaTimeout()}
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
func (p *OllamaProvider) ExecuteStream(ctx context.Context, req Request, w http.ResponseWriter) (*Response, bool, error) {
	targetURL, _ := url.Parse(p.Endpoint + "/api/chat")
	maxTokens := resolveMaxTokens(req)

	ollamaReq := map[string]interface{}{
		"model":    req.Model,
		"messages": req.Messages,
		"tools":    req.Tools,
		"stream":   true,
		"options": map[string]interface{}{
			"num_ctx":       contextWindow(req.Model, req.Messages, maxTokens, req.Routing.ProviderParams),
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

	httpReq, _ := http.NewRequestWithContext(ctx, "POST", targetURL.String(), bytes.NewBuffer(body))
	httpReq.Header.Set("Content-Type", "application/json")

	// Bound time-to-first-byte, not the whole stream. A streamed answer may
	// legitimately run long, so there is no total timeout; but Ollama sends its
	// headers only once prefill finishes and the first token is ready, so a stalled
	// server is still caught here rather than pinning the inference slot forever.
	// The caller's context, attached above, governs cancellation after that.
	client := &http.Client{Transport: p.httpTransport()}
	resp, err := client.Do(httpReq)
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
// LATTICE_GATEWAY_MAX_CONTEXT. The default is 32768: the 65536 raise that
// matched what agent runtimes carry was measured and rejected — one large local
// inference inflated the Mac's resident set to 13 GB and wrote ~2.2 GB to swap,
// against 9.2 GB and ~40 MB at 32768.
var maxContext = maxContextTokens()

func maxContextTokens() int {
	if v := latticeconfig.Env("LATTICE_GATEWAY_MAX_CONTEXT", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	// 32768 is the measured ceiling. 65536 was implemented and measured and
	// drove the Mac into swap, so the smaller value is the affordable one.
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

// ollamaTimeout bounds a single call to Ollama, configurable via
// LATTICE_GATEWAY_OLLAMA_TIMEOUT (a Go duration, e.g. "30m").
//
// The default is deliberately generous rather than tight, because on this path the
// bound covers prompt prefill *plus* the whole decoded answer, and both are slow
// here: decoding runs at a measured ~6.7 tok/s, so the default request's 4096
// output tokens need ~10 minutes by themselves, and prefill adds roughly a minute
// per few thousand prompt tokens at the measured ~40 tok/s. A normal worst case —
// a few thousand prompt tokens plus the full default output — therefore lands near
// 14 minutes. 20m leaves that ~45% headroom; a tighter guard does not catch hangs,
// it turns ordinary requests into 500s, which is what five minutes did.
//
// A request that fills the context ceiling can exceed any sane bound; raise this
// rather than lowering it if such prompts are expected. A client that gives up
// releases the inference through its context, so a long bound no longer risks
// pinning the slot.
func ollamaTimeout() time.Duration {
	if v := latticeconfig.Env("LATTICE_GATEWAY_OLLAMA_TIMEOUT", ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 20 * time.Minute
}

// acquireSlot takes the single local-inference slot, reporting false if the
// caller gave up while queued. Blocking forever on the slot would let an
// abandoned request hold up every later one on a machine that can only run one
// model at a time.
func acquireSlot(ctx context.Context) bool {
	select {
	case inferenceSlots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
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
// doubles until it fits the prompt + output + margin, capped at the model's
// ceiling. The ceiling is per-model (modelContextCeiling): a linear-attention
// model may be granted 64K+ in config while dense locals stay at the global
// default. reasoning_effort=low lowers the cap to 4096; every other value,
// including the default, uses the full per-model ceiling.
func contextWindow(model string, messages []interface{}, maxTokens int, providerParams map[string]interface{}) int {
	ceiling := modelContextCeiling(model)
	if re, ok := providerParams["reasoning_effort"].(string); ok && re == "low" {
		ceiling = 4096
	}

	needed := estimateTokens(messages) + maxTokens + 256 // +margin for framing/overhead

	ctx := 2048
	for ctx < needed && ctx < ceiling {
		ctx *= 2
	}
	if ctx > ceiling {
		ctx = ceiling
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
	registry      *providerRegistry
	budgeter      *MemoryBudgeter
	ollamaURL     string

	// inferenceSlots serializes local inference so only one model is resident
	// at a time; the memory budgeter can't stop two models loading concurrently.
	inferenceSlots = make(chan struct{}, 1)
)

// logQueuedCancel records a request whose client went away while it waited for
// the inference slot. Nothing was sent to Ollama, so there is no status left to
// report, but the gap would otherwise be invisible in the telemetry stream.
func logQueuedCancel(req Request, ctxWindow int, t0 time.Time) {
	logTelemetry(Telemetry{
		RequestID:     req.Routing.RequestID,
		Model:         req.Model,
		ContextWindow: ctxWindow,
		Elapsed:       time.Since(t0).Seconds(),
		Error:         "client_cancelled_while_queued",
	})
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

	t0 := time.Now()
	maxTokens := resolveMaxTokens(req)
	ctxWindow := contextWindow(req.Model, req.Messages, maxTokens, req.Routing.ProviderParams)

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

	providerName, upstream := resolveModel(req.Model)
	providerMutex.RLock()
	provider, ok := providers[providerName]
	providerMutex.RUnlock()

	if !ok {
		// Fail loudly: a model mapped to a provider that is not registered is a
		// configuration fault, never silently re-homed to Ollama.
		http.Error(w, "No provider registered for model", http.StatusInternalServerError)
		return
	}

	// The provider may name the model differently upstream (an MLX Hugging Face
	// repo id vs the local alias). The alias stays on req for telemetry; only the
	// copy handed to the provider carries the upstream name.
	upstreamReq := req
	upstreamReq.Model = upstream

	// Stream when the client asked for it and the provider supports it: most real
	// chat clients stream by default and would otherwise render an empty reply.
	if req.Stream {
		if sp, ok := provider.(StreamingProvider); ok {
			if !acquireSlot(r.Context()) {
				logQueuedCancel(req, ctxWindow, t0)
				return
			}
			res, committed, err := sp.ExecuteStream(r.Context(), upstreamReq, w)
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

	if !acquireSlot(r.Context()) {
		logQueuedCancel(req, ctxWindow, t0)
		return
	}
	res, err := provider.Execute(r.Context(), upstreamReq)
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

// handleEmbeddings forwards an embedding request to Ollama's own OpenAI-compatible
// /v1/embeddings and writes one telemetry line.
//
// It does not reuse handleInference. That handler decodes the chat Request shape
// and applies the generation context machinery — contextWindow, num_ctx,
// resolveMaxTokens, kv_cache_type — none of which an embedding model has: it has a
// fixed small context and no output to predict. Ollama implements the endpoint
// natively, so nothing is translated and the response's shape, its base64
// encoding and its precision are Ollama's by construction.
//
// The request carries no routing envelope, so its id arrives as a header. A
// caller that reaches this port directly leaves the id empty, exactly as a direct
// caller to /v1/chat/completions leaves it empty today.
func handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	requestID := r.Header.Get("X-Request-Id")

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !budgeter.CanAccommodate() {
		fmt.Println("  Memory pressure: available RAM below safety margin. Rejecting embedding to avoid swap.")
		logTelemetry(Telemetry{
			RequestID: requestID,
			Elapsed:   time.Since(t0).Seconds(),
			Error:     "memory_pressure",
		})
		http.Error(w, "Local memory pressure: available RAM below safety margin", http.StatusTooManyRequests)
		return
	}

	// The same single slot chat takes, for the same reason: with one model
	// resident, a concurrent embed evicts the resident chat model — which is the
	// swap pressure this project has already paid for once.
	if !acquireSlot(r.Context()) {
		logTelemetry(Telemetry{
			RequestID: requestID,
			Elapsed:   time.Since(t0).Seconds(),
			Error:     "client_cancelled_while_queued",
		})
		return
	}
	defer func() { <-inferenceSlots }()

	upstream, err := http.NewRequestWithContext(r.Context(), "POST",
		ollamaURL+"/v1/embeddings", bytes.NewReader(bodyBytes))
	if err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: ollamaTimeout()}
	resp, err := client.Do(upstream)
	if err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()})
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	te := Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds()}
	// The upstream status is the client's to interpret: an empty encoding_format
	// or a token-array input is Ollama's rejection, and masking it as a gateway
	// fault would send the caller looking in the wrong place.
	if resp.StatusCode != http.StatusOK {
		te.Error = fmt.Sprintf("upstream status %d", resp.StatusCode)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	if _, copyErr := io.Copy(w, resp.Body); copyErr != nil && te.Error == "" {
		te.Error = copyErr.Error()
	}
	logTelemetry(te)
}

// defaultImageModel is the Lattice alias for the diffusion model. An image
// request that omits model gets this rather than failing on an empty name.
const defaultImageModel = "flux-dev"

// imageMarginBytes is the free-RAM floor below which an image request is
// refused. It is separate from the chat margin (LATTICE_GATEWAY_MEMORY_MARGIN_MB)
// because the diffusion model is ~9 GB resident — far larger than any chat
// model — so a chat-sized margin would admit an image load that thrashes swap.
func imageMarginBytes() uint64 {
	mb := 10240
	if v := latticeconfig.Env("LATTICE_GATEWAY_IMAGE_MARGIN_MB", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			mb = n
		}
	}
	return uint64(mb) * 1024 * 1024
}

func handleImageGenerations(w http.ResponseWriter, r *http.Request) {
	handleImage(w, r, "generate")
}

func handleImageEdits(w http.ResponseWriter, r *http.Request) {
	handleImage(w, r, "edit")
}

// handleImage mirrors handleEmbeddings: the same single slot, the same
// telemetry-on-every-exit discipline, but against the image memory margin and
// the mflux provider. The id rides the X-Request-Id header, exactly as an
// embedding's does — an image body carries no routing envelope.
func handleImage(w http.ResponseWriter, r *http.Request, op string) {
	t0 := time.Now()
	requestID := r.Header.Get("X-Request-Id")

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var img ImageRequest
	if err := json.Unmarshal(bodyBytes, &img); err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()})
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if img.Model == "" {
		img.Model = defaultImageModel
	}

	if !budgeter.CanAccommodateWith(imageMarginBytes()) {
		fmt.Println("  Memory pressure: available RAM below image margin. Rejecting to avoid swap.")
		logTelemetry(Telemetry{
			RequestID: requestID,
			Model:     img.Model,
			Elapsed:   time.Since(t0).Seconds(),
			Error:     "memory_pressure",
		})
		http.Error(w, "Local memory pressure: available RAM below image margin", http.StatusTooManyRequests)
		return
	}

	if !acquireSlot(r.Context()) {
		logTelemetry(Telemetry{
			RequestID: requestID,
			Model:     img.Model,
			Elapsed:   time.Since(t0).Seconds(),
			Error:     "client_cancelled_while_queued",
		})
		return
	}
	defer func() { <-inferenceSlots }()

	providerName, _ := resolveModel(img.Model)
	providerMutex.RLock()
	provider := providers[providerName]
	providerMutex.RUnlock()
	if provider == nil {
		logTelemetry(Telemetry{RequestID: requestID, Model: img.Model, Elapsed: time.Since(t0).Seconds(), Error: "no provider registered for model"})
		http.Error(w, "No provider registered for model", http.StatusInternalServerError)
		return
	}

	ip, ok := provider.(ImageProvider)
	if !ok {
		logTelemetry(Telemetry{RequestID: requestID, Model: img.Model, Elapsed: time.Since(t0).Seconds(), Error: fmt.Sprintf("provider %q is not an image provider", providerName)})
		http.Error(w, "Provider is not an image provider", http.StatusInternalServerError)
		return
	}

	var res *ImageResponse
	if op == "edit" {
		res, err = ip.EditImage(r.Context(), img)
	} else {
		res, err = ip.GenerateImage(r.Context(), img)
	}

	te := Telemetry{RequestID: requestID, Model: img.Model, Elapsed: time.Since(t0).Seconds()}
	if err != nil {
		te.Error = err.Error()
		logTelemetry(te)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	logTelemetry(te)

	// Set this explicitly: without it Go sniffs the JSON body as text/plain,
	// which strict OpenAI clients reject.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// defaultTranscriptionModel and defaultSynthesisModel are the Lattice aliases for
// the speech models. A speech request that omits model gets these rather than
// failing on an empty name, mirroring defaultImageModel.
const (
	defaultTranscriptionModel = "whisper-small"
	defaultSynthesisModel     = "kokoro-82m"
)

func handleAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	handleSpeech(w, r, "transcribe")
}

func handleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	handleSpeech(w, r, "synthesize")
}

// handleSpeech mirrors handleEmbeddings/handleImage: the same single slot, the
// same telemetry-on-every-exit discipline, but against the speech provider. The
// id rides the X-Request-Id header, exactly as an embedding's does — a speech
// body carries no routing envelope. Speech models are small, so they use the chat
// memory margin rather than the image margin.
func handleSpeech(w http.ResponseWriter, r *http.Request, op string) {
	t0 := time.Now()
	requestID := r.Header.Get("X-Request-Id")

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !budgeter.CanAccommodate() {
		fmt.Println("  Memory pressure: available RAM below safety margin. Rejecting speech request.")
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: "memory_pressure"})
		http.Error(w, "Local memory pressure: available RAM below safety margin", http.StatusTooManyRequests)
		return
	}

	if !acquireSlot(r.Context()) {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: "client_cancelled_while_queued"})
		return
	}
	defer func() { <-inferenceSlots }()

	// The two ops decode different request shapes but share the resolve →
	// type-assert → call skeleton. A model routed to a non-speech provider is a
	// config fault and fails loudly, never re-homed to Ollama.
	if op == "synthesize" {
		var req SynthesisRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()})
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Model == "" {
			req.Model = defaultSynthesisModel
		}
		sp, providerName, ok := speechCapability(req.Model)
		if !ok {
			logTelemetry(Telemetry{RequestID: requestID, Model: req.Model, Elapsed: time.Since(t0).Seconds(), Error: fmt.Sprintf("provider %q is not a speech provider", providerName)})
			http.Error(w, "Provider is not a speech provider", http.StatusInternalServerError)
			return
		}
		res, err := sp.Synthesize(r.Context(), req)
		te := Telemetry{RequestID: requestID, Model: req.Model, Elapsed: time.Since(t0).Seconds()}
		if err != nil {
			te.Error = err.Error()
			logTelemetry(te)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		logTelemetry(te)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
		return
	}

	var req TranscriptionRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()})
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = defaultTranscriptionModel
	}
	sp, providerName, ok := speechCapability(req.Model)
	if !ok {
		logTelemetry(Telemetry{RequestID: requestID, Model: req.Model, Elapsed: time.Since(t0).Seconds(), Error: fmt.Sprintf("provider %q is not a speech provider", providerName)})
		http.Error(w, "Provider is not a speech provider", http.StatusInternalServerError)
		return
	}
	res, err := sp.Transcribe(r.Context(), req)
	te := Telemetry{RequestID: requestID, Model: req.Model, Elapsed: time.Since(t0).Seconds()}
	if err != nil {
		te.Error = err.Error()
		logTelemetry(te)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	logTelemetry(te)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// speechCapability resolves a speech model name to its provider and asserts it
// implements the SpeechCapability interface. ok is false when the resolved
// provider is not a speech provider.
func speechCapability(model string) (SpeechCapability, string, bool) {
	providerName, _ := resolveModel(model)
	providerMutex.RLock()
	provider := providers[providerName]
	providerMutex.RUnlock()
	sp, ok := provider.(SpeechCapability)
	return sp, providerName, ok
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	if !budgeter.CanAccommodate() {
		http.Error(w, "Memory pressure high", http.StatusServiceUnavailable)
		return
	}

	resp, err := http.Get(ollamaURL + "/api/tags")
	if err != nil {
		http.Error(w, "Ollama unhealthy", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "Ollama unhealthy", http.StatusServiceUnavailable)
		return
	}

	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		http.Error(w, "Ollama unhealthy", http.StatusServiceUnavailable)
		return
	}
	tagNames := make([]string, 0, len(tags.Models))
	for _, m := range tags.Models {
		tagNames = append(tagNames, m.Name)
	}

	caps := []string{"local", "chat", "embeddings", "tool_calling"}
	models := []string{}
	if registry != nil {
		caps = registry.announcedCapabilities()
		models = registry.announcedModels(tagNames)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":       "ok",
		"max_context":  maxContext,
		"capabilities": caps,
		"slots":        cap(inferenceSlots),
		"models":       models,
	})
}

// newRouter holds the route table so it can be asserted in a test: a route that
// exists in the source but is never registered is, from the control plane's
// side, indistinguishable from one that does not exist.
func newRouter() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleInference)
	mux.HandleFunc("/v1/embeddings", handleEmbeddings)
	mux.HandleFunc("/v1/images/generations", handleImageGenerations)
	mux.HandleFunc("/v1/images/edits", handleImageEdits)
	mux.HandleFunc("/v1/audio/transcriptions", handleAudioTranscriptions)
	mux.HandleFunc("/v1/audio/speech", handleAudioSpeech)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/telemetry", handleTelemetry)
	return mux
}

func main() {
	budgeter = NewMemoryBudgeter()
	ollamaURL = latticeconfig.Env("LATTICE_OLLAMA_URL", "http://localhost:11434")

	cfgPath := latticeconfig.Env("LATTICE_GATEWAY_PROVIDERS", "")
	reg, provs, err := loadProviders(cfgPath)
	if err != nil {
		log.Fatalf("Failed to load providers: %v", err)
	}
	registry = reg
	providerMutex.Lock()
	providers = provs
	providerMutex.Unlock()

	addr := latticeconfig.Env("LATTICE_GATEWAY_ADDR", ":8081")
	fmt.Printf("Lattice Gateway listening on %s (Dynamic Memory Budgeting active)\n", addr)
	log.Fatal(http.ListenAndServe(addr, newRouter()))
}
