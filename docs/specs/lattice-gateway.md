# Spec: Lattice Gateway (Phase 3)

**Status:** Draft
**Host:** Mac (Apple M1 16GB)

The Lattice Gateway is the execution arm for all local inference. It translates `inference.v1` requests into Ollama calls and manages the local hardware resources to protect the system's health.

## 1. Core Responsibilities

1. **Protocol Implementation**: Serve the `inference.v1` API.
2. **Ollama Adaptation**: Translate `inference.v1` requests into Ollama's `/api/chat` or `/v1/chat/completions` calls.
3. **Memory-Aware Concurrency (Swap Avoidance)**: prevent memory over-subscription to protect the Mac's SSD from swap thrashing.
4. **Telemetry**: Track actual local latency, token throughput, and memory pressure.

## 2. Swap Avoidance Strategy

The Mac has 16GB of unified memory. Running multiple LLMs or very large contexts can trigger swap.

**The "No-Swap" Guard**:
- **Resident Model Set**: The Gateway should ideally pin a set of common models (e.g., `granite4:3b` as main) to avoid constant unloading/reloading (the `OLLAMA_MAX_LOADED_MODELS=1` issue).
- **Concurrency Limit**: Maintain a hard limit on concurrent local requests based on the active model's memory footprint.
- **Pressure Signal**: If the system reports high memory pressure (or if the calculated memory usage of active requests exceeds a threshold), the Gateway must:
  - Queue new requests.
  - Or return `429 Too Many Requests` to signal the Control plane to reroute to cloud.

## 3. Implementation Logic

When a request arrives from the Control plane:

1. **Identify Model**: Get the mapped resident model name.
2. **Check Memory Budget**:
   - Calculate: `(Number of active requests * Avg context size) + Model size`
   - If `Estimated Memory > Budget` $\rightarrow$ Queue/Reject.
3. **Execute**: Call Ollama.
4. **Telemetry**: Record wall-clock time, token count, and peak memory usage.
5. **Return**: Standard OpenAI response.

## 4. Implementation Plan

- **Language**: Go.
- ** API**: HTTP server on `:8081`.
- **Exit Test**:
  - Send a request from the Control plane $\rightarrow$ Gateway executes on Mac $\rightarrow$ return result.
  - Simulate memory pressure $\rightarrow$ Gateway rejects/queues request.
  - Measure and record actual resident latency (warm vs cold).
