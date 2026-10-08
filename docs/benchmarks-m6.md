# M6 benchmark log

Model measurements on the M6 gateway (Apple M6, 32 GB unified memory, 12-core,
Metal 4, 170 GB/s). Protocol matches `docs/measurements.md`: same
prompt, `num_ctx 2048`, `num_predict 128`, `stream=false`, warm decode taken as
`eval_count / eval_duration` on the second (resident) call. Cold call includes
model load. `size_vram` and swap read from `/api/ps` + `sysctl vm.swapusage`.

Goal: prove which models the 32 GB M6 hosts that the 16 GB M1 cannot, and pick a
set covering MoE / dense / vision / image-gen across ollama + MLX + mflux.

## gpt-oss:20b — MoE (done)

MoE 20.9 B total / 3.6 B active, MXFP4, 12.7 GB weights, 131072 native context.

| metric | cold | warm |
|---|---|---|
| model load | 11.43 s | 0 s |
| prefill (135 tok) | 278 tok/s | 5066 tok/s (prompt-cache hit) |
| decode (128 tok) | 39.88 tok/s | **39.34 tok/s** |

- `size_vram` 12.7 GB; **swap 0.00 MB**; system free 52% (model resident).
- Contrast with M1 (16 GB): same model previously hit ~11 GB RSS and swap-thrashed.
  On M6 it fits entirely in unified memory and decodes at a steady ~39 tok/s.
- Verdict: **host on M6**. The canonical "doesn't fit on 16 GB" MoE.

## qwen3.8:27b — flagship hybrid (done)

27 B, Gated-DeltaNet hybrid (near-dense), 17.3 GB weights, "needs 24 GB VRAM".

| metric | cold | warm |
|---|---|---|
| model load | 6.87 s | 0 s |
| prefill | 81 tok/s | 467 tok/s |
| decode (128 tok) | 17.97 tok/s | **17.62 tok/s** |

- `size_vram` 17.3 GB; **swap 0.00 MB**; system free 31% (model resident).
- Decode ~17.6 tok/s ≈ half the MoE's rate: all 27 B params active per token vs
  3.6 B for gpt-oss. Fits on 32 GB with ~10 GB headroom; would exceed 16 GB M1.
- Verdict: **host on M6** (32 GB-only flagship).

## qwen3-vl:8b — vision (done)

Vision-language model, 5.5 GB weights. Test image: 64×64, red square center,
blue square top-left, white background (see `test.png`).

| metric | cold | warm |
|---|---|---|
| model load | 8.91 s | 0 s |
| prefill (incl. ~1k image tokens) | 318 tok/s | prompt-cache hit |
| decode | 29.29 tok/s | **~28 tok/s** |

- Correctly answered: "blue square top-left, red square center, white background".
  Vision pipeline works end-to-end (image → ~1k vision tokens → answer).
- `size_vram` 5.5 GB — fits both M1 (16 GB) and M6. Not M6-exclusive; placement is
  a routing decision, not a capacity one.

## Cross-cutting findings

- **All three models are "thinking" (reasoning) models** — Ollama 0.35 returns a
  `thinking` field ahead of `response`. `ollama show gpt-oss:20b` declares the
  `thinking` capability; qwen3.8 and qwen3-vl both emit a reasoning trace.
  - qwen3.8:27b is a heavy thinker: 256 `num_predict` was still not enough to
    finish reasoning (1k+ chars, `done_reason: length`, no answer emitted).
  - qwen3-vl:8b at 256 completes: ~337–548 thinking tokens, then a correct answer.
  - Routing implication: thinking adds token overhead + time-to-first-answer
    latency. If a non-thinking variant is wanted for latency-sensitive routes,
    that is a separate model selection question.
- **Memory ceiling**: 12.7 + 17.3 + 5.5 = 34.5 GB > 32 GB, so the three cannot be
  resident simultaneously (Ollama evicts). Two big + one small does not co-exist.
  The gateway's `slots` is now *dynamic* (model-size aware, recomputed each health
  poll from free memory vs. `/api/tags` sizes, capped by `LATTICE_GATEWAY_MAX_SLOTS`):
  with a 26b-class model resident (~18 GB RSS leaves ~4 GB usable) it reports
  `slots: 1`; once the big model evicts and memory frees, the count rises and several
  small models can serve concurrently. The `MemoryBudgeter` remains the per-request
  backstop that rejects an individual load overrunning the safety margin.

## z-image-turbo — image-gen (done)

Z-Image-Turbo (`Tongyi-MAI/Z-Image-Turbo`), open (not gated). Runtime is **mflux**
(MLX), via the dedicated `mflux-generate-z-image-turbo` entry point.

| metric | cold | warm |
|---|---|---|
| model load + 9 steps | 93.73 s | 58.80 s |
| per-step (steady state) | ~11 s first, → ~5.4 s | ~5.4 s |
| **Peak MLX memory** | **26.73 GB** | 26.73 GB |

- Output: valid 512×512 PNG, 98 KB. 9 inference steps (turbo).
- **26.73 GB peak MLX memory** — exceeds the 16 GB M1; another M6-exclusive model.
  This is the heaviest of the four classes measured.
- **Gotcha (root-caused here):** Z-Image is *not* FLUX. It has a single text
  encoder (`text_encoder/`, 3 safetensors shards) rather than FLUX's dual
  `text_encoder` + `text_encoder_2` (CLIP-L + T5-XXL). Invoking it through the
  FLUX CLI (`mflux-generate --model z-image-turbo`) makes the loader treat the
  repo as FLUX and die on the missing `text_encoder_2`. Correct entry point is
  `mflux-generate-z-image-turbo` (same for `-qwen`, `-flux2`, `-fibo`, etc. —
  each architecture has its own CLI).

## Cross-cutting finding (image-gen)

- mflux is a separate provider from Ollama — it does **not** appear in
  `/api/tags`. Serving image-gen through the lattice gateway means exposing mflux
  as a second gateway provider, not just pulling an Ollama model.

---

# New-model benchmarks (cutover round 2, 2026-10-03)

Goal: cover the remaining capability categories — abliterated text, abliterated
vision, embeddings, and speech (STT/TTS) — and prove which of them the M6 hosts.
Text/vision decode rates below use `num_predict 128` (text) / natural stop
(vision), `eval_count / eval_duration`, `stream=false`.

## dolphin3-abliterated — abliterated text (done)

`huihui_ai/dolphin3-abliterated`, 8.0 B, Q4_K_M, 4.9 GB weights. Uncensored /
abliterated variant of Dolphin 3.0 (Llama-3.1-8B base).

| metric | cold | warm |
|---|---|---|
| model load | 1.82 s | 0.0007 s |
| prefill (78 tok) | 78 tok / 0.223 s | 77/78 prompt-cache hit |
| decode (128 tok) | 31.96 tok/s | **31.77 tok/s** |

- `size_vram` 4.9 GB; family `llama`, context 2048 (as benchmarked).
- Coherent long-form answer to the MoE-vs-dense tradeoff prompt; ~32 tok/s is
  typical 8B/Q4 on Metal. Fits the 16 GB M1 too — not M6-exclusive.
- Verdict: **text/chat default candidate** (uncensored), routes by priority.

## qwen2.5-vl-abliterated:7b — abliterated vision (done)

`huihui_ai/qwen2.5-vl-abliterated:7b`, 8.3 B, Q4_K_M, 5.5 GB weights. Abliterated
Qwen2.5-VL-7B (vision + text). Test image: `test.png` (red + blue squares).

| metric | cold | warm |
|---|---|---|
| model load | 3.04 s | 0.0007 s |
| prefill (1063 tok, incl. image) | 436 tok/s | 1062/1063 prompt-cache hit |
| decode (60 tok) | 33.1 tok/s | **32.7 tok/s** |

- Correctly described the image ("blue square top-left, red square bottom-right").
  Vision pipeline works end-to-end on the abliterated model.
- `size_vram` 5.5 GB — fits both M1 and M6; placement is a routing decision.
- Verdict: **vision default candidate** (uncensored).

## bge-m3 — embeddings (done)

`bge-m3` (Ollama), 1.2 GB, 1024-dim dense embeddings.

| metric | cold | warm |
|---|---|---|
| single short-text embed | 1.24 s (incl. load) | **0.031 s** |

- 31 ms warm embed is plenty fast for a retrieval/indexing path.
- Verdict: **embeddings provider** on the M6.

## kokoro-82M — TTS (done)

`mlx-community/Kokoro-82M-bf16` via `kokoro-mlx` (MLX), run from
`~/lattice-speech/.venv`. Library-only — no CLI.

| metric | value |
|---|---|
| cold load (cached) | 0.45 s |
| first synth (incl. spacy `en-core-web-sm` + phonemizer init) | ~2 s (one-time) |
| warm synth | **~105 ms** |

- Emits 24 kHz WAV (`test-24k.wav`); resample to 16 kHz (`test-16k.wav`) via
  `afconvert` for the STT path.
- Verdict: **TTS provider**. Needs a gateway "speech" provider kind (no Ollama /
  mflux CLI — it's a Python library).

## whisper-small-mlx — STT (done)

`mlx-community/whisper-small-mlx` via `mlx-whisper` (MLX), `mlx_whisper` CLI.

| metric | value |
|---|---|
| transcribe 8 s audio (warm, cached model) | **~1.05 s** (≈7.6× realtime) |

- Transcript: "The Lattice project roots local inference requests…" — **one word
  error** ("roots" vs the source "routes"); expected for whisper-*small*.
- **Dependency gotcha:** `mlx_whisper` shells out to `ffmpeg` to decode audio; the
  M6 had none (no brew, no sudo). Fixed with a static ffmpeg via the
  `imageio-ffmpeg` wheel, symlinked to `~/bin/ffmpeg` (v7.1).
- Verdict: **STT provider**; same "speech" gateway kind as TTS.

## FLUX.1-schnell — image-gen (done)

`black-forest-labs/FLUX.1-schnell`, the gated Apache-2.0 4-step distilled FLUX.
Unblocked with a read-scoped HF token (`hf auth login` → token `m6-lattice`), pulled
via `mflux` and baked to 4-bit.

| metric | value |
|---|---|
| baked model size | **9.0 GB** (`mflux-save --model schnell --quantize 4`) |
| 512×512, 4 steps | **~13.7 s wall** (~2.4 s/step after warm-up) |
| 1024×1024, 4 steps, `--vae-tiling` | **~2.5 min wall** (~37 s/step) |
| peak MLX memory | **10.0 GB** |

- **The 4-step distillation pays off on the M6:** ~14 s for a 512² image vs the
  spec's M1 16 GB figure of ~60 s/*step* for 28-step FLUX.1-dev. schnell is the
  clear image-gen default for this host.
- **`--vae-tiling` is mandatory at 1024².** A full-res VAE decode OOMs the Metal
  command buffer (`Insufficient Memory` at `mx.eval(latents)`) even on 32 GB; with
  `--vae-tiling` the same run completes at ~10 GB peak. 512² needs no tiling. The
  sidecar's default is 1024², so `MFLUX_EXTRA_ARGS` carries `--vae-tiling`.
- No `--low-ram` needed (that was a 16 GB-M1 constraint); ~10 GB peak is comfortable
  on 32 GB. The baked 4-bit model loads direct — no on-the-fly fp16 re-read.
- Baked to `~/mflux-models/flux-schnell-4bit`, wired as the mflux sidecar's model
  (`--base-model schnell`). See the mflux-sidecar "stale path" fix for how the
  `uv`-tool migration changed the runtime wiring.

## Round-2 cross-cutting findings

- **Three new provider *shapes* emerged** beyond Ollama `chat`/`embeddings`:
  - `mflux` = image-gen (already a gateway kind).
  - `speech` = kokoro-mlx (TTS) + mlx-whisper (STT) — both MLX Python libs, no
    OpenAI-compatible serving layer; need a new gateway provider kind that shells
    out to the venv.
  - embeddings = `bge-m3` via Ollama (already supported).
- **Abliterated variants run clean** through Ollama and behave like their base
  models on the bench metrics (~32 tok/s at 8B/Q4). The "uncensored" selection is
  a model-name/registry concern, not a runtime one.
- **Memory**: all round-2 text/vision/embedding models are ≤5.5 GB, co-resident
  with headroom on 32 GB; only image-gen (Z-Image ~27 GB) is M6-exclusive.

## Gemma 4 — text + agentic (round 3)

Google's Gemma 4 class (April 2026). Native function-calling, `<|think|>` reasoning
token (on by default), `<|tool_call|>` format, MTP (multi-token-prediction)
speculative decoding. Ollama tags `gemma4:12b`/`26b`/`31b` (min Ollama 0.30.4; M6 has
0.35.1). Benchmarked via Ollama `/api/chat` on the M6, decode at `num_predict=200`
plus a `get_weather` tool-call probe.

| model | params | size | decode | prompt eval | tool-call |
|---|---|---|---|---|---|
| gemma4:12b | 12B dense | 8.0 GB | **32.8 tok/s** | 117 tok/s | `{"location":"Paris"}` ✓ |
| gemma4:26b | 25.2B MoE (3.8B active) | 18 GB | **58.5 tok/s** | 159 tok/s | `{"location":"Paris"}` ✓ |
| gemma4:31b | 30.7B dense | 20 GB | **16.4 tok/s** | 47 tok/s | `{"location":"Paris"}` ✓ |

- **MoE wins on speed:** 26b is the fastest of the three despite being "bigger",
  because only ~3.8B active params run per token. It is the natural agentic pick on
  the M6 — near-60 tok/s with 25B-class quality.
- **Tool-calling works, thinking is on by default.** All three correctly emit a
  structured `get_weather` tool_call with a parsed `{"location":"Paris"}` argument.
  The `<|think|>` reasoning token is `default: true`, so every request pays a
  thinking phase before the tool call — latency on the tool path is higher than bare
  decode, but argument extraction is intact.

### Stability caveat (operational, worth knowing)

Ollama 0.35.1's `llama-server` runner (Gemma 4 uses the new `--spec-type draft-mtp`
path) can **wedge** under memory pressure or after unload/reload churn, then return
an empty-but-`done` response:

```
{"model":"","created_at":"0001-01-01T00:00:00Z","message":{"role":"","content":""},"done":false}
```

Symptoms seen during this round: a model that had just benchmarked fine would later
return zero `eval_count` / empty content (decode *and* tool-call), with the runner
still listed in `ollama ps` at a fraction of its on-disk size (e.g. 31b showing
2.1 GB of 20 GB). This is **not** a tool-calling or model bug — the fix is to
restart the Ollama service:

```
launchctl kickstart -k gui/$UID/com.ollama.ollama
```

After restart all three models benchmarked clean. Note for the registry: don't read
an empty `message.content` + missing `eval_count` as "the model refused" — it's the
runner wedged; retry once, and if it repeats, kickstart.

## FLUX uncensored — schnell + Lustly LoRA, wired (round 4)

The image-gen model is now the **uncensored** combination: the baked 4-bit schnell
(`flux-schnell-4bit`, already on disk) with the Lustly.ai uncensored LoRA
(`shauray/flux-uncensored-lora`) applied at inference time. Registry name
**`flux-uncensored`**, distinct from the M1's `flux-dev` (dev + Lustly) so the two
gateways coexist without a name collision.

| piece | value |
|---|---|
| sidecar | `com.lattice.mflux` LaunchAgent, `0.0.0.0:8899` |
| flags | `--base-model schnell --no-bake-lora --vae-tiling` + `--lora shauray/flux-uncensored-lora 1.0` |
| 512×512, 4 steps | **28.3 s** first run (LoRA download + model load + steps); steady-state ~14 s |
| peak | ~10 GB (schnell bake) — LoRA not baked, applied at inference |

- **`--no-bake-lora` is load-bearing**: mflux's default `--bake-lora` merges the LoRA
  into fp16 (~47 GB) and OOMs even 32 GB. `--no-bake-lora` applies the delta at
  inference, keeping peak ~10 GB.
- **LoRA-on-schnell caveat (recorded, not yet stress-tested):** the Lustly LoRA is
  trained on FLUX.1-**dev**. dev and schnell share an architecture, so it *applies*,
  but the uncensoring effect is calibrated for dev's weights and may be weaker/less
  faithful on the 4-step distilled schnell. dev + Lustly (`flux-dev` on the M1, or a
  future M6 dev bake) is the fallback if schnell+Lustly underperforms.

### Wiring (verified end-to-end)

- `deploy/gateway-providers.json` (M6) gained the `mflux` provider
  (`http://127.0.0.1:8899`) + the `flux-uncensored` model route. The gateway announces
  `image_generation` + `flux-uncensored`; Gemma 4 and the other Ollama models are
  auto-announced via `/api/tags` (no config entry needed).
- Control plane `/capabilities` shows both gateways: `m1-gateway` hosts `flux-dev`,
  `m6-gateway` hosts `flux-uncensored` + Gemma 4 + speech + embeddings + vision.
- `/route` resolves `flux-uncensored` → `m6-gateway` (and `gemma4:26b` → `m6-gateway`),
  `locality: local`. The gateway's `POST /v1/images/generations` returns
  `{data:[{b64_json}]}` from a prompt.

### Operational note — reboot auto-recovery (resolved)

The M6's services are `gui/$UID` LaunchAgents, which only exist while a user is logged
into the console. **Auto-login is configured** for `igor` (`autoLoginUser = igor`,
FileVault off), so a reboot now boots straight into `igor`'s console session and
Ollama, the gateway, the mflux sidecar, speech and Tailscale all auto-start. Verified
across a reboot (2026-10-04): Tailscale reconnects (direct peer to rpi4), the gateway
and Ollama return, and the control plane re-marks `m6-gateway` healthy with no manual
steps.

Notes for the record:

- `igor` is a full admin (group 80) with working sudo — the earlier "igor has no sudo"
  note was wrong. The LaunchDaemon path (`/Library/LaunchDaemons/`, root) remains
  available if auto-login is ever disabled.
- Tailscale (standalone GUI app) is not a `gui` LaunchAgent; it runs via a login-item
  helper (`io.tailscale.ipn.macsys.login-item-helper`) plus a system extension, so it
  shares the same console-login dependency.

## Gemma 4 context ceiling — per-model caps (round 5)

The global context cap (32768) was raised **per-model** for Gemma 4, since its
native context is 262144 but the M6's usable headroom (~22–24 GB after macOS + the
three co-resident lattice services) can't hold the 25B-class KV cache that far.
Each model was loaded via Ollama `/api/chat` at rising `num_ctx` (1-token reply) and
measured by `llama-server` RSS + `sysctl vm.swapusage`.

| model | 32k | 64k | 128k | 256k |
|---|---|---|---|---|
| gemma4:12b (8 GB) | — | — | — | **13.2 GB, clean** |
| gemma4:26b (18 GB) | 19.5 GB ✓ | 20.3 GB ✓ | **21.5 GB, clean** | 23.3 GB, 2.66 GB swap |
| gemma4:31b (20 GB) | — | — | 24.2 GB, 4.2 GB swap | **9.5 GB swap, 134 s load** |

- **262144 does not fit the 25B-class models.** Only the 12B has headroom for it.
  The 31B is the outlier: its 20 GB dense weights start swapping at 128k, and at
  256k it thrashed to 9.5 GB swap over 134 s, tripping the gateway's protective
  memory budgeter (`/health` → "Memory pressure high") — confirming that layer works
  as designed.
- **Per-model caps set** in `gateway-providers.json` (the `context` field on the
  model route): `gemma4:12b` → 262144, `gemma4:26b` → 131072, `gemma4:31b` → 65536.
  Each is the measured no-swap ceiling.
- **Mechanism** (unchanged code): `contextWindow` sizes `num_ctx` to the prompt,
  doubling from 2048 and capping at `modelContextCeiling(model)` — the per-model
  `context` override, else the global 32768. So the cap is a *permission*, not a
  reservation: a short prompt still uses ~2–4k context; only a genuinely huge
  prompt approaches the ceiling. Verified live: a 160k-char prompt sizes `gemma4:26b`
  to `num_ctx 65536` (`ollama ps` CONTEXT), past the old 32768 cap.
- **The announced `max_context` stays 32768** — it is a per-gateway global scalar
  (the `LATTICE_GATEWAY_MAX_CONTEXT` default), min'd across gateways by the control
  plane, and does not express per-model ceilings. The per-model `context` override
  is what actually governs serving; the announcement is informational.

## Multi-model image placement — prequant z-image + qwen-image-2.1 (round 6)

Goal: place the uncensored model zoo across M1 (16 GB) and M6 (32 GB) and wire
every model through the gateway (see `docs/specs/lattice-image-multi-model.md`).
Prequantized HF repos, no baking; measurement as round 4 (mflux's own Peak MLX
line, `sysctl vm.swapusage` deltas).

| model (repo) | host | steps @512² | wall | per-step | peak MLX | swap Δ |
|---|---|---|---|---|---|---|
| Z-Image-Turbo 4-bit (`filipstrand/Z-Image-Turbo-mflux-4bit`) | M6 | 4 | 6 s | 1.37 s | **5.63 GB** | 0 |
| Z-Image-Turbo 4-bit (same) | M1 | 4 | 1:37 / 1:45 | 23.8 s | **5.63 GB** | **0** (3846.19 MB used before and after) |
| Qwen-Image-2.1 4-bit (`OsaurusAI/Qwen-Image-2.1-mflux-4bit`) | M6 | 4 | 9 s | ~2.0 s | **10.00 GB** | 0 |

- **Quantization is the placement lever, again.** The fp16 Z-Image-Turbo measured
  26.73 GB (round above, M6-exclusive); its 4-bit prequant peaks at the same
  5.63 GB on both hosts. The per-step gap (1.37 s on M6 vs 23.8 s on M1) is
  compute-side, not memory-side — the M1 held zero swap delta under `--low-ram`
  while Ollama-era swap residue (3.8 GB) stayed untouched.
- **Qwen-Image-2.1 is M6-exclusive by measurement** (10 GB peak — the 16 GB M1
  cannot take it with anything resident).
- Each architecture's CLI entry point remains the rule from the first
  z-image round (`mflux-generate-qwen-2.1`, `--base-model qwen-image-2.1`,
  `--base-model z-image-turbo` verified with third-party prequant repos).
- End-to-end through the lattice from the Pi: all three image models
  (`z-image-turbo`, `qwen-image-2.1`, `flux-uncensored`) returned valid images
  via the frontend → control → gateway → sidecar chain, control telemetry
  `target:"m6-gateway", locality:"local"`.

## Ollama registry pinning — all resident chat + embedding models through the gateway (round 7)

Goal: pin the six Ollama models in the gateway registry (`models` entries in the
provider config) so each has a `lattice/gateway` reference and announced
`max_context`, and sync the repo template (`deploy/gateway-providers.json`) to
the live config — it had drifted (stale mlx provider, `qwen2.5-coder:3b`,
`lfm2.5-2.6b` entries for hardware no longer in the fleet).

- **New pin this round: `qwen3.8:27b`** with `context: 65536` — the repo's hybrid
  Gated-DeltaNet attention keeps KV small like the lfm2.5 linear-attention case
  (round 5), so the dense-model ceiling was not applied.
- **Already pinned and carried over:** `gemma4:12b` → 262144, `gemma4:26b` →
  131072, `gemma4:31b` → 65536 (round 5 measured no-swap ceilings),
  `huihui_ai/dolphin3-abliterated:latest` (enhance model), and
  `huihui_ai/qwen2.5-vl-abliterated:7b` + `bge-m3:latest` at the default ceiling.
- **Benchmark** (same protocol as round 1/2: fixed two-sentence prompt,
  `max_tokens 64`, single request, gateway OpenAI endpoint; native Ollama follow-up
  call on the resident model for precise decode counters — `eval_count /
  eval_duration`):

| model | gateway wall | native decode | decode tok/s (round 7) | prior round |
|---|---|---|---|---|
| `bge-m3:latest` (embedding, 1024 dims) | 1.14 s | — | — | 0.031 s warm |
| `huihui_ai/qwen2.5-vl-abliterated:7b` | 4.48 s | 38 tok / 1.12 s | 33.9 | 32.7 |
| `gemma4:12b` | 7.05 s | 64 tok / 1.87 s | 34.2 | 32.8 |
| `qwen3.8:27b` | 14.07 s | 64 tok / 3.73 s | 17.2 | 17.6 |
| `gemma4:26b` | 7.70 s | 64 tok / 1.21 s | 52.9 | 58.5 |
| `gemma4:31b` | 11.46 s | 64 tok / 4.28 s | 15.0 | 16.4 |

- **All six entries answered with coherent text through the gateway OpenAI
  endpoint** on the first attempt — no runner-wedge (empty-but-done) instance this
  round; the fallback (`launchctl kickstart -k gui/$UID/com.ollama.ollama`) was not
  needed.
- **Decode speeds are within noise of the prior rounds** — registry pinning is
  pure routing config and adds no serving cost; the gateway wall additionally
  includes slot acquisition, and on a cold model, load time.
- **Announcements** (`/v1/models`) reflect the pins: all six names listed with the
  gateway default `max_context` 32768; the per-model `context` overrides remain
  serving-side ceilings (round 5 mechanism, unchanged).
