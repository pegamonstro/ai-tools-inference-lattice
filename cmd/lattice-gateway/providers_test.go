package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveModelDefaultsToOllamaPassthrough(t *testing.T) {
	registry = &providerRegistry{
		defaultProvider: "ollama",
		modelProviders:  map[string]string{},
		modelUpstream:   map[string]string{},
	}
	defer func() { registry = nil }()

	p, u := resolveModel("granite3-moe:3b")
	if p != "ollama" || u != "granite3-moe:3b" {
		t.Fatalf("unmapped model: got provider=%q upstream=%q, want ollama passthrough", p, u)
	}
}

func TestResolveModelRoutesMappedModelWithUpstreamRename(t *testing.T) {
	registry = &providerRegistry{
		defaultProvider: "ollama",
		modelProviders:  map[string]string{"qwen2.5-coder:3b": "mlx"},
		modelUpstream:   map[string]string{"qwen2.5-coder:3b": "mlx-community/Qwen2.5-Coder-3B-Instruct-4bit"},
	}
	defer func() { registry = nil }()

	p, u := resolveModel("qwen2.5-coder:3b")
	if p != "mlx" || u != "mlx-community/Qwen2.5-Coder-3B-Instruct-4bit" {
		t.Fatalf("mapped model: got provider=%q upstream=%q, want mlx + HF id", p, u)
	}
}

func TestLoadProvidersBuildsRegistryAndAnnouncement(t *testing.T) {
	cfg := `{
		"default_provider": "ollama",
		"providers": [
			{"name": "ollama", "kind": "ollama", "endpoint": "http://localhost:11434"},
			{"name": "mlx", "kind": "mlx", "endpoint": "http://localhost:8080"}
		],
		"models": [
			{"name": "qwen2.5-coder:3b", "provider": "mlx", "upstream": "mlx-community/Qwen2.5-Coder-3B-Instruct-4bit"}
		]
	}`
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(path, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}

	reg, provs, err := loadProviders(path)
	if err != nil {
		t.Fatalf("loadProviders: %v", err)
	}
	if _, ok := provs["ollama"]; !ok {
		t.Error("ollama provider not registered")
	}
	if _, ok := provs["mlx"]; !ok {
		t.Error("mlx provider not registered")
	}
	if reg.defaultProvider != "ollama" {
		t.Errorf("default provider = %q, want ollama", reg.defaultProvider)
	}
	caps := reg.announcedCapabilities()
	for _, want := range []string{"local", "chat", "embeddings", "tool_calling"} {
		if !contains(caps, want) {
			t.Errorf("announcedCapabilities missing %q: %v", want, caps)
		}
	}
	models := reg.announcedModels(nil)
	if len(models) != 1 || models[0] != "qwen2.5-coder:3b" {
		t.Errorf("announcedModels = %v, want [qwen2.5-coder:3b]", models)
	}
}

func TestLoadProvidersUnknownKindFails(t *testing.T) {
	cfg := `{
		"providers": [
			{"name": "ghost", "kind": "spooky", "endpoint": "http://localhost:1"}
		]
	}`
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(path, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadProviders(path); err == nil {
		t.Error("unknown provider kind should fail loudly")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestAnnouncedCapabilitiesChatOnlyForMLX(t *testing.T) {
	reg := &providerRegistry{providerKinds: map[string]string{"mlx": "mlx"}}
	caps := reg.announcedCapabilities()
	if contains(caps, "embeddings") {
		t.Fatalf("MLX-only gateway must not announce embeddings: %v", caps)
	}
}

func TestAnnouncedCapabilitiesAddsOllamaModalities(t *testing.T) {
	reg := &providerRegistry{providerKinds: map[string]string{"ollama": "ollama", "mlx": "mlx"}}
	caps := reg.announcedCapabilities()
	for _, want := range []string{"local", "chat", "embeddings", "tool_calling"} {
		if !contains(caps, want) {
			t.Fatalf("announcedCapabilities missing %q: %v", want, caps)
		}
	}
}

func TestAnnouncedModelsFlattensAndDropsCloudTags(t *testing.T) {
	reg := &providerRegistry{
		served: map[string][]string{"mlx": {"qwen2.5-coder:3b"}, "ollama": nil},
	}
	models := reg.announcedModels([]string{"granite3-moe:3b", "gemma4:31b-cloud", "qwen2.5-coder:3b"})
	want := map[string]bool{"granite3-moe:3b": true, "qwen2.5-coder:3b": true}
	if len(models) != len(want) {
		t.Fatalf("got %v, want exactly %v", models, want)
	}
	for _, m := range models {
		if !want[m] {
			t.Fatalf("unexpected model %q (or cloud tag not dropped)", m)
		}
	}
}

// The image half of the announcement: only mflux-family providers, sorted, and
// not polluted by chat tags — the labelled list clients consume instead of
// guessing among the flat models.
func TestAnnouncedImageModelsKeepsImageProvidersOnly(t *testing.T) {
	reg := &providerRegistry{
		served: map[string][]string{
			"mflux":        {"flux-dev"},
			"mflux-zimage": {"z-image-turbo"},
			"ollama":       nil,
		},
		providerKinds: map[string]string{"mflux": "mflux", "mflux-zimage": "mflux", "ollama": "ollama"},
	}
	got := reg.announcedImageModels()
	if len(got) != 2 || got[0] != "flux-dev" || got[1] != "z-image-turbo" {
		t.Fatalf("announcedImageModels = %v, want [flux-dev z-image-turbo]", got)
	}
}

func TestLoadProvidersRegistersMfluxAndAnnouncesImageGeneration(t *testing.T) {
	cfg := `{
		"default_provider": "ollama",
		"providers": [
			{"name": "ollama", "kind": "ollama", "endpoint": "http://localhost:11434"},
			{"name": "mflux", "kind": "mflux", "endpoint": "http://127.0.0.1:8899"}
		],
		"models": [
			{"name": "flux-dev", "provider": "mflux"}
		]
	}`
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(path, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}

	reg, provs, err := loadProviders(path)
	if err != nil {
		t.Fatalf("loadProviders: %v", err)
	}
	if _, ok := provs["mflux"].(*MfluxProvider); !ok {
		t.Errorf("mflux provider not registered as *MfluxProvider")
	}
	if !contains(reg.announcedCapabilities(), "image_generation") {
		t.Errorf("announcedCapabilities missing image_generation: %v", reg.announcedCapabilities())
	}
	models := reg.announcedModels(nil)
	if len(models) != 1 || models[0] != "flux-dev" {
		t.Errorf("announcedModels = %v, want [flux-dev]", models)
	}
}

// The stable-diffusion.cpp engine shares the mflux image body contract, so
// kind "sdxl" registers as the same provider type under its own kind name, and
// the announcement must list its models as image models beside the mflux zoo —
// otherwise control would route to this gateway for sdxl pins and the image
// registry would not know they exist.
func TestLoadProvidersRegistersSdxlKind(t *testing.T) {
	cfg := `{
		"default_provider": "ollama",
		"providers": [
			{"name": "ollama", "kind": "ollama", "endpoint": "http://localhost:11434"},
			{"name": "sdxl", "kind": "sdxl", "endpoint": "http://127.0.0.1:8902"}
		],
		"models": [
			{"name": "sdxl-base", "provider": "sdxl"}
		]
	}`
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(path, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}

	reg, provs, err := loadProviders(path)
	if err != nil {
		t.Fatalf("loadProviders: %v", err)
	}
	if _, ok := provs["sdxl"].(*MfluxProvider); !ok {
		t.Errorf("sdxl provider not registered as *MfluxProvider")
	}
	if !contains(reg.announcedCapabilities(), "image_generation") {
		t.Errorf("announcedCapabilities missing image_generation: %v", reg.announcedCapabilities())
	}
	imgs := reg.announcedImageModels()
	if len(imgs) != 1 || imgs[0] != "sdxl-base" {
		t.Errorf("announcedImageModels = %v, want [sdxl-base]", imgs)
	}
	models := reg.announcedModels(nil)
	if len(models) != 1 || models[0] != "sdxl-base" {
		t.Errorf("announcedModels = %v, want [sdxl-base]", models)
	}
}

// Both image engines echo the seed the run actually used; the response must
// carry it beside the image so a client can reproduce the picture by resending
// it — the seed the client sent was only a request, and the engine may have
// replaced it with its own.
func TestImageProviderReturnsTheSidecarSeed(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"aW1hZ2U=","seed":424242,"width":512,"height":512,"seconds":1.0}`))
	}))
	defer sidecar.Close()

	p := &MfluxProvider{Endpoint: sidecar.URL}
	res, err := p.GenerateImage(context.Background(), ImageRequest{Prompt: "a cat", Size: "512x512"})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if len(res.Data) != 1 || res.Data[0].Seed != 424242 {
		t.Errorf("seed did not reach the response: %+v", res.Data)
	}
}

// An error from the sd engine must name the engine's kind, not mflux's —
// otherwise a failed SDXL job reads as an mflux failure on the client's side.
func TestSdxlProviderErrorsNameTheKind(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer sidecar.Close()

	p := &MfluxProvider{Endpoint: sidecar.URL, name: "sdxl"}
	_, err := p.GenerateImage(context.Background(), ImageRequest{Prompt: "a cat", Size: "512x512"})
	if err == nil {
		t.Fatal("GenerateImage: want an error")
	}
	if !strings.Contains(err.Error(), `sdxl: boom`) {
		t.Errorf("error = %q, want an sdxl-prefixed message", err)
	}
}

func TestMfluxProviderGeneratesFromTheSidecar(t *testing.T) {
	var sawPath string
	var sawBody map[string]interface{}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&sawBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"aW1hZ2U=","seed":1,"width":512,"height":512,"seconds":1.0}`))
	}))
	defer sidecar.Close()

	p := &MfluxProvider{Endpoint: sidecar.URL}
	res, err := p.GenerateImage(context.Background(), ImageRequest{Prompt: "a cat", Size: "512x512"})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if sawPath != "/generate" {
		t.Errorf("path = %q, want /generate", sawPath)
	}
	if sawBody["prompt"] != "a cat" {
		t.Errorf("prompt = %v, want 'a cat'", sawBody["prompt"])
	}
	if sawBody["width"] != float64(512) || sawBody["height"] != float64(512) {
		t.Errorf("size not translated to width/height: %v", sawBody)
	}
	if len(res.Data) != 1 || res.Data[0].B64JSON != "aW1hZ2U=" {
		t.Errorf("unexpected response: %+v", res)
	}
}

func TestMfluxProviderEditsFromTheSidecar(t *testing.T) {
	var sawPath string
	var sawBody map[string]interface{}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&sawBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"aW1hZ2U=","seed":1,"width":512,"height":512,"seconds":1.0}`))
	}))
	defer sidecar.Close()

	p := &MfluxProvider{Endpoint: sidecar.URL}
	res, err := p.EditImage(context.Background(), ImageRequest{Prompt: "a cat", Image: "aW1hZ2U=", Size: "512x512"})
	if err != nil {
		t.Fatalf("EditImage: %v", err)
	}
	if sawPath != "/edit" {
		t.Errorf("path = %q, want /edit", sawPath)
	}
	if sawBody["init_image"] != "aW1hZ2U=" {
		t.Errorf("init_image = %v, want the source image", sawBody["init_image"])
	}
	if len(res.Data) != 1 || res.Data[0].B64JSON != "aW1hZ2U=" {
		t.Errorf("unexpected response: %+v", res)
	}
}

func TestMfluxProviderEditRejectsMask(t *testing.T) {
	// A non-empty Mask must fail in runImage before any sidecar call, so the
	// sidecar must never be reached. Assert both: an error is returned AND zero
	// requests were made (a connection-refused error alone would not prove the
	// mask was rejected rather than silently dropped).
	var calls int
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer sidecar.Close()

	p := &MfluxProvider{Endpoint: sidecar.URL}
	_, err := p.EditImage(context.Background(), ImageRequest{Prompt: "a cat", Image: "aW1hZ2U=", Mask: "bWFzaw=="})
	if err == nil {
		t.Fatal("EditImage with a Mask must fail, not silently drop it")
	}
	if calls != 0 {
		t.Errorf("sidecar called %d times, want 0 — the mask must be rejected before any request", calls)
	}
}

func TestImageRequestDecodesConditioningExtension(t *testing.T) {
	// The exact wire shape img-gen's Phase B client sends: conditioning
	// wrapped under "extension", strengths optional. This is the decode that
	// previously swallowed the field wholesale — a missing tag here is the
	// original silent-drop bug coming back.
	raw := `{"model":"sdxl-illustrious","prompt":"a cat",
	          "extension":{"control":{"net":"pose","image":"Yw==","strength":0.5},
	                       "ip_adapter":{"image":"aQ=="}}}`
	var img ImageRequest
	if err := json.Unmarshal([]byte(raw), &img); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if img.Extension == nil || img.Extension.Control == nil || img.Extension.IPAdapter == nil {
		t.Fatalf("extension not decoded: %+v", img.Extension)
	}
	c := img.Extension.Control
	if c.Net != "pose" || c.Image != "Yw==" || c.Strength == nil || *c.Strength != 0.5 {
		t.Errorf("control = %+v", c)
	}
	ia := img.Extension.IPAdapter
	if ia.Image != "aQ==" || ia.Strength != nil {
		t.Errorf("ip_adapter = %+v, want image decoded and strength absent", ia)
	}
}

func TestMfluxProviderRelaysConditioningToTheSidecar(t *testing.T) {
	// The relay contract: the client's "extension" wrapper becomes top-level
	// control_net/ip_adapter on the sidecar body — the shape the sidecar and
	// direct mode both speak. The wrapper itself must not leak through.
	var sawBody map[string]interface{}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&sawBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"aW1hZ2U=","seed":1,"width":512,"height":512,"seconds":1.0}`))
	}))
	defer sidecar.Close()

	half, full := 0.5, 0.8
	p := &MfluxProvider{Endpoint: sidecar.URL}
	_, err := p.GenerateImage(context.Background(), ImageRequest{
		Prompt: "a cat", Size: "512x512",
		Extension: &ImageExtension{
			Control:   &ImageControl{Net: "depth", Image: "Yw==", Strength: &half},
			IPAdapter: &ImageIPAdapter{Image: "aQ==", Strength: &full},
		},
	})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if _, leaks := sawBody["extension"]; leaks {
		t.Errorf("the client's extension wrapper leaked to the sidecar: %v", sawBody)
	}
	cn, ok := sawBody["control_net"].(map[string]interface{})
	if !ok {
		t.Fatalf("control_net missing from sidecar body: %v", sawBody)
	}
	if cn["net"] != "depth" || cn["image"] != "Yw==" || cn["strength"] != float64(0.5) {
		t.Errorf("control_net relay = %v", cn)
	}
	ia, ok := sawBody["ip_adapter"].(map[string]interface{})
	if !ok {
		t.Fatalf("ip_adapter missing from sidecar body: %v", sawBody)
	}
	if ia["image"] != "aQ==" || ia["strength"] != float64(0.8) {
		t.Errorf("ip_adapter relay = %v", ia)
	}
}

func TestMfluxProviderConditioningOmitsAbsentStrengthAndEdit(t *testing.T) {
	// Absent pointers must stay absent — an omitted strength selects the
	// sidecar's 0.7/0.6 defaults, and a forwarded "0" would defeat that, and a
	// nil reference must not mint an empty key. Edit requests never take
	// conditioning, so even a present Extension stays off the edit body: an
	// mflux sidecar must never see a new key it has no contract for.
	var sawBody map[string]interface{}
	sawPath := ""
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		// A fresh map per request: json.Decode merges into a non-nil map, and
		// a merged generate body would masquerade as edit leakage.
		b := map[string]interface{}{}
		json.NewDecoder(r.Body).Decode(&b)
		sawBody = b
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"image":"aW1hZ2U=","seed":1,"width":512,"height":512,"seconds":1.0}`))
	}))
	defer sidecar.Close()

	p := &MfluxProvider{Endpoint: sidecar.URL}
	req := ImageRequest{Prompt: "a cat", Size: "512x512",
		Extension: &ImageExtension{
			Control: &ImageControl{Net: "pose", Image: "Yw=="},
		},
	}
	if _, err := p.GenerateImage(context.Background(), req); err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	cn, _ := sawBody["control_net"].(map[string]interface{})
	if cn["net"] != "pose" || cn["image"] != "Yw==" {
		t.Errorf("control_net relay = %v", cn)
	}
	if _, has := cn["strength"]; has {
		t.Errorf("control_net.strength forwarded despite absent pointer: %v", cn)
	}
	if _, has := sawBody["ip_adapter"]; has {
		t.Errorf("ip_adapter present with nil reference: %v", sawBody)
	}

	if _, err := p.EditImage(context.Background(), req); err != nil {
		t.Fatalf("EditImage: %v", err)
	}
	if sawPath != "/edit" {
		t.Fatalf("path = %q, want /edit", sawPath)
	}
	for _, key := range []string{"control_net", "ip_adapter", "extension"} {
		if _, has := sawBody[key]; has {
			t.Errorf("edit body carries %q — conditioning is generate-mode only: %v", key, sawBody)
		}
	}
}
