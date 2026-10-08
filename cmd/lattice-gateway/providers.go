package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pegamonstro/ai-tools-inference-lattice/pkg/latticeconfig"
)

// providerConfigFile is the on-disk shape of LATTICE_GATEWAY_PROVIDERS. When the
// variable is unset the gateway runs with the single-provider behaviour it has
// always had (Ollama only, every model passed through), so an existing deployment
// is untouched until it opts in by pointing the variable at a file.
type providerConfigFile struct {
	DefaultProvider string          `json:"default_provider"`
	Providers       []providerEntry `json:"providers"`
	Models          []modelRoute    `json:"models"`
}

type providerEntry struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Endpoint string `json:"endpoint"`
}

// modelRoute pins one local model id to a provider, optionally renaming it for
// the upstream engine (an MLX model is named by its Hugging Face repo, not by an
// Ollama tag).
type modelRoute struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Upstream string `json:"upstream"`
	// Context is an optional per-model ceiling on the context window, overriding
	// the global default. A linear-attention model (LFM2.5) can afford 64K where
	// a dense local model cannot; only the models that can are given the raise.
	Context int `json:"context"`
}

// providerRegistry holds the routing tables resolveModel and the /health
// announcement read. It is built once at startup and only read afterwards, so it
// needs no lock of its own.
type providerRegistry struct {
	defaultProvider string
	modelProviders  map[string]string
	modelUpstream   map[string]string
	modelContexts   map[string]int
	// served lists, per provider name, the local model ids explicitly routed to
	// it. The default provider's slice is left to mean "everything else not
	// named", not an exhaustive inventory (that would duplicate Ollama /api/tags).
	served        map[string][]string
	providerKinds map[string]string
}

// loadProviders builds the provider map and the routing tables from the config
// file at cfgPath. An empty path yields the legacy single-provider behaviour.
func loadProviders(cfgPath string) (*providerRegistry, map[string]Provider, error) {
	reg := &providerRegistry{
		defaultProvider: "ollama",
		modelProviders:  map[string]string{},
		modelUpstream:   map[string]string{},
		modelContexts:   map[string]int{},
		served:          map[string][]string{},
		providerKinds:   map[string]string{},
	}
	provs := map[string]Provider{}
	ollamaURL := latticeconfig.Env("LATTICE_OLLAMA_URL", "http://localhost:11434")

	if cfgPath == "" {
		provs["ollama"] = &OllamaProvider{Endpoint: ollamaURL}
		reg.providerKinds["ollama"] = "ollama"
		return reg, provs, nil
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	var cfg providerConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, nil, err
	}

	if cfg.DefaultProvider != "" {
		reg.defaultProvider = cfg.DefaultProvider
	}

	for _, p := range cfg.Providers {
		if p.Name == "" {
			continue
		}
		reg.served[p.Name] = nil
		reg.providerKinds[p.Name] = p.Kind
		switch p.Kind {
		case "ollama":
			provs[p.Name] = &OllamaProvider{Endpoint: p.Endpoint}
		case "mlx":
			provs[p.Name] = &MLXProvider{Endpoint: p.Endpoint}
		case "mflux":
			provs[p.Name] = &MfluxProvider{Endpoint: p.Endpoint}
		case "speech":
			provs[p.Name] = &SpeechProvider{Endpoint: p.Endpoint}
		default:
			return nil, nil, fmt.Errorf("provider %q has unknown kind %q", p.Name, p.Kind)
		}
	}

	for _, m := range cfg.Models {
		if m.Name == "" || m.Provider == "" {
			continue
		}
		reg.modelProviders[m.Name] = m.Provider
		if m.Upstream != "" {
			reg.modelUpstream[m.Name] = m.Upstream
		}
		if m.Context > 0 {
			reg.modelContexts[m.Name] = m.Context
		}
		reg.served[m.Provider] = append(reg.served[m.Provider], m.Name)
	}

	return reg, provs, nil
}

// resolveModel maps a client-named model to its provider and upstream name. A
// model not listed in the config falls to the default provider with its name
// passed through unchanged.
func resolveModel(model string) (provider, upstream string) {
	if registry == nil {
		return "ollama", model
	}
	if p, ok := registry.modelProviders[model]; ok {
		u := model
		if v, ok := registry.modelUpstream[model]; ok {
			u = v
		}
		return p, u
	}
	return registry.defaultProvider, model
}

// modelContextCeiling returns the context ceiling a model may load at: an
// explicit per-model override from config, or the global default otherwise. The
// override lets a linear-attention model (LFM2.5) afford 64K+ where a dense
// local model cannot, without raising the global ceiling that keeps the dense
// ones out of swap.
func modelContextCeiling(model string) int {
	if registry != nil {
		if c, ok := registry.modelContexts[model]; ok && c > 0 {
			return c
		}
	}
	return maxContext
}

// announcedCapabilities is the capability half of the /health announcement:
// what this gateway can serve. "local" and "chat" are intrinsic to the host;
// "embeddings" and "tool_calling" come from Ollama (MLX is chat-only).
func (reg *providerRegistry) announcedCapabilities() []string {
	caps := []string{"local", "chat"}
	seen := map[string]bool{}
	add := func(v string) {
		if !seen[v] {
			seen[v] = true
			caps = append(caps, v)
		}
	}
	for _, kind := range reg.providerKinds {
		switch kind {
		case "ollama":
			add("embeddings")
			add("tool_calling")
		case "mflux":
			add("image_generation")
		case "speech":
			add("speech_recognition")
			add("speech_synthesis")
		}
	}
	return caps
}

// isCloudTag reports whether a model name is an Ollama routing alias rather than
// a local model — the ":cloud" / "<size>-cloud" suffix the measurements record
// documents.
func isCloudTag(name string) bool {
	i := strings.LastIndex(name, ":")
	if i < 0 {
		return false
	}
	tag := name[i+1:]
	return tag == "cloud" || strings.HasSuffix(tag, "-cloud")
}

// announcedModels is the flattened list of local model ids the gateway serves,
// across all providers: every model explicitly routed in config, plus every
// local model Ollama reports via /api/tags, cloud aliases dropped.
func (reg *providerRegistry) announcedModels(ollamaTags []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, models := range reg.served {
		for _, m := range models {
			if m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	for _, m := range ollamaTags {
		if m != "" && !isCloudTag(m) && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// MLXProvider is an HTTP client for an mlx-lm server (started via mlx_lm.server,
// which exposes an OpenAI-compatible /v1/chat/completions). It is chat-only: it
// forwards messages and returns text, with no tool-call translation and no
// num_ctx/KV-cache control — mlx-lm sets its own context at server load.
type MLXProvider struct {
	Endpoint string

	// transportOnce guards transport, for the same reason as OllamaProvider:
	// one shared transport so streamed answers reuse connections instead of
	// stranding one per request.
	transportOnce sync.Once
	transport     *http.Transport
}

func (p *MLXProvider) Name() string {
	return "mlx"
}

func (p *MLXProvider) httpTransport() *http.Transport {
	p.transportOnce.Do(func() {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = ollamaTimeout()
		p.transport = t
	})
	return p.transport
}

// Execute POSTs a chat-only completion to the mlx-lm server. Its response is
// already OpenAI-shaped, so it decodes straight into the gateway's own Response
// types — no translation the way Ollama's native /api/chat needs one.
func (p *MLXProvider) Execute(ctx context.Context, req Request) (*Response, error) {
	targetURL, _ := url.Parse(p.Endpoint + "/v1/chat/completions")

	body := map[string]interface{}{
		"model":      req.Model,
		"messages":   req.Messages,
		"stream":     false,
		"max_tokens": resolveMaxTokens(req),
	}
	b, _ := json.Marshal(body)

	httpReq, _ := http.NewRequestWithContext(ctx, "POST", targetURL.String(), bytes.NewBuffer(b))
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Transport: p.httpTransport(), Timeout: ollamaTimeout()}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mlx returned status %d", resp.StatusCode)
	}

	var out Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExecuteStream streams the completion as OpenAI-compatible SSE, the same shape
// as OllamaProvider.ExecuteStream so the client contract (finish_reason, [DONE])
// is identical. stream_options.include_usage makes the terminal chunk carry token
// counts for telemetry.
func (p *MLXProvider) ExecuteStream(ctx context.Context, req Request, w http.ResponseWriter) (*Response, bool, error) {
	targetURL, _ := url.Parse(p.Endpoint + "/v1/chat/completions")

	body := map[string]interface{}{
		"model":      req.Model,
		"messages":   req.Messages,
		"stream":     true,
		"max_tokens": resolveMaxTokens(req),
		"stream_options": map[string]interface{}{
			"include_usage": true,
		},
	}
	b, _ := json.Marshal(body)

	httpReq, _ := http.NewRequestWithContext(ctx, "POST", targetURL.String(), bytes.NewBuffer(b))
	httpReq.Header.Set("Content-Type", "application/json")

	// No total timeout: a streamed answer may legitimately run long. The caller's
	// context governs cancellation; ResponseHeaderTimeout (set on the transport)
	// still catches a stalled server before the first byte.
	client := &http.Client{Transport: p.httpTransport()}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("mlx returned status %d", resp.StatusCode)
	}

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
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var ev struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *Usage `json:"usage"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		if ev.Model != "" {
			model = ev.Model
		}
		if ev.Created != 0 {
			created = ev.Created
		}
		if ev.Usage != nil {
			promptTokens, completionTokens = ev.Usage.PromptTokens, ev.Usage.CompletionTokens
		}
		if len(ev.Choices) == 0 {
			continue
		}
		delta := ev.Choices[0].Delta.Content
		if delta == "" {
			continue
		}
		content.WriteString(delta)
		writeSSEChunk(w, id, created, model, delta, "")
		if flusher != nil {
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, true, err
	}

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

// imageTimeout is the gateway→sidecar client timeout for an image call. It is
// separate from ollamaTimeout because a diffusion step is an order of magnitude
// slower than a chat token: the sidecar's own MFLUX_GEN_TIMEOUT is 3600 s, and
// the client must outlast it rather than inherit the 20 m chat budget.
func imageTimeout() time.Duration {
	if v := latticeconfig.Env("LATTICE_GATEWAY_IMAGE_TIMEOUT", ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 2 * time.Hour
}

// ImageProvider is implemented by providers that generate images. handleImage
// type-asserts to it, exactly as handleInference asserts to StreamingProvider:
// a request that names a provider without the capability fails loudly rather
// than being silently proxied to a chat endpoint that does not exist.
type ImageProvider interface {
	GenerateImage(ctx context.Context, req ImageRequest) (*ImageResponse, error)
	EditImage(ctx context.Context, req ImageRequest) (*ImageResponse, error)
}

// MfluxProvider is an HTTP client for the mflux sidecar (deploy/mflux-sidecar.py),
// which serves FLUX over POST /generate and /edit. It is image-only: Execute
// fails loudly, because a chat request naming the image model is a routing fault
// that must not be silently re-homed to Ollama.
type MfluxProvider struct {
	Endpoint string
}

func (p *MfluxProvider) Name() string { return "mflux" }

// Execute exists only to satisfy Provider. A chat request that names the image
// model reaches here and must fail loudly rather than be silently dropped.
func (p *MfluxProvider) Execute(ctx context.Context, req Request) (*Response, error) {
	return nil, fmt.Errorf("mflux is an image provider, not a chat provider")
}

func (p *MfluxProvider) GenerateImage(ctx context.Context, req ImageRequest) (*ImageResponse, error) {
	return p.runImage(ctx, req, "generate")
}

func (p *MfluxProvider) EditImage(ctx context.Context, req ImageRequest) (*ImageResponse, error) {
	return p.runImage(ctx, req, "edit")
}

// runImage translates the OpenAI Images shape to the sidecar's and back, one
// sidecar call per requested image. The sidecar is single-flight and holds the
// model resident, so n images are sequential by construction; the gateway slot
// already serialises them anyway.
func (p *MfluxProvider) runImage(ctx context.Context, req ImageRequest, op string) (*ImageResponse, error) {
	width, height, err := parseImageSize(req.Size)
	if err != nil {
		return nil, err
	}
	if req.ResponseFormat == "url" {
		return nil, fmt.Errorf("response_format %q unsupported: the sidecar returns base64 only, use b64_json", req.ResponseFormat)
	}
	if op == "edit" && req.Mask != "" {
		return nil, fmt.Errorf("mask is not supported by the mflux sidecar")
	}

	n := req.N
	if n <= 0 {
		n = 1
	}

	out := &ImageResponse{Created: time.Now().Unix(), Data: make([]ImageDataItem, 0, n)}
	for i := 0; i < n; i++ {
		item, err := p.oneImage(ctx, req, op, width, height)
		if err != nil {
			return nil, err
		}
		out.Data = append(out.Data, item)
	}
	return out, nil
}

func (p *MfluxProvider) oneImage(ctx context.Context, req ImageRequest, op string, width, height int) (ImageDataItem, error) {
	body := map[string]interface{}{
		"prompt": req.Prompt,
		"width":  width,
		"height": height,
	}
	if req.SidecarModel != "" {
		body["model"] = req.SidecarModel
	}
	if len(req.Loras) > 0 {
		body["loras"] = req.Loras
	}
	if req.Steps > 0 {
		body["steps"] = req.Steps
	}
	if req.Guidance > 0 {
		body["guidance"] = req.Guidance
	}
	if req.NegativePrompt != "" {
		body["negative_prompt"] = req.NegativePrompt
	}
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}
	if req.Strength != nil && op == "edit" {
		body["strength"] = *req.Strength
	}
	if op == "edit" {
		body["init_image"] = req.Image
	}
	b, err := json.Marshal(body)
	if err != nil {
		return ImageDataItem{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.Endpoint+"/"+op, bytes.NewBuffer(b))
	if err != nil {
		return ImageDataItem{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: imageTimeout()}
	resp, err := client.Do(httpReq)
	if err != nil {
		return ImageDataItem{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		msg := e.Error
		if msg == "" {
			msg = fmt.Sprintf("sidecar status %d", resp.StatusCode)
		}
		return ImageDataItem{}, fmt.Errorf("mflux: %s", msg)
	}

	var side struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&side); err != nil {
		return ImageDataItem{}, err
	}
	return ImageDataItem{B64JSON: side.Image}, nil
}

// parseImageSize turns OpenAI's "WxH" size into the sidecar's width/height.
// An empty size defaults to 1024x1024 (the sidecar's own default); a size the
// sidecar could not honour is refused rather than guessed.
func parseImageSize(size string) (int, int, error) {
	if size == "" {
		return 1024, 1024, nil
	}
	var w, h int
	if _, err := fmt.Sscanf(size, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("invalid size %q: want WxH", size)
	}
	return w, h, nil
}

// speechTimeout is the gateway→sidecar client timeout for a speech call. It is
// generous because a transcription of long audio can outlast any chat budget; the
// sidecar's own subprocess timeout is 600 s, and the client must outlast it.
func speechTimeout() time.Duration {
	if v := latticeconfig.Env("LATTICE_GATEWAY_SPEECH_TIMEOUT", ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 15 * time.Minute
}

// SpeechCapability is implemented by providers that transcribe or synthesize
// speech. handleTranscriptions/handleSpeech type-assert to it, exactly as
// handleImage asserts to ImageProvider: a request that names a provider without
// the capability fails loudly rather than being silently proxied to a chat
// endpoint that does not exist.
type SpeechCapability interface {
	Transcribe(ctx context.Context, req TranscriptionRequest) (*TranscriptionResponse, error)
	Synthesize(ctx context.Context, req SynthesisRequest) (*SynthesisResponse, error)
}

// SpeechProvider is an HTTP client for the speech sidecar
// (deploy/speech-sidecar.py), which serves kokoro-mlx TTS and mlx-whisper STT.
// It is speech-only: Execute fails loudly, because a chat request naming a speech
// model is a routing fault that must not be silently re-homed to Ollama.
type SpeechProvider struct {
	Endpoint string
}

func (p *SpeechProvider) Name() string { return "speech" }

// Execute exists only to satisfy Provider. A chat request that names the speech
// model reaches here and must fail loudly rather than be silently dropped.
func (p *SpeechProvider) Execute(ctx context.Context, req Request) (*Response, error) {
	return nil, fmt.Errorf("speech is a speech provider, not a chat provider")
}

func (p *SpeechProvider) Transcribe(ctx context.Context, req TranscriptionRequest) (*TranscriptionResponse, error) {
	var out TranscriptionResponse
	// The sidecar's /transcribe names its base64 audio field "audio"; the gateway
	// exposes the OpenAI-style "file" to clients, so translate at the boundary.
	// (Synthesis needs no such step: the sidecar accepts "input" as an alias.)
	if err := p.post(ctx, "/transcribe", map[string]string{"audio": req.File}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *SpeechProvider) Synthesize(ctx context.Context, req SynthesisRequest) (*SynthesisResponse, error) {
	var out SynthesisResponse
	if err := p.post(ctx, "/synthesize", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// post is the shared POST to the sidecar: marshal the request, decode the
// response or the sidecar's {"error": ...} envelope into an error.
func (p *SpeechProvider) post(ctx context.Context, path string, in, out interface{}) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.Endpoint+path, bytes.NewBuffer(b))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: speechTimeout()}
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		msg := e.Error
		if msg == "" {
			msg = fmt.Sprintf("speech sidecar status %d", resp.StatusCode)
		}
		return fmt.Errorf("speech: %s", msg)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
