# Spec: Provider Abstraction (Phase 8)

**Status:** Draft
**Host:** Mac (Lattice Gateway)

Currently, the Lattice Gateway is a direct proxy to Ollama. Phase 8 introduces a provider abstraction layer, allowing the Gateway to route requests to different inference engines (e.g., Ollama, vLLM, TGI) without changing the `inference.v1` protocol.

## 1. Provider Interface

The Gateway will define a `Provider` interface:

```go
type Provider interface {
    Execute(req Request) (Response, error)
    GetCapability() Capability
}
```

## 2. Implementation: Ollama Provider

The current proxy logic will be encapsulated into an `OllamaProvider`. It will handle:
- Mapping the requested model to the Ollama-specific API.
- Handling Ollama-specific error codes.

## 3. Provider Registry

The Gateway will maintain a registry of active providers. The routing decision from the Control plane will now include a `provider_hint` or the Gateway will map the `model_name` to a provider.

## 4. Implementation Plan

1. **Define Provider Interface**: Create the interface in the Gateway codebase.
2. **Encapsulate Ollama**: Move current proxy logic into `OllamaProvider`.
3. **Configurable Providers**: Add a `providers.yaml` to the Gateway to map models to providers.
4. **Exit Test**:
   - Configure one model to use `OllamaProvider`.
   - Mock a second provider (e.g., a simple "MockProvider" that returns a static string).
   - Verify the Gateway routes to the correct provider based on the model name.
