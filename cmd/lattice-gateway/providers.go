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
}

// providerRegistry holds the routing tables resolveModel and the /health
// announcement read. It is built once at startup and only read afterwards, so it
// needs no lock of its own.
type providerRegistry struct {
	defaultProvider string
	modelProviders  map[string]string
	modelUpstream   map[string]string
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

// announcedCapabilities is the capability half of the /health announcement:
// what this gateway can serve. "local" and "chat" are intrinsic to the host;
// "embeddings" and "tool_calling" come from Ollama (MLX is chat-only).
func (reg *providerRegistry) announcedCapabilities() []string {
	caps := []string{"local", "chat"}
	for _, kind := range reg.providerKinds {
		if kind == "ollama" {
			caps = append(caps, "embeddings", "tool_calling")
			break
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
