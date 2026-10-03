# Spec: Local image generation (uncensored FLUX)

**Status:** Draft
**Scope:** `deploy/mflux-sidecar.py`, `deploy/com.lattice.mflux.plist.in`,
`deploy/hermes/plugins/image_gen/mflux/`, `deploy/install-macos.sh`
**Depends on:** nothing in the chat/embeddings surface — this is a new modality.

> **M6 cutover note (2026-10-03).** This spec was written against the M1 (16 GB,
> FLUX.1-dev + Lustly LoRA, a manual `~/lattice-mflux/venv`). On the M6 the runtime
> moved to a `uv tool`: the sidecar is stdlib-only and runs under `/usr/bin/python3`,
> `mflux-generate` is the shim at `~/.local/bin/mflux-generate`, and the baked model
> is `mflux-save --model schnell --quantize 4 --path ~/mflux-models/flux-schnell-4bit`
> (schnell is 4-step and Apache-2.0, not the gated non-commercial dev). The stale
> `~/lattice-mflux/venv` path and `flux-dev-4bit` bake referenced below no longer exist;
> the corrected values are in `deploy/install-macos.sh` and
> `deploy/com.lattice.mflux.plist.in`. On the M6 "uncensored" means **schnell +
> Lustly LoRA** (`--lora shauray/flux-uncensored-lora`, applied at inference, not
> baked), registry name `flux-uncensored`; dev + Lustly is the fallback (see §11).

---

## 1. Purpose

Lattice routes text: chat, embeddings, and tool-calling over an OpenAI-shaped
contract. It has no concept of an image, and it has no diffusion runtime. This spec
adds the *other* half the user asked for — local, uncensored image generation and
editing — without bending the gateway into a shape it was never built for.

The model is FLUX.1-dev with the **Lustly.ai uncensored LoRA** (`shauray/flux-uncensored-lora`), served by **mflux** (the
MLX-native FLUX runtime) on the Mac, the only host with MLX and enough RAM. Hermes
reaches it through its existing `image_gen` plugin system.

---

## 2. Why not the gateway

**Superseded 2026-10-01 by [lattice-image-gateway.md](lattice-image-gateway.md)**,
which routes image generation *through* the gateway via an OpenAI Images route. This
section is retained as the historical record of the original decision; its conclusion
is reversed.

The first question to answer is *why this does not go through Lattice's gateway*.
The answer is a modality boundary, not a workaround:

- The gateway's public contract is OpenAI chat + embeddings. A diffusion model has
  no `messages`, no `tools`, no `stream` — the request and response shapes do not
  overlap. Retrofitting image gen into the gateway would add a second, unrelated
  contract to the one thing invariant 1 says must stay singular.
- Hermes **already owns** an image-generation plugin surface (`plugins/image_gen/`,
  `agent.image_gen_provider.ImageGenProvider`), with text-to-image *and* editing in
  one `generate(prompt, image_url=…)` call. The correct integration is a *plugin*
  there, not new plumbing in Lattice.
- The mflux runtime is MLX-native and resident on the Mac. The gateway already
  speaks to MLX (the `mlx` provider) — but only for chat. A diffusion runtime is a
  sibling concern, addressed by a sibling process (a sidecar), exactly as Ollama and
  mlx-lm are sibling runtimes behind the gateway.

So: **Lattice is untouched.** Image gen rides the existing Hermes `image_gen`
mechanism, backed by a local sidecar on the Mac. "Through the Lattice" in the
original request resolves to "on the same homelab hardware, with the same
no-external-service stance" — not "through the gateway's router".

---

## 3. The model

The request started as "the uncensored SDXL model" and landed on FLUX. The path:

- **SDXL / Pony rejected.** mflux supports only FLUX/FLUX.2/Z-Image/Krea/Qwen-Image
  families — no SDXL, no Pony. (An earlier survey misread a third-party hardware-fit
  table as mflux model support; the mflux README is authoritative.) SDXL would need a
  different runtime (ComfyUI / diffusers+PyTorch), which pulls in a second, heavier
  stack for one model.
- **FLUX.1-dev** (`black-forest-labs/FLUX.1-dev`) is the base — gated on the Hub, so
  it needs a Hugging Face token to pull. ~12 GB fp16; **4-bit** (`--quantize 4`) is
  the lever that fits 16 GB.
- **The uncensored LoRA** is `shauray/flux-uncensored-lora` — the Lustly.ai
  uncensored LoRA, a ~344 MB `flux_lustly-ai_v1.safetensors`, applied via
  `--lora <repo> 1.0`. (The originally selected `enhanceaiteam/Flux-uncensored-v2`
  was rejected at download time: Hugging Face had disabled the file — "Access to this
  resource is disabled" — a content-policy takedown, so an accessible equivalent was
  substituted.)

This is the "uncensored" the user asked for, and the user stated the use case plainly
(artwork, no foul intent). Nothing here is a misuse concern: it is a local model run
on the user's own hardware for the user's own images.

---

## 4. Architecture

```
Hermes (Pi, control plane)
  └─ image_gen tool ──► mflux plugin (deploy/hermes/plugins/image_gen/mflux/)
        │  POST /generate  /edit   (tailnet)
        ▼
mflux sidecar (Mac)   ──► mflux-generate ──► FLUX.1-dev + uncensored LoRA (4-bit)
  deploy/mflux-sidecar.py          (subprocess)
```

- The plugin maps a Hermes `generate(prompt, aspect_ratio, image_url=…)` call to a
  sidecar POST; it owns the Hermes-side concerns (aspect→pixels, source-image fetch,
  result caching).
- The sidecar owns the mflux-side concerns (CLI assembly, single-flight, base64
  round-trip) and is deliberately thin.
- The sidecar is **not** behind the gateway and **not** announced by it. It is a new
  listener, which is the one place this spec presses against invariant 5 (no new
  infrastructure) — addressed honestly in §9.

---

## 5. Components

### 5.1 The sidecar — `deploy/mflux-sidecar.py`

A stdlib `http.server` wrapper. `GET /health`, `POST /generate` (text-to-image),
`POST /edit` (image-to-image). All config is environment-driven (`MFLUX_BIN`,
`MFLUX_MODEL`, `MFLUX_LORA`, `MFLUX_LORA_SCALE`, `MFLUX_QUANTIZE`, `MFLUX_BIND`,
`MFLUX_PORT`, `MFLUX_EXTRA_ARGS`, `MFLUX_TOKEN`).

Two decisions that need stating:

- **CLI via subprocess, not the Python API.** mflux's Python API is not
  version-stable (the top-level `Flux1` import is gone in current mflux). The CLI is
  stable. The cost is a model reload per request; `run_generation()` is isolated so a
  resident-model backend can replace it later without touching the HTTP layer.
- **Single-flight.** mflux holds the whole model in RAM; concurrent generations would
  OOM a 16 GB host. A second request while one runs gets `409`.

### 5.2 The LaunchAgent — `deploy/com.lattice.mflux.plist.in`

Mirrors `com.lattice.mlx.plist.in`: same `__…__` substitution, `RunAtLoad` +
`KeepAlive`, logs to `__LOG_DIR__/lattice-mflux.{out,err}.log`. `MFLUX_MODEL` is the
baked 4-bit path (a new `__MFLUX_MODEL__` placeholder → `~/lattice-mflux/models/flux-dev-4bit`),
so `MFLUX_QUANTIZE` is empty and `MFLUX_EXTRA_ARGS` is
`--base-model dev --no-bake-lora --low-ram` (the bake requires all three, §6).
`MFLUX_BIND` is `0.0.0.0` because the Pi must reach it over the tailnet.
`install-macos.sh` gains a mflux block that only supervises when the
`~/lattice-mflux/venv/bin/python` exists, and refuses to fight an unmanaged sidecar
on `:8899`.

### 5.3 The plugin — `deploy/hermes/plugins/image_gen/mflux/`

`plugin.yaml` + `__init__.py` implementing `ImageGenProvider` directly (not
`StaticImageGenProvider`, which is built around an API key). Key contract points,
taken from the Hermes source:

- `is_available()` **must not make network calls** (the picker calls it on every
  paint), so it returns whether the sidecar URL is configured — a down sidecar is a
  runtime `connection_error`, not absence.
- `generate()` returns `success_response`/`error_response` dicts and caches the
  result through `save_b64_image`; the source image for an edit is fetched to bytes
  on the Pi (data URL, http URL, or local path) and sent base64, since the Mac cannot
  read the Pi's filesystem.
- `capabilities()` reports `{"modalities": ["text", "image"], "max_reference_images": 1}`
  so the tool schema knows editing is honored.

The plugin files live in the repo (`deploy/hermes/…`) as the source of truth and are
copied to the Pi's Hermes plugin directory by hand;
the copy is a deployment step, not a committed artifact of the Pi.

---

## 6. Constraints

- **16 GB RAM.** 4-bit FLUX.1-dev *fits*, but only baked. On-the-fly `--quantize 4`
  re-reads the 23.8 GB fp16 weights every run and thrashes 16 GB into swap (~100 M
  pageins, 60+ s/step). The fix is `mflux-save --model dev --quantize 4` — a one-time
  bake to a ~9 GB 4-bit model that loads directly. Two flags are then mandatory on
  every generation: `--no-bake-lora` (the default `--bake-lora` merges the LoRA into
  fp16 ~47 GB and OOMs) and `--low-ram` (caps the MLX cache at 1 GB, implies
  `--vae-tiling`, and keeps swap *shrinking* rather than thrashing the SSD — invariant
  7). The sidecar's single-flight lock is the same philosophy as the gateway's
  one-local-inference-slot rule.
- **Speed.** A baked 4-bit FLUX.1-dev is ~60 s/step on this M1 16 GB under load
  (benchmark §10.5), so a 10-step 512² image is ~10 minutes and a 28-step 1024² image
  is over an hour. The subprocess approach adds a model reload per request. Acceptable
  for occasional use; fewer steps, an idle Mac, a resident-model backend (§11), and the
  plugin's `size` preset (`image_gen.mflux.size: small`, ~¼ the pixels) are the
  recorded levers.

---

## 7. Security and secrets

The model is uncensored and the endpoint is image-generation — worth being explicit:

- **Secrets stay out of the repo.** The Hugging Face token is written by
  `huggingface-cli login` to `~/.cache/huggingface/token`, outside the repo, and read
  automatically by mflux/huggingface_hub. It is never committed, never in a plist,
  never in an env example. This matches the repo's existing convention
  (`*.env.example` files carry no values).
- **No auth by default.** The sidecar binds `0.0.0.0` unauthenticated, so anything on
  the LAN/tailnet could consume the Mac's GPU or generate images. For a single-user
  homelab this is the same trust model as the unauthenticated Ollama/MLX listeners
  already in play. If that changes, `MFLUX_TOKEN` on the sidecar plus `image_gen.mflux.token`
  on the Pi add a one-header shared secret — already wired, just unset.
- **No address in the repo.** The sidecar URL is `image_gen.mflux.url` in the Pi's
  config, with no committed default (invariant 8). The plugin refuses to run until it
  is set.

---

## 8. Non-goals

- **Gateway/control-plane routing for images.** ~~Explicitly out: Lattice stays text.~~ **Superseded 2026-10-01** — now in scope, speced in [lattice-image-gateway.md](lattice-image-gateway.md).
- **Video generation** — out of scope; image gen only.
- **SDXL/Pony** — rejected (§3); mflux cannot serve them.
- **A Lattice-native diffusion provider kind** — rejected (§2); the Hermes plugin
  surface already exists and is the correct home.
- **Resident-model (persistent) backend** — deferred (§11); the subprocess path works
  first, and the seam to upgrade it is already drawn.

---

## 9. Invariant check

| invariant | status |
|---|---|
| 1 — one public entry point | **held** — the gateway's contract is unchanged; the sidecar is not a second *Lattice* entry point, it is a Hermes plugin backend |
| 2 — the Mac computes, the Pi routes | held — diffusion computes on the Mac; Hermes decides and calls |
| 3 — `LOCAL_ONLY` never falls back | n/a — no cloud path exists to fall back to |
| 4 — the gateway translates, it does not decide | held — the gateway is untouched |
| 5 — no new infrastructure | **pressed** — the sidecar is a new process/listener. Honest accounting: it is new *local* infrastructure, but it is not Lattice infrastructure (no bus, no inventory, no routing change). Recorded rather than hidden |
| 6 — Bee is not modified | held |
| 7 — protect the Mac's SSD | **held** — single-flight + `--vae-tiling` + quantization are the memory levers; the benchmark (§11) is the acceptance gate |
| 8 — no usernames/hostnames/addresses | **held** — the sidecar URL is config-only, no committed default |
| 9 — telemetry shares one key | n/a — the sidecar is outside Lattice telemetry |

---

## 10. Acceptance

1. `GET /health` on the sidecar returns `200` with the configured model/lora; a
   second concurrent `POST /generate` returns `409` while one is running.
2. `POST /generate` produces a valid PNG from a prompt; `POST /edit` produces a PNG
   from `init_image` + `prompt`; both return `seed` for reproducibility.
3. `install-macos.sh` installs and bootstraps `com.lattice.mflux` when the venv is
   present, and skips it (with a note) when it is not.
4. On the Pi, `image_gen.provider: mflux` plus a configured URL makes the picker list
   Flux Uncensored, and a `generate` call returns a cached image path.
5. **Memory benchmark — the real gate.** Measured on the 16 GB M1: a 512² 10-step
   generation from the baked 4-bit model with `--no-bake-lora --low-ram` completed in
   9:52 (~59 s/step) at a **peak MLX memory of 8.31 GB**, with swap *shrinking* (not
   thrashing). The on-the-fly `--quantize 4` path that §6 rejects thrashed at 60–76
   s/step with swap growing to 9.8 GB and 100 M+ pageins. The winning invocation —
   baked model + `--base-model dev --no-bake-lora --low-ram` — is now the plist
   default (§5.2). A 1024² run was not attempted: at ~60 s/step it would exceed an
   hour, so the acceptance gate is the 512² no-thrash completion above, and 1024² is
   recorded as a speed (not memory) concern.

---

## 11. Open items

- **Bake (done, but manual).** The 4-bit model is produced once by
  `mflux-save --model dev --quantize 4 --path ~/lattice-mflux/models/flux-dev-4bit`
  (no `--lora`, no `--bake-lora`) — a ~9 GB output that `install-macos.sh` points
  `MFLUX_MODEL` at. This is a per-host one-time step, like the gated download, and is
  not something the repo can do for the user: it needs the token-accepted fp16 model
  on disk and ~10 GB of free disk. The 33 GB fp16 cache can be deleted afterwards to
  reclaim disk.
- **Speed (open, accepted).** ~60 s/step on the M1 under load means a 28-step 1024²
  image is over an hour. The recorded levers — fewer steps, running when the Mac is
  idle (the `dsh` jobs peg CPU at loadavg 22), and the resident-model backend below —
  are user choices, not code.
- **Resident-model backend** — replace the per-request subprocess with a persistent
  mflux process behind `run_generation()`, removing the reload cost, once the mflux
  Python API is mapped against a real generation.
- **Uncensored model selection (resolved 2026-10-03).** The M6 serves **schnell +
  Lustly LoRA** as `flux-uncensored`: the baked 4-bit schnell
  (`~/mflux-models/flux-schnell-4bit`, Apache-2.0, 4-step) with
  `shauray/flux-uncensored-lora` applied at inference via `--lora … --no-bake-lora`.
  `--no-bake-lora` is load-bearing: the default `--bake-lora` merges the LoRA into
  fp16 (~47 GB) and OOMs even 32 GB. The LoRA is dev-trained; dev and schnell share a
  FLUX.1 architecture so it applies, but the uncensoring effect is calibrated for
  dev's weights and may be weaker on the 4-step distilled schnell — **dev + Lustly
  is the fallback** (`flux-dev`, currently on the M1) if schnell+Lustly underperforms.
  First-run 512² 4-step measured ~28 s (LoRA download + load + steps). See the M6
  benchmark log (`docs/benchmarks-m6.md`, round 4).
