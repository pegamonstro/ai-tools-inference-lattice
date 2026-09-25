# Phase 0 — Baseline

Recorded 2026-09-25. Read-only inspection; nothing changed on any host.

## Hosts

| Host | OS | Arch | Kernel | Toolchains |
|---|---|---|---|---|
| rpi4 | Debian 13 (trixie) | aarch64 | 6.18.50+rpt-rpi-v8 | Go 1.24.4, Python 3.13.5, gcc 14.2.0 — **no Rust/Node** |
| Mac (this machine) | macOS | arm64 (Apple M1) | — | `ollama` at `/usr/local/bin/ollama` |
| rpi3 | Alpine Linux v3.24.2 | aarch64 | — | busybox/OpenRC — **no Ollama** |

## Inference surface (model-tier classification)

### Cloud tier — rpi4 Ollama `:cloud` (subscription, fast, max 3 parallel)

`nemotron-3-nano:30b-cloud`, `nemotron-3-ultra:cloud`, `nemotron-3-super:cloud`, `minimax-m3:cloud`, `minimax-m2.7:cloud`, `kimi-k3:cloud`, `kimi-k2.6:cloud`, `kimi-k2.5:cloud`, `kimi-k2.7-code:cloud`, `glm-5.1/5.2/5.3:cloud`, `glm-5.3-flash:cloud`, `deepseek-v4-pro:cloud`, `deepseek-v4-flash:cloud`, `deepseek-v4.1-flash:cloud`, `gemini-3-flash-preview:latest`, `gpt-oss:120b-cloud`, `gemma4:cloud`, `mistral-large-3:675b-cloud`, `qwen3.5:cloud`.

Hermes default cloud model: `gemma4:31b-cloud` (provider `ollama-launch`, api `http://127.0.0.1:11434/v1`).

### Mac-local LLM tier — Ollama (slow, free, unlimited parallel)

`granite4:3b` (main local, 2.1 GB), `gemma3:4b` (3.3 GB), `command-r7b:7b` (5.1 GB), `hermes3:8b` (4.7 GB).

### rpi4 tiny local models — NOT a routing tier (user decision 2026-09-25)

`smollm2:135m`, `smollm2:360m`, `qwen2.5:1.5b`, `nomic-embed-text`, `embeddinggemma`, `solace-tinyllama`. These exist on rpi4's Ollama but are **out of scope**: rpi4 is cloud-only; all local inference (including embeddings) consolidates to the Mac.

### rpi3

No Ollama, no local inference — confirmed. It runs the homelab's service surface (mail 993/995/465/143/110, DNS 53, Postgres 5432, MySQL 3306, Redis 6379, memcached 11211, Samba 445, VNC 5900, ssh 2222). "Security appliance" = it never computes inference; it is not a bare box.

## Local-latency measurement (Mac, granite4:3b primary)

| Model | State | Prompt | Wall | completion_tokens | tok/s |
|---|---|---|---|---|---|
| granite4:3b | **cold** (first call, model load) | short | 9.5s | 3 | load-dominated |
| granite4:3b | warm | short | 0.4s | 3 | 7.8 |
| granite4:3b | warm | ~100-token | 12.7s | 123 | 9.7 |
| gemma3:4b | **cold** | short | 5.9s | 4 | load-dominated |

## Conclusion

**granite4:3b runs ~10 tok/s warm** — a 100-token turn ≈ 13s, a 300-token turn ≈ 30s, a 1000-token turn ≈ 100s. Interactive-viable only for **short replies**; not viable for long generations or long-context.

The observed "30min+/turn" is **not** the steady-state rate; it is the **cold-load / long-context / large-model** case, almost certainly amplified by model-swap thrash (the earlier doctrine flagged `OLLAMA_MAX_LOADED_MODELS=1`, which forces a full unload+reload on every model switch — e.g. alternating `granite4:3b` and `hermes3:8b`).

**Routing implication (confirms the refined model):** cloud is the interactive default. Local serves (a) short local drafts, (b) embeddings, (c) parallel batch work where unlimited concurrency compensates for per-item slowness.

**Gateway lever for Phase 3:** pin a single resident model (`granite4:3b`), avoid model switching, and treat model-load as a first-class scheduling cost.

## Phase 0 exit test

- Repo exists and committed: yes (`git init`; `README.md` charter, `docs/lattice-design.md`, `docs/baseline.md`).
- Three hosts baselined with model-tier classification: yes.
- Local-latency question resolved: yes — warm ≈ 10 tok/s (short-reply viable); 30min+ is cold/long-context/thrash; cloud is the interactive default.
- Deviations from frozen design: none. Read-only throughout.
