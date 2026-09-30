package main

import (
	"os"
	"path/filepath"
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
