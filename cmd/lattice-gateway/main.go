package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
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
	// Think, when non-nil, overrides whether the native Ollama request asks a
	// thinking model for its think channel. It is a pointer so an absent field
	// (nil, the default of think: false) is distinguishable from an explicit
	// true. routing.provider_params["think"] is honoured as a second override.
	Think *bool `json:"think,omitempty"`
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

// ollamaArguments re-emits OpenAI-style arguments for Ollama's native
// /api/chat. Ollama 0.34 parses the arguments text directly and rejects OpenAI's
// JSON-encoded-string convention, failing the whole request with 400 "Value
// looks like object, but can't find closing '}' symbol" whenever replayed
// history carries string arguments, so every tool loop breaks on its second
// turn. A valid-JSON arguments string re-emits verbatim as an object; anything
// else degrades to an empty object so the request stays parseable.
func ollamaArguments(raw string) json.RawMessage {
	if json.Valid([]byte(raw)) {
		return json.RawMessage(raw)
	}
	return json.RawMessage("{}")
}

// ollamaMessages normalizes an OpenAI-shaped history for Ollama's native
// /api/chat by remapping tool-call arguments from the JSON-encoded strings the
// OpenAI convention uses to the object shape Ollama requires (see
// ollamaArguments). Messages without tool calls pass through unchanged.
func ollamaMessages(messages []interface{}) []interface{} {
	out := make([]interface{}, len(messages))
	for i, rawMsg := range messages {
		msg, ok := rawMsg.(map[string]interface{})
		if !ok {
			out[i] = rawMsg
			continue
		}
		rawCalls, ok := msg["tool_calls"].([]interface{})
		if !ok {
			out[i] = rawMsg
			continue
		}
		copyMsg := make(map[string]interface{}, len(msg))
		for k, v := range msg {
			copyMsg[k] = v
		}
		calls := make([]interface{}, len(rawCalls))
		for j, rawCall := range rawCalls {
			call, ok := rawCall.(map[string]interface{})
			if !ok {
				calls[j] = rawCall
				continue
			}
			copyCall := make(map[string]interface{}, len(call))
			for k, v := range call {
				copyCall[k] = v
			}
			if fn, ok := copyCall["function"].(map[string]interface{}); ok {
				copyFn := make(map[string]interface{}, len(fn))
				for k, v := range fn {
					copyFn[k] = v
				}
				if args, ok := fn["arguments"].(string); ok {
					copyFn["arguments"] = ollamaArguments(args)
				}
				copyCall["function"] = copyFn
			}
			calls[j] = copyCall
		}
		copyMsg["tool_calls"] = calls
		out[i] = copyMsg
	}
	return out
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
	// Client is stamped in logTelemetry from the argument every call site passes,
	// so a handler cannot forget attribution (the compiler enforces that no call
	// site exists without a client). Direct dials attribute as the dialer's
	// address; relayed requests carry the frontend's X-Lattice-Client, which is
	// the original caller the lattice actually served.
	Client string `json:"client,omitempty"`
	// Locality is stamped in logTelemetry, not by callers: this process is the
	// local executor, so the value is constant, and setting it in one place makes
	// it true by construction at every call site.
	Locality string `json:"locality"`
	Error    string `json:"error,omitempty"`
}

// clientID resolves who to attribute an inference to. The frontend's forwarded
// header wins because it observed the real caller; anything else is the peer
// address of the connection that reached this gateway.
func callerID(r *http.Request) string {
	if c := r.Header.Get("X-Lattice-Client"); c != "" {
		return c
	}
	return remoteHost(r.RemoteAddr)
}

// remoteHost strips the port an address rides on: attribution is where the
// client connected from, not which ephemeral port it used.
func remoteHost(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
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

func logTelemetry(t Telemetry, client string) {
	metrics.observe(t)

	// Both set here rather than by struct field at the call sites so no site can
	// forget them — client arrives as an enforced argument, locality is constant
	// for this process. The gateway has no target field and does not gain one:
	// its provenance is this process, and locality is the one shared dimension
	// the three streams need.
	t.Locality = "local"
	t.Client = client

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

// availableBytes is the memory actually usable for new model loads: freeBytes
// minus the safety margin, floored at zero. Slot sizing counts models into this
// rather than the raw free figure so a load never plans to consume the margin
// the budgeter relies on to avoid swap.
func (mb *MemoryBudgeter) availableBytes() uint64 {
	free := mb.freeBytes()
	if free <= mb.safeMargin {
		return 0
	}
	return free - mb.safeMargin
}

var errQueueFull = fmt.Errorf("inference queue full")

type waiter struct {
	priority int
	enqueued time.Time
}

// prioritySemaphore is a dynamic counting semaphore that orders waiters by
// effective priority (tier minus aging), not arrival order.
type prioritySemaphore struct {
	mu      sync.Mutex
	cond    *sync.Cond
	used    int
	limit   int
	maxQ    int
	aging   float64 // priority points granted per second of waiting
	waiters []waiter
}

func newPrioritySemaphore(limit int, aging float64) *prioritySemaphore {
	s := &prioritySemaphore{limit: limit, maxQ: 100, aging: aging}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *prioritySemaphore) setLimit(n int) {
	s.mu.Lock()
	s.limit = n
	s.cond.Broadcast()
	s.mu.Unlock()
}
func (s *prioritySemaphore) limitValue() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit
}
func (s *prioritySemaphore) setMaxQueue(n int) {
	s.mu.Lock()
	s.maxQ = n
	s.mu.Unlock()
}
func (s *prioritySemaphore) waiting() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.waiters)
}

// effPriority is the aging-adjusted precedence; lower wins.
func (s *prioritySemaphore) effPriority(w waiter) float64 {
	return float64(w.priority) - s.aging*time.Since(w.enqueued).Seconds()
}

func (s *prioritySemaphore) acquire(ctx context.Context, priority int) (bool, error) {
	s.mu.Lock()
	if len(s.waiters) >= s.maxQ {
		s.mu.Unlock()
		return false, errQueueFull
	}
	w := waiter{priority: priority, enqueued: time.Now()}
	s.waiters = append(s.waiters, w)
	for {
		if ctx.Err() != nil {
			s.removeWaiter(w)
			s.mu.Unlock()
			return false, nil
		}
		if s.used < s.limit && s.isHead(w) {
			s.removeWaiter(w)
			s.used++
			s.mu.Unlock()
			return true, nil
		}
		s.wait(ctx)
	}
}

// isHead reports whether w is the minimum-effective-priority waiter.
func (s *prioritySemaphore) isHead(w waiter) bool {
	for _, other := range s.waiters {
		if s.effPriority(other) < s.effPriority(w) {
			return false
		}
	}
	return true
}

func (s *prioritySemaphore) removeWaiter(w waiter) {
	for i, other := range s.waiters {
		if other == w {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			return
		}
	}
}

// wait blocks until a release/setLimit broadcast or the caller's context is
// cancelled, matching the old semaphore's cancellation contract.
func (s *prioritySemaphore) wait(ctx context.Context) {
	done := ctx.Done()
	if done == nil {
		s.cond.Wait()
		return
	}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-done:
			s.cond.Broadcast()
		case <-stopped:
		}
	}()
	s.cond.Wait()
	close(stopped)
}

func (s *prioritySemaphore) release() {
	s.mu.Lock()
	s.used--
	s.cond.Broadcast()
	s.mu.Unlock()
}

// computeSlots returns how many of the given model sizes (bytes) fit into
// available memory using a smallest-first greedy fit, which maximizes the count.
// Clamped to [1, max]. The result is a ceiling, not a reservation: the budgeter
// still rejects an individual load that would overrun the safety margin.
func computeSlots(sizes []int64, availableBytes uint64, max int) int {
	if max < 1 {
		max = 1
	}
	sorted := make([]int64, len(sizes))
	copy(sorted, sizes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var used uint64
	count := 0
	for _, sz := range sorted {
		if sz <= 0 {
			continue
		}
		if used+uint64(sz) > availableBytes {
			break
		}
		used += uint64(sz)
		count++
	}
	if count < 1 {
		count = 1
	}
	if count > max {
		count = max
	}
	return count
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

// thinkRequested decides whether the native Ollama request should ask the model
// to emit its thinking channel. The gateway defaults to false: a thinking model
// that fills only the think channel leaves OpenAI content empty while usage
// still reports a full completion, which clients render as a blank reply (the
// gemma4:12b failure). An explicit override wins: the top-level think field if
// the client sent one, otherwise routing.provider_params["think"].
func thinkRequested(req Request) bool {
	if req.Think != nil {
		return *req.Think
	}
	if v, ok := req.Routing.ProviderParams["think"].(bool); ok {
		return v
	}
	return false
}

// ollamaContent picks the text an OpenAI client should see from a native Ollama
// message. Content wins whenever it has any non-whitespace; only when content is
// empty does the thinking channel stand in, so a model that filled only its
// think channel still yields a non-empty reply instead of a blank one. Thinking
// is never merged into a non-empty content: doing so would corrupt the answer.
func ollamaContent(content, thinking, reasoning string) string {
	if strings.TrimSpace(content) != "" {
		return content
	}
	for _, candidate := range []string{thinking, reasoning} {
		if strings.TrimSpace(candidate) != "" {
			return candidate
		}
	}
	return ""
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
		"messages": ollamaMessages(req.Messages),
		"tools":    req.Tools,
		"stream":   false,
		"think":    thinkRequested(req),
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
			// Thinking and Reasoning are the channels a thinking model may use
			// instead of (or before) content. They are decoded so content can
			// fall back to them when content itself is empty.
			Thinking  string `json:"thinking"`
			Reasoning string `json:"reasoning"`
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
			Index: 0,
			Message: Message{
				Role:      native.Message.Role,
				Content:   ollamaContent(native.Message.Content, native.Message.Thinking, native.Message.Reasoning),
				ToolCalls: toolCalls,
			},
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
		"messages": ollamaMessages(req.Messages),
		"tools":    req.Tools,
		"stream":   true,
		"think":    thinkRequested(req),
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
	var content, thinking, reasoning strings.Builder

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
				Content   string `json:"content"`
				Thinking  string `json:"thinking"`
				Reasoning string `json:"reasoning"`
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
		if ev.Message.Thinking != "" {
			thinking.WriteString(ev.Message.Thinking)
		}
		if ev.Message.Reasoning != "" {
			reasoning.WriteString(ev.Message.Reasoning)
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

	// A thinking-only model leaves the content builder empty even though Ollama
	// streamed a full think channel. Emit the aggregated thinking as content only
	// now, at the end, so a model that also streamed real content never has its
	// thinking interleaved into the reply.
	assembled := content.String()
	if strings.TrimSpace(assembled) == "" {
		if fallback := ollamaContent("", thinking.String(), reasoning.String()); fallback != "" {
			writeSSEChunk(w, id, created, model, fallback, "")
			if flusher != nil {
				flusher.Flush()
			}
			assembled = fallback
		}
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
			Message:      Message{Role: "assistant", Content: assembled},
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

// agingWeight controls how fast a queued request's effective priority improves,
// configurable via LATTICE_GATEWAY_AGING_WEIGHT (priority points per second of
// waiting). A positive weight lets lower-tier requests overtake higher-tier ones
// after enough queue time, bounding starvation.
func agingWeight() float64 {
	if v := latticeconfig.Env("LATTICE_GATEWAY_AGING_WEIGHT", ""); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return 1.0
}

// acquireSlot takes the single local-inference slot, reporting false if the
// caller gave up while queued and errQueueFull if the queue is at capacity.
// Blocking forever on the slot would let an abandoned request hold up every
// later one on a machine that can only run one model at a time.
func acquireSlot(ctx context.Context, priority int) (bool, error) {
	return inferenceSlots.acquire(ctx, priority)
}

func priorityFor(latencyClass string) int {
	switch latencyClass {
	case "interactive":
		return 0
	case "batch":
		return 2
	default:
		return 1
	}
}

func headerPriority(r *http.Request) int {
	return priorityFor(r.Header.Get("X-Priority"))
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

	// inferenceSlots bounds concurrent local inference. Its limit is dynamic: the
	// health poll recomputes it from free memory + the announced model sizes, so
	// a host with headroom for several co-resident models can serve them without
	// the eviction churn a hard single slot would force. The memory budgeter
	// remains the per-request backstop that rejects when free RAM drops below the
	// safety margin.
	inferenceSlots = newPrioritySemaphore(1, agingWeight())

	// maxSlots caps the dynamic slot count so a host full of tiny models never
	// announces unbounded concurrency. Overridden in main() from
	// LATTICE_GATEWAY_MAX_SLOTS.
	maxSlots = 3
)

// logQueuedCancel records a request whose client went away while it waited for
// the inference slot. Nothing was sent to Ollama, so there is no status left to
// report, but the gap would otherwise be invisible in the telemetry stream.
func logQueuedCancel(req Request, ctxWindow int, t0 time.Time, client string) {
	logTelemetry(Telemetry{
		RequestID:     req.Routing.RequestID,
		Model:         req.Model,
		ContextWindow: ctxWindow,
		Elapsed:       time.Since(t0).Seconds(),
		Error:         "client_cancelled_while_queued",
	}, client)
}

func handleInference(w http.ResponseWriter, r *http.Request) {
	caller := callerID(r)
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
		}, caller)
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
			granted, err := acquireSlot(r.Context(), priorityFor(req.Routing.LatencyClass))
			if err == errQueueFull {
				logTelemetry(Telemetry{RequestID: req.Routing.RequestID, Model: req.Model, ContextWindow: ctxWindow, Elapsed: time.Since(t0).Seconds(), Error: "queue_full"}, caller)
				http.Error(w, "Inference queue full; retry later", http.StatusTooManyRequests)
				return
			}
			if !granted {
				logQueuedCancel(req, ctxWindow, t0, caller)
				return
			}
			res, committed, err := sp.ExecuteStream(r.Context(), upstreamReq, w)
			inferenceSlots.release()

			te := Telemetry{
				RequestID:     req.Routing.RequestID,
				Model:         req.Model,
				ContextWindow: ctxWindow,
				Elapsed:       time.Since(t0).Seconds(),
			}
			if err != nil {
				te.Error = err.Error()
				logTelemetry(te, caller)
				// Only a failure before the first byte can still become a status code.
				if !committed {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
				return
			}
			te.PromptTokens = res.Usage.PromptTokens
			te.CompletionTokens = res.Usage.CompletionTokens
			logTelemetry(te, caller)
			return
		}
	}

	granted, err := acquireSlot(r.Context(), priorityFor(req.Routing.LatencyClass))
	if err == errQueueFull {
		logTelemetry(Telemetry{RequestID: req.Routing.RequestID, Model: req.Model, ContextWindow: ctxWindow, Elapsed: time.Since(t0).Seconds(), Error: "queue_full"}, caller)
		http.Error(w, "Inference queue full; retry later", http.StatusTooManyRequests)
		return
	}
	if !granted {
		logQueuedCancel(req, ctxWindow, t0, caller)
		return
	}
	res, err := provider.Execute(r.Context(), upstreamReq)
	inferenceSlots.release()

	if err != nil {
		logTelemetry(Telemetry{
			RequestID:     req.Routing.RequestID,
			Model:         req.Model,
			ContextWindow: ctxWindow,
			Elapsed:       time.Since(t0).Seconds(),
			Error:         err.Error(),
		}, caller)
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
	}, caller)

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
	caller := callerID(r)

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()}, caller)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !budgeter.CanAccommodate() {
		fmt.Println("  Memory pressure: available RAM below safety margin. Rejecting embedding to avoid swap.")
		logTelemetry(Telemetry{
			RequestID: requestID,
			Elapsed:   time.Since(t0).Seconds(),
			Error:     "memory_pressure",
		}, caller)
		http.Error(w, "Local memory pressure: available RAM below safety margin", http.StatusTooManyRequests)
		return
	}

	// The same single slot chat takes, for the same reason: with one model
	// resident, a concurrent embed evicts the resident chat model — which is the
	// swap pressure this project has already paid for once.
	granted, err := acquireSlot(r.Context(), headerPriority(r))
	if err == errQueueFull {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: "queue_full"}, caller)
		http.Error(w, "Inference queue full; retry later", http.StatusTooManyRequests)
		return
	}
	if !granted {
		logTelemetry(Telemetry{
			RequestID: requestID,
			Elapsed:   time.Since(t0).Seconds(),
			Error:     "client_cancelled_while_queued",
		}, caller)
		return
	}
	defer inferenceSlots.release()

	upstream, err := http.NewRequestWithContext(r.Context(), "POST",
		ollamaURL+"/v1/embeddings", bytes.NewReader(bodyBytes))
	if err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()}, caller)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: ollamaTimeout()}
	resp, err := client.Do(upstream)
	if err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()}, caller)
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
	logTelemetry(te, caller)
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
	caller := callerID(r)

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()}, caller)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var img ImageRequest
	if err := json.Unmarshal(bodyBytes, &img); err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()}, caller)
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
		}, caller)
		http.Error(w, "Local memory pressure: available RAM below image margin", http.StatusTooManyRequests)
		return
	}

	granted, err := acquireSlot(r.Context(), headerPriority(r))
	if err == errQueueFull {
		logTelemetry(Telemetry{RequestID: requestID, Model: img.Model, Elapsed: time.Since(t0).Seconds(), Error: "queue_full"}, caller)
		http.Error(w, "Inference queue full; retry later", http.StatusTooManyRequests)
		return
	}
	if !granted {
		logTelemetry(Telemetry{
			RequestID: requestID,
			Model:     img.Model,
			Elapsed:   time.Since(t0).Seconds(),
			Error:     "client_cancelled_while_queued",
		}, caller)
		return
	}
	defer inferenceSlots.release()

	providerName, _ := resolveModel(img.Model)
	providerMutex.RLock()
	provider := providers[providerName]
	providerMutex.RUnlock()
	if provider == nil {
		logTelemetry(Telemetry{RequestID: requestID, Model: img.Model, Elapsed: time.Since(t0).Seconds(), Error: "no provider registered for model"}, caller)
		http.Error(w, "No provider registered for model", http.StatusInternalServerError)
		return
	}

	ip, ok := provider.(ImageProvider)
	if !ok {
		logTelemetry(Telemetry{RequestID: requestID, Model: img.Model, Elapsed: time.Since(t0).Seconds(), Error: fmt.Sprintf("provider %q is not an image provider", providerName)}, caller)
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
		logTelemetry(te, caller)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	logTelemetry(te, caller)

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
	caller := callerID(r)

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()}, caller)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !budgeter.CanAccommodate() {
		fmt.Println("  Memory pressure: available RAM below safety margin. Rejecting speech request.")
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: "memory_pressure"}, caller)
		http.Error(w, "Local memory pressure: available RAM below safety margin", http.StatusTooManyRequests)
		return
	}

	granted, err := acquireSlot(r.Context(), headerPriority(r))
	if err == errQueueFull {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: "queue_full"}, caller)
		http.Error(w, "Inference queue full; retry later", http.StatusTooManyRequests)
		return
	}
	if !granted {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: "client_cancelled_while_queued"}, caller)
		return
	}
	defer inferenceSlots.release()

	// The two ops decode different request shapes but share the resolve →
	// type-assert → call skeleton. A model routed to a non-speech provider is a
	// config fault and fails loudly, never re-homed to Ollama.
	if op == "synthesize" {
		var req SynthesisRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()}, caller)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Model == "" {
			req.Model = defaultSynthesisModel
		}
		sp, providerName, ok := speechCapability(req.Model)
		if !ok {
			logTelemetry(Telemetry{RequestID: requestID, Model: req.Model, Elapsed: time.Since(t0).Seconds(), Error: fmt.Sprintf("provider %q is not a speech provider", providerName)}, caller)
			http.Error(w, "Provider is not a speech provider", http.StatusInternalServerError)
			return
		}
		res, err := sp.Synthesize(r.Context(), req)
		te := Telemetry{RequestID: requestID, Model: req.Model, Elapsed: time.Since(t0).Seconds()}
		if err != nil {
			te.Error = err.Error()
			logTelemetry(te, caller)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		logTelemetry(te, caller)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
		return
	}

	var req TranscriptionRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		logTelemetry(Telemetry{RequestID: requestID, Elapsed: time.Since(t0).Seconds(), Error: err.Error()}, caller)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = defaultTranscriptionModel
	}
	sp, providerName, ok := speechCapability(req.Model)
	if !ok {
		logTelemetry(Telemetry{RequestID: requestID, Model: req.Model, Elapsed: time.Since(t0).Seconds(), Error: fmt.Sprintf("provider %q is not a speech provider", providerName)}, caller)
		http.Error(w, "Provider is not a speech provider", http.StatusInternalServerError)
		return
	}
	res, err := sp.Transcribe(r.Context(), req)
	te := Telemetry{RequestID: requestID, Model: req.Model, Elapsed: time.Since(t0).Seconds()}
	if err != nil {
		te.Error = err.Error()
		logTelemetry(te, caller)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	logTelemetry(te, caller)
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
			Size int64  `json:"size"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		http.Error(w, "Ollama unhealthy", http.StatusServiceUnavailable)
		return
	}
	tagNames := make([]string, 0, len(tags.Models))
	sizes := make([]int64, 0, len(tags.Models))
	for _, m := range tags.Models {
		tagNames = append(tagNames, m.Name)
		if m.Size > 0 {
			sizes = append(sizes, m.Size)
		}
	}

	caps := []string{"local", "chat", "embeddings", "tool_calling"}
	models := []string{}
	if registry != nil {
		caps = registry.announcedCapabilities()
		models = registry.announcedModels(tagNames)
	}

	// Recompute the concurrency ceiling from free memory and the announced model
	// sizes, then apply it to the semaphore so the reported slot count and the
	// actual enforcement always agree.
	slots := computeSlots(sizes, budgeter.availableBytes(), maxSlots)
	inferenceSlots.setLimit(slots)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":       "ok",
		"max_context":  maxContext,
		"capabilities": caps,
		"slots":        slots,
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
	mux.HandleFunc("/metrics", handleMetrics)
	return mux
}

func main() {
	budgeter = NewMemoryBudgeter()
	ollamaURL = latticeconfig.Env("LATTICE_OLLAMA_URL", "http://localhost:11434")

	if v := latticeconfig.Env("LATTICE_GATEWAY_MAX_SLOTS", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxSlots = n
		}
	}

	if v := latticeconfig.Env("LATTICE_GATEWAY_MAX_QUEUE", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			inferenceSlots.setMaxQueue(n)
		}
	}

	cfgPath := latticeconfig.Env("LATTICE_GATEWAY_PROVIDERS", "")
	reg, provs, err := loadProviders(cfgPath)
	if err != nil {
		log.Fatalf("Failed to load providers: %v", err)
	}
	registry = reg
	providerMutex.Lock()
	providers = provs
	providerMutex.Unlock()

	for _, a := range gatewayBindAddrs() {
		go serveOn(a)
	}
	// Every serveOn blocks for the process's lifetime; failure inside one is
	// fatal to the process, so main has nothing better to do than wait.
	select {}
}

// gatewayBindAddrs resolves the listeners to open. LATTICE_GATEWAY_BIND lists
// explicit "ip:port" pairs — a gateway normally serves 127.0.0.1 for its own
// host plus its tailnet address, which is what the control plane polls. Without
// it the legacy LATTICE_GATEWAY_ADDR semantics apply unchanged, including the
// wildcard default. The gateway is unauthenticated; who can reach the port is
// therefore a bind decision, not application logic.
func gatewayBindAddrs() []string {
	if b := latticeconfig.Env("LATTICE_GATEWAY_BIND", ""); b != "" {
		var out []string
		for _, e := range strings.Split(b, ",") {
			if e = strings.TrimSpace(e); e != "" {
				out = append(out, e)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{latticeconfig.Env("LATTICE_GATEWAY_ADDR", ":8081")}
}

// serveOn opens one listener and serves it for the process's lifetime. A
// machine booting can reach here before Tailscale has raised the tailnet
// interface, so an address that is not up yet is retried — dying on the first
// attempt would leave launchd restarting the whole gateway (and losing the
// telemetry ring) over a race the interface resolves by itself within seconds.
// The other listeners are unaffected either way.
func serveOn(addr string) {
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			fmt.Printf("Lattice Gateway listening on %s (Dynamic Memory Budgeting active)\n", addr)
			log.Fatal(http.Serve(ln, newRouter()))
		}
		fmt.Printf("Lattice Gateway waiting for %s: %v (retry in 5s)\n", addr, err)
		time.Sleep(5 * time.Second)
	}
}
