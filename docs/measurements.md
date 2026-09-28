# Measurements

The consolidated measurement record for Lattice's local inference. Every figure
here was taken on the Mac gateway (Apple M1, 16 GB unified memory) unless marked
*external*. Earlier figures lived scattered across `docs/baseline.md`, the
specs, and commit messages; this is the single source of truth, updated as new
measurements land.

## Host under test

- **Hardware:** Apple M1, 16 GB unified memory, macOS.
- **Inference:** Ollama (which wraps llama.cpp as its engine), reached through
  the gateway — never directly, by design.
- **Binding constraint:** no swap thrashing. The gateway's memory-pressure gate,
  the single inference slot, and the 32768 context ceiling all exist to keep the
  Mac out of sustained swap (SSD wear), per the project charter.

## Model inventory (local, installed on the Mac)

| Model | Architecture | Weights | Class |
|---|---|---|---|
| `granite4:3b` | dense | 2.1 GB | default local |
| `granite3-moe:1b` | **MoE** (1.3 B) | 0.8 GB | measured 2026-09-28 — fast general (below) |
| `granite3-moe:3b` | **MoE** (3.4 B) | 2.1 GB | measured 2026-09-28 — fast general (below) |
| `sam860/olmoe-1b-7b-0924` | **MoE** (6.9 B total / ~1.3 B active) | 3.0 GB | measured 2026-09-28 (below) |
| `llama3.2:3b` | dense | 2.0 GB | — |
| `gemma3:4b` | dense | 3.3 GB | — |
| `mistral:latest` | dense (7b) | 4.4 GB | — |
| `hermes3:8b` | dense | 4.7 GB | — |
| `qwen2.5-coder:3b` | dense | 1.9 GB | measured 2026-09-28 — coding (below) |
| `qwen2.5-coder:7b` | dense | 4.7 GB | coding |
| `llama3.1:8b` | dense | 4.9 GB | — |
| `command-r7b:7b` | dense | 5.1 GB | — |
| `qwen3:8b` | dense | 5.2 GB | — |
| `mistral-nemo:12b` | dense | 7.1 GB | fits at low context only |
| `gpt-oss:20b` | **MoE** (20.9 B total / 3.6 B active) | 14.4 GB | measured 2026-09-28 — does not fit (below) |
| `embeddinggemma` | embedding | 0.6 GB | `/v1/embeddings` |
| `smollm2:360m` | dense | 0.7 GB | toy |

Everything `:cloud`-suffixed is a 0-byte routing alias (resolved to a cloud
subscription), not a local model.

## Latency (baseline, 2026-09-25)

| Model | State | Prompt | Wall | completion_tokens | tok/s |
|---|---|---|---|---|---|
| `granite4:3b` | cold (model load) | short | 9.5 s | 3 | load-dominated |
| `granite4:3b` | warm | short | 0.4 s | 3 | 7.8 |
| `granite4:3b` | warm | ~100-token | 12.7 s | 123 | 9.7 |
| `gemma3:4b` | cold | short | 5.9 s | 4 | load-dominated |

**Conclusion:** `granite4:3b` runs ~10 tok/s warm. A 100-token turn ≈ 13 s, a
300-token turn ≈ 30 s, a 1000-token turn ≈ 100 s. Interactive-viable only for
short replies; the reported "30 min+/turn" is the cold-load / long-context /
model-swap-thrash case, not the steady state.

## Memory vs context — the binding constraint

Weights are *not* what decides whether a model thrashes; the **KV cache** is,
and it scales with context × layers. Every model below is measured with the
gateway's q8_0 KV cache and dynamic `num_ctx` (2048 → doubling → 32768 cap).

| Model | Context | Resident | Source |
|---|---|---|---|
| `granite4:3b` (2.1 GB weights) | 2048 | 2.2 GB | embedding-reload run |
| `granite4:3b` | 32768 | 9.2 GB | ceiling measurement (§3.5 of agent-surface spec) |
| `granite4:3b` | 65536 | 13 GB, ~2.2 GB swap | **rejected ceiling** |
| `granite4:3b` | 131072 (app default) | ~13 GB, sustained swap | thrash incident (below) |
| `granite4:3b`, reload under Ollama's own defaults (larger ctx + unquantized cache) | — | 12.7 GB | trips the memory margin |

**Read-off:** `granite4:3b` is 2.1 GB of weights but ~7 GB of KV+overhead at
32768 context. A 12 b dense (`mistral-nemo:12b`, 7.1 GB) scales that same
overhead roughly 4×, so it only fits at low context. This is why Lattice caps
context rather than model size — context is the lever that moves memory.

## Context ceiling decision

The ceiling is **32768**, and it is a *measured* decision, not a formatting one:

- 32768 → **9.2 GB** resident, ~40 MB swap.
- 65536 → **13 GB** resident, **~2.2 GB** swap.

The 65536 raise was implemented, measured, and rejected. 32768 is what the
16 GB host can afford; the ceiling is exposed in `/health` (`max_context`) and
`/v1/models` so it is discoverable. It is expected to rise on a larger machine.

## Embedding interleave / reload cost

Interleaving an embedding with a resident chat model evicts and reloads it. The
reload cost was measured at the shape the build loop actually runs (the ~13k-token
harness tax, resolving to the 32768 ceiling):

- At **32768** context (q8_0 cache): reload **2.2 s** (three runs 1.84 / 2.21 /
  2.58 s, median 2.21), resident model at **4.9 GB** vs 2.2 GB at 2048.
- The same reload under Ollama's own defaults (larger context + unquantized
  cache) took **8.8 s** and **12.7 GB** — enough to trip the memory margin.

The capped context and the q8_0 cache are what keep the reload affordable; it is
not a property of the model.

## Swap-thrash incident and resolution (2026-09-27/28)

The recurring thrash was traced to a non-Lattice client: **Ollama.app's own
agent loop** calling `127.0.0.1 POST /v1/messages?beta=true` against local
`granite4:3b` with no `num_ctx`, inheriting the app's 262144 default and
clamping to 131072 → ~13 GB resident → sustained swap. Lattice was never the
offender (the gateway always sends its own capped `num_ctx`).

Resolved by resetting Ollama.app to defaults: the loaded model now reports
`context_length: 8192`, the server binds loopback-only, and the thrash is gone.
The app's loop still fires but is now cheap. DSH and opencode were repointed at
Lattice earlier, closing their bypass.

## MoE (dense vs mixture-of-experts) — measured 2026-09-28

**Goal:** verify whether an MoE model relieves memory pressure on this host
through Lattice, and find the context point where it stays clean.

**Model:** `gpt-oss:20b` (OpenAI GPT-OSS) — `ollama show` reports architecture
`gptoss`, **20.9 B** parameters, 131072 context. 14.4 GB on disk.

**Operational fact first:** the model does *not* live in `ollama serve` (which
stays ~6–17 MB — it is only the router). The weights load into a child
**`llama-server`** process. Any RSS reading of `ollama serve` is the wrong
process; the footprint is `llama-server`'s.

**The decisive test — does mmap relieve the memory?**

| condition | `llama-server` RSS | `size_vram` | host free mem | swap used |
|---|---|---|---|---|
| `num_ctx` 2048 (minimal KV cache) | **11.0 GB** | 11.5 GB | **8 %** | ~7 GB |
| default (uncapped, 131072) | 10.6 GB | 8.3 GB | ~0 % | 10.7 GB |

**It does not.** At the smallest context — where the KV cache is negligible and
the number can only be the *weights* — `llama-server` is still **11 GB
resident** and the host is left at **8 % free memory** with ~7 GB in swap. The
inactive experts did *not* stay paged out. The model effectively requires
~11.5 GB of unified memory, same as any dense model of that size would.

**Why the theory failed here:** the mmap paging relief depends on the weights
being *file-backed and CPU-accessed*. Ollama offloads **all** layers to the GPU
by default on Apple Silicon, and GPU-offloaded weights are resident unified
memory, not pageable file-backed pages — so every expert loads. The external
benchmark that *did* show 0-swap relief (Qwen3.5-35B-A3B on a 16 GB Mac, 17.3
tok/s) pinned `--n-gpu-layers 0`, i.e. **CPU-only** inference, which is exactly
what keeps the weights mmap'd and the inactive experts evictable. Ollama does
not run that way by default, and Lattice's gateway does not (yet) force it.

**At default context it is worse:** firing with no `num_ctx` inherited the
model's 131072 ceiling, took a 152 s cold load, and drove swap to 10.7 GB —
the same uncapped-context thrash shape as the granite incident, only larger.

**Conclusion for what Lattice serves:** `gpt-oss:20b` does **not** fit cleanly
on this 16 GB host through the current path. MoE relieves *compute* here, not
memory; the memory relief is a property of CPU-only inference (`n_gpu_layers=0`)
that neither Ollama's default nor the gateway exercises. A larger host (more
unified memory) or a CPU-only runner would be required to host a 20 B MoE
without thrash. The dense-vs-MoE distinction does **not** change Lattice's
sizing ceiling, which is still set by context and by total resident weights.

## `num_gpu:0` (CPU-only) re-test — measured 2026-09-28

`num_gpu:0` is Ollama's API switch for `--n-gpu-layers 0`. Re-testing
`gpt-oss:20b` through it answers whether the mmap relief is reachable *without*
a second provider. (`granite4:3b` under `num_gpu:0` was also checked: the option
takes effect — `size_vram` → 0.0 — but a small dense model shows nothing,
because every weight is hot every token; there are no inactive experts to page.)

**It engages mmap — confirmed.** With `num_gpu:0`, `llama-server`'s RSS stayed
~600–700 MB (collapsing 1.7 GB → 0.7 GB as it idled) while `/api/ps` reported
`size 13.9 GB, size_vram 0.0`. The 13.9 GB of weights are **file-backed, not
resident** — the same model under GPU offload held 10.5–11 GB RSS. This is the
"inactive experts page from SSD" mechanism actually firing.

**But it still does not fit on 16 GB.** The *load event* reads ~13.9 GB into the
page cache, which forces macOS to push ~5–6 GB of *everything else* (other apps,
and the host the gateway runs on) into swap: swap went 3.4 GB → 9.5 GB and kept
climbing; free memory held at 34 %. The relief shrinks the model's *own*
footprint but does not stop a 13.9 GB file from displacing the rest of a 16 GB
host while it is read in.

**Refined conclusion.** Two distinct facts, and the earlier one still stands:

- Under **GPU offload (default)**, MoE gives no relief — all weights resident,
  11 GB, no mmap.
- Under **`num_gpu:0`**, the mmap relief *is* engaged (weights file-backed,
  ~0.7 GB resident), but a 20 B MoE is still too large for 16 GB to *load without
  displacing the host*. The relief is real and reachable through the existing
  API — **no second provider needed** — but it only becomes comfortable on a
  **larger-RAM host** (32 GB+), where a 13.9 GB mmap'd model leaves the OS and
  apps untouched. The cost there is CPU-only token speed, unmeasured here and
  expected to be low for a 20 B MoE.

## Small MoE vs dense — measured 2026-09-28

**Goal:** pick small MoE models for *general* routing and small dense models
for *coding*, and verify how each behaves on the 16 GB host.

**Protocol:** same prompt, `num_ctx` 2048, `num_predict` 128, `stream=false`,
each model unloaded before the next so RSS is unambiguous. Warm tok/s is
`eval_count / eval_duration` on the second (resident) call.

| Model | Arch | Total params | Native ctx | Weights | Warm tok/s | Resident RSS | `size_vram` |
|---|---|---|---|---|---|---|---|
| `granite3-moe:1b` | MoE | 1.3 B | 4096 | 821 MB | **51.0** | 1.1 GB | 0.9 GB |
| `granite3-moe:3b` | MoE | 3.4 B | 4096 | 2.1 GB | **25.7** | 2.2 GB | 2.2 GB |
| `qwen2.5-coder:3b` | dense | 3.1 B | 32768 | 1.9 GB | 9.3 | 1.8 GB | 2.1 GB |
| `sam860/olmoe-1b-7b-0924` | MoE | 6.9 B | 4096 | 3.0 GB | **18.4** | 3.2 GB | 3.3 GB |

Swap held flat at ~3.1 GB throughout — all four fit without thrash.

**Three conclusions:**

1. **MoE relieves compute, not memory — now reproduced at small scale.**
   `granite3-moe:3b` (3.4 B) runs 25.7 tok/s vs `qwen2.5-coder:3b` (3.1 B dense)
   at 9.3 tok/s, ~2.8× faster at the *same* footprint. But resident RSS ≈
   on-disk size for every model (2.2 GB → 2.2 GB; 3.3 GB → 3.2 GB): all experts
   are resident under Ollama's default GPU offload. This is the same verdict as
   the `gpt-oss:20b` test above, now confirmed on small MoE.

2. **The speed win is the active-vs-total gap.** `olmoe` carries 6.9 B of
   weights but only ~1.3 B are active per token, so it computes at 18.4 tok/s —
   a 7 B model's knowledge at a 3 B model's compute cost. tok/s tracks *active*
   params; RSS tracks *total* params. The two axes are decoupled, which is what
   makes the small-MoE strategy viable here.

3. **The caveat is context, not memory.** All three MoE models are 4096-context
   native; `qwen2.5-coder:3b` is 32768. The MoE models cannot reach Lattice's
   32768 ceiling, so a *general* request needing >4 K context must not land on
   them.

**Routing assignment (config, not code):** *general* → `granite3-moe:3b`
(25.7 tok/s), with `granite3-moe:1b` as the ultra-fast fallback for trivial
queries; *coding* → `qwen2.5-coder:3b` (dense, 32 K context), with
`qwen2.5-coder:7b` as the larger option. The 4 K context ceiling on the MoE
models is the one thing to guard: long-context general requests fall through to
a dense or cloud path.
