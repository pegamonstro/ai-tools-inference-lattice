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
- A pre-upgrade gateway (without this code) returns `400` on content-part bodies; img-gen detects this and falls back.

Token estimation (`estimateTokens`, main.go:1350) counts image payloads as a fixed 1000-token allowance plus `len(base64)/4` characters.

---

## 2. Tagger Sidecar — WD14 (Port 8904)

`deploy/tagger-sidecar.py` + `deploy/com.lattice.tagger.plist.in` — the SmilingWolf/wd-v1-4-convnextv2-tagger-v2 ONNX backend for the critic's tag-vocabulary overlap drift detector.

**Endpoints:**
- `GET /health` → `{status, model, session_ready}`
- `GET /meta` → `{tags: [name, ...]}` (full vocabulary, cached after first load)
- `POST /tag` → `{image: base64 png, threshold?: float}` → `{tags: [{name, score}, ...], seconds}` | `{error, error_type}`
  - 64 MiB body cap, single-flight (`409 busy` when occupied), unauthenticated `0.0.0.0` bind.

**Install:** uv venv at `tagger-venv/bin/python` (onnxruntime + Pillow + numpy + huggingface_hub). Model weights (~0.5 GB) auto-download from Hugging Face on first `/tag`; session + vocab stay resident. Not in `install-macos.sh` — deployed by targeted install like rembg.

**Plist placeholders:** `__TAGGER_PYTHON__`, `__REPO_ROOT__`, `__LOG_DIR__`.

**Environment keys:** `TAGGER_BIND` (default `0.0.0.0`), `TAGGER_PORT` (default `8904`), `TAGGER_REPO` (default `SmilingWolf/wd-v1-4-convnextv2-tagger-v2`).

---

## 3. Consumers — img-gen Phase D

img-gen's Phase D critic (vision pairing) consumes both contracts:
- Chat image content parts for vision-enabled chat turns.
- Tagger sidecar at `TAGGER_URL` for WD14 tag extraction (objective drift detection via vocabulary overlap).