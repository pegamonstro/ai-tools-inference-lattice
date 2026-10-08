# Spec: Multi-model uncensored image generation (M1 + M6 placement)

**Status:** Implemented
**Scope:** gateway `handleImage` + `MfluxProvider` (sidecar model selection),
`deploy/gateway-providers.json`, `deploy/com.lattice.mflux-zimage.plist.in`,
`deploy/install-macos.sh`, `docs/benchmarks-m6.md`
**Depends on:** the image route (`lattice-image-gateway.md`, implemented), the
mflux sidecar contract (`lattice-image-generation.md`).
**Extends:** [lattice-image-gateway.md](lattice-image-gateway.md) — same routes,
same slot and margin discipline, same telemetry stream.

---

## 1. Purpose

The image gateway routed one model per host: the sidecar served whatever
`MFLUX_MODEL` pointed at, and the gateway sent only `prompt`, `width`
and `height` — the requested `model` field was read for routing
(`resolveModel` → registered name) but never *used* downstream. This spec
extends the same route to serve several diffusion models per host and to place
each model on the host class it fits, with no control-plane change.

The uncensored model zoo this serves (all run on user-owned homelab hardware
for the user's own artwork — the content boundary recorded in
`lattice-image-generation.md` §3 applies unchanged):

- **Z-Image-Turbo 4-bit** (`filipstrand/Z-Image-Turbo-mflux-4bit`) — 6 B,
  **5.63 GB peak MLX** (vs 26.73 GB for the fp16 original, round
  `z-image-turbo` in `docs/benchmarks-m6.md`), ~9 steps, no model-level NSFW
  filter. The quantization is what moves it from M6-exclusive to both hosts.
- **Qwen-Image-2.1 4-bit** (`OsaurusAI/Qwen-Image-2.1-mflux-4bit`) — 7.1 B,
  **10.0 GB peak MLX memory** at 512²: a 16 GB host cannot fit it alongside
  anything, so it is M6-only.
- **Persephone 2.0 4-bit** (Civitai #1775002, served as `flux-uncensored`) —
  the dedicated NSFW FLUX.1-dev fine-tune already on the M6; unchanged here.

## 2. What changed

### 2.1 Gateway: forward the upstream mapping as the sidecar model

`ImageRequest` gains two fields:

- `SidecarModel` (`json:"-"`) — **not client-owned**. `handleImage` fills it
  from the registry's existing per-model `upstream` mapping in
  `deploy/gateway-providers.json`; a client-sent `model` is never trusted as a
  path. Empty keeps the sidecar on its env-default model, which is what makes
  the change a strict no-op for the pre-existing single-model configs:
  `flux-dev` (M1) and `flux-uncensored` (M6) have no `upstream`, so nothing new
  is sent and nothing observable changes on those hosts.
- `Loras` (`json:"loras,omitempty"`) — a Lattice extension on the images body
  (not an OpenAI field), forwarded to the sidecar's `[{name, scale}]` LoRA refs,
  which the sidecar already supported per-request.
- `Steps` / `Guidance` / `NegativePrompt` / `Seed` / `Strength` — the sidecar's
  tuning knobs on the same extension footing as `loras`: they ride the images
  body and are forwarded only when the client sends them, so this stays a
  strict no-op for bodies that don't carry them. `Seed` and `Strength` are
  pointers (`*int`, `*float64`) because sidecar-side 0 is meaningful (literal
  seed 0; edit strength 0 = keep the input, vs the sidecar's absent-key default
  of 0.4), and `omitempty` on a plain int/float cannot express "sent as 0".
  `Strength` is forwarded only on an edit, since the sidecar carries it
  positionally after `--image` in the CLI argv. These are what let the
  OpenAI-shaped clients below (the Hermes plugin, the MCP server) keep their
  sidecar tunables *through* the lattice instead of losing them — the frontend's
  `buildProxyBody` re-marshals a plain `map[string]interface{}`, so unknown keys
  pass through untouched.

Why `upstream` and not a new config shape: `resolveModel` already returns it,
the config format already carries it (`{ "name": …, "upstream": … }`, used by
the `mlx` provider for chat), and it namespaces exactly what needs
namespacing — the *routing* name is the registry name; the *host-local bake
path or repo id* is the upstream. A model with an `upstream` gets that value in
the sidecar body; without one, the request is bit-for-bit what it was before.

### 2.2 One sidecar per image CLI

`deploy/mflux-sidecar.py` shells exactly one image CLI per instance
(`MFLUX_BIN`). Z-Image and Qwen are different CLIs from `mflux-generate`, so
each model family gets its own sidecar instance instead of teaching the
sidecar multi-CLI dispatch:

| instance | binary | port | hosts |
|---|---|---|---|
| `com.lattice.mflux` | `mflux-generate` | 8899 | M1 (flux-dev-4bit + Lustly), M6 (persephone-4bit) |
| `com.lattice.mflux-zimage` | `mflux-generate-z-image-turbo` | 8898 | M1 + M6 (same prequant repo id) |
| `com.lattice.mflux-qwen` | `mflux-generate-qwen-2.1` | 8897 | M6 |

These are ordinary `kind: mflux` gateway providers at loopback endpoints in
each host's runtime `deploy/gateway-providers.json`. The model names route on
control's existing `selectGateway` — `z-image-turbo` is hosted on *both*
gateways, and control's scoring picks the target (observed: `m6-gateway`,
deterministically at current loads); `qwen-image-2.1` and `flux-uncensored`
exist only on M6's announcement, so there is nothing to misroute.

The prequant repos need no bake on either host: `mflux -model <org/model>
--base-model <family>` with an empty `MFLUX_QUANTIZE` downloads once into the
HF cache and loads directly. The HF cache was rsynced from M6 to M1 to avoid
a second CDN crawl. (Cache copy, not committed config: the HF token stays in
`~/.cache/huggingface/token` per the standing secrets rule.)

## 3. Benchmarks (2026-10-07)

Prompt/512²/4 steps/seed 42/43, warm HF cache, M6 idle (load 1.37), M1 under
its normal resident load. Peak memory is mflux's own `Peak MLX memory` line.

| model | host | seconds/step | wall (4 steps) | peak MLX | swap |
|---|---|---|---|---|---|
| Z-Image-Turbo 4-bit | M6 (32 GB) | 1.37 s | 6 s | **5.63 GB** | 0 |
| Z-Image-Turbo 4-bit | M1 (16 GB) | 23.8 s | 1:37–1:45 | **5.63 GB** | **0 delta** |
| Qwen-Image-2.1 4-bit | M6 (32 GB) | ~2.0 s | 9 s | **10.00 GB** | 0 |

Reading the table:

- **Z-Image-Turbo is the M1's image model.** Same 5.63 GB peak as on the M6 —
  the resident size is a property of the weights, not the host — and with
  `--low-ram` the swap counter did not move one byte across a full
  generation. The seconds/step gap (23.8 vs 1.37) is compute-side; at 4 steps
  it is still a ~2-minute image, comfortably inside the 3600 s timeout chain.
- **Qwen-Image-2.1 is M6-only by measurement**, confirming the placement
  table: its 10 GB peak is exactly the class of load that swapped the M1
  thrash-era history (the 8.3 GB FLUX dev run needed 10 GiB of margin and
  *still* thrashed with Ollama resident).
- Persephone 2.0 (flux-uncensored) benchmarks are already recorded
  (`docs/benchmarks-m6.md`, ~28 s for a 512² 4-step run).

## 4. Invariant check

| invariant | status |
|---|---|
| 1 — one public entry point | held — images still enter via the frontend routes |
| 2 — the Mac computes, the Pi routes | held |
| 3 — `LOCAL_ONLY` never falls back | held — untagged image names still route `local` only |
| 4 — the gateway translates, it does not decide | held — the upstream mapping is config the host carries; the client names a model, not a path |
| 5 — no new infrastructure | pressed, same honest accounting as the first sidecar — a second/third *instance* of the same stdlib process on new loopback ports, plus the repo templates for them |
| 6 — Bee is not modified | held |
| 7 — protect the Mac's SSD | held — measured: zero swap delta on the M1's Z-Image run; Qwen stays off the M1 by placement |
| 8 — no usernames/hostnames/addresses | held — repo files carry model repo ids and loopback endpoints only; host paths (`upstream`, plists) are runtime config |
| 9 — telemetry shares one key | held — one `request_id` per image across frontend → control → gateway |

## 5. Acceptance

Measured live 2026-10-07, all three models, from the Pi frontend
(`POST /v1/images/generations`, `512x512`):

1. `z-image-turbo` → 200, 1 item; `qwen-image-2.1` → 200, 1 item;
   `flux-uncensored` → 200, 1 item.
2. Control telemetry for each: `target:"m6-gateway"`, `locality:"local"`,
   `required_cap:"local"`.
3. Gateway unit tests: upstream mapping arrives as the sidecar `model`;
   no mapping forwards nothing; `loras` arrive in `[{name, scale}]` shape.

## 6. Open items

- **Registry visibility + LoRA access (resolved 2026-10-08).** The zoo was
  routed but invisible: Hermes `list_models` returned only the configured
  default, `_TUNABLES` omitted `loras`, and the MCP tools had no `loras`
  parameter. Resolved live-source-first: the gateway's `/health` announcement
  gains `image_models` (every `kind: mflux*` provider's registry names — the
  registry already knew who was an image provider; now it says so), control
  aggregates them into `/capabilities` sorted and deduplicated, and the
  frontend serves the aggregate at `GET /v1/images/models` (OpenAI list shape,
  `image_model: true`). The chat `/v1/models` stays capability-alias-only so a
  chat picker is never polluted with image names. Consumers: the Hermes plugin's
  `list_models` reads the route live (30 s cache; read failure degrades to the
  configured default entry), the picker's `image_gen.model` choice is now
  honored per-request as the `model` kwarg (previously ignored), and LoRAs
  travel as the existing `loras` extension from `image_gen.mflux.loras`
  (config-level, since the core `image_generate` schema advertises no loras
  arg) and from the MCP tools' new optional `loras` string (`"name:scale,…"`).
- **Hermes plugin / MCP `model` field (resolved 2026-10-08).** Both clients now
  speak OpenAI Images shape at the frontend with **registry model names**:
  - The MCP server's default model moved to `LATTICE_IMAGEGEN_MODEL` (committed
    fallback `flux-dev`), and both tools accept an optional `model` parameter —
    DSH can request `z-image-turbo`, `qwen-image-2.1`, `flux-uncensored`, ….
  - The Hermes `mflux` plugin was rewritten from direct sidecar calls to the
    frontend routes: `image_gen.mflux.url` now names the frontend (Pi loopback),
    `image_gen.mflux.model` names the default registry model, and the sidecar
    tuneables (`steps`/`guidance`/`seed`/`negative_prompt`/edit `strength`)
    still travel, as the §2.1 Lattice extensions. The direct-sidecar variant was
    rejected because one sidecar = one image CLI: multi-model direct would have
    needed a URL per sidecar port (:8899/:8898/:8897) instead of one registry
    name.
- **Qwen-Image 20B 4-bit.** The 32 GB M6 takes the 7.1 B model at 10 GB peak;
  the 20 B at ~26 GB is borderline against resident chat and is deferred
  until the Ollama residency on the M6 is measured concurrent with it.
- **Z-Image uncensoring posture.** Z-Image-Turbo has no model-level NSFW
  filter (restrictions live host-side only in its own tooling); mflux applies
  no filter either. Its uncensored *convention-following* (vs Persephone's
  weights-level fine-tune) still needs a real-prompt A/B against
  `flux-uncensored` on both hosts before it can claim parity.
- **Committed vs host-local registry (resolved 2026-10-08).** The M6's
  diverged `deploy/gateway-providers.json` was moved to runtime config
  (`~/.config/lattice/gateway-providers.json`, plist `LATTICE_GATEWAY_PROVIDERS`
  repointed) — the same home the control plane's env file uses — and its
  deployment tree's `deploy/` files were restored to match committed main,
  so future syncs are idempotent. The committed file tracks the M1's
  registry; any host that diverges from it gets its own runtime-config file
  rather than dirtying the checkout.