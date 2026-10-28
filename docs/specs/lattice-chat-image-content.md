# Spec: Lattice Chat Image Content + WD14 Tagger (Phase D)

**Status:** Implemented
**Host:** Mac (Lattice Gateway)

This note documents the two lattice-side contracts img-gen's Phase D (critic/vision pairing) depends on. The img-gen spec cross-references them at `docs/superpowers/specs/2026-10-28-critic-vision-pairing-design.md` in the sibling img-gen checkout.

---

## 1. Chat Image Content — `ollamaMessages` Contract

The gateway's `ollamaMessages` (cmd/lattice-gateway/main.go:117) normalizes OpenAI-style content-part arrays for Ollama's native `/api/chat`:

- **Text parts** → concatenated into the message's `content` string.
- **Image parts** → only `data:` URLs accepted; base64 payload extracted and appended to the Ollama message's native `images` array. Non-data image URLs return `400 Bad Request` (the gateway cannot fetch remote URLs; silently dropping would be a correctness lie).
- **Non-array content** (string or null) passes through unchanged — pre-image protocol bodies produce byte-identical output.
- Works for both streaming and non-streaming requests; translation happens once at `handleInference` entry.
- Pre-upgrade detection contract: a pre-upgrade gateway (without this code) does **not** return a clean `400` — `Execute` collapses upstream non-200s and `handleInference` writes errors as `500`, so it fails with a `500` whose body text contains `ollama returned status 400`; a directly-migrated path (a caller that reaches Ollama itself) surfaces the upstream `400` as-is. img-gen's detection matches both: a `500` with `ollama returned status` in the body, or a `400` from a directly-migrated path.

Token estimation (`estimateTokens`, main.go:1367) counts images on both shapes. Pre-translation `image_url` parts keep the existing counting; post-translation messages (the production shape after `ollamaMessages`) count each `msg["images"]` entry as a flat 1000-token allowance plus `ceil(len(base64)/4)` tokens — the payload term **caps at 2048** so one huge image can never pin `num_ctx` at the `maxContext` (32768) ceiling: KV sizing is the memory-unsafe direction. `contextWindow` sizing therefore sees translated images (production wiring is covered by `TestContextWindowCountsImages`).

---

## 2. Tagger Sidecar — WD14 (Port 8904)

`deploy/tagger-sidecar.py` + `deploy/com.lattice.tagger.plist.in` — the SmilingWolf/wd-v1-4-convnextv2-tagger-v2 ONNX backend for the critic's tag-vocabulary overlap drift detector.

**Endpoints:**
- `GET /health` → `{status, model, session_ready}`
- `GET /meta` → `{tags: [name, ...]}` (full vocabulary, cached after first load)
- `POST /tag` → `{image: base64 png, threshold?: float}` → `{tags: [{name, score}, ...], seconds}` | `{error, error_type}`
  - 64 MiB body cap, single-flight (`409 busy` when occupied), unauthenticated `0.0.0.0` bind.

**Install:** uv venv at `tagger-venv/bin/python` (onnxruntime + Pillow + numpy + huggingface_hub). Model weights (~0.5 GB) auto-download from Hugging Face on **first use** — the gateway-side `/meta` call at img-gen boot, not the first `/tag` — so the preload happens at boot; session + vocab stay resident. Not in `install-macos.sh` — deployed by targeted install like rembg.

**Preprocessing parity:** `_preprocess` centers the pasted image on the white canvas where the reference SmilingWolf interrogator composites top-left; scores may shift slightly for non-square inputs — the 0.35 drift floor is calibrated against this centering basis.

**Auth:** unauthenticated like the sibling image sidecars (esrgan/sdxl/rembg posture) — the img-gen spec's `token header X-Tagger-Token` line is superseded by this fleet-wide ruling.

**Plist placeholders:** `__TAGGER_PYTHON__`, `__REPO_ROOT__`, `__LOG_DIR__`.

**Environment keys:** `TAGGER_BIND` (default `0.0.0.0`), `TAGGER_PORT` (default `8904`), `TAGGER_REPO` (default `SmilingWolf/wd-v1-4-convnextv2-tagger-v2`).

---

## 3. Consumers — img-gen Phase D

img-gen's Phase D critic (vision pairing) consumes both contracts:
- Chat image content parts for vision-enabled chat turns.
- Tagger sidecar at `TAGGER_URL` for WD14 tag extraction (objective drift detection via vocabulary overlap).
