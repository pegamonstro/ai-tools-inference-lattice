# Gateway Image-Content Enabler Implementation Plan (Phase D / D1)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the lattice chat path multimodal input — OpenAI-style image content parts translate to Ollama-native `images` — plus the WD14 tagger sidecar the img-gen critic consumes, so the Phase D critic can run without img-gen bypassing the lattice.

**Architecture:** One translation point in `ollamaMessages` (covers streaming + non-streaming, both native-path call sites), coarse image-token accounting in `estimateTokens`, and a new standalone sidecar following the rembg deployment template. img-gen detects a pre-upgrade gateway honestly and fails with a named error.

**Tech Stack:** Go (stdlib only), Python sidecar (onnxruntime + Pillow + numpy + huggingface_hub in a uv venv).

**Spec:** `img-gen/docs/superpowers/specs/2026-10-28-critic-vision-pairing-design.md` ("D1 — Gateway enabler" + "Tagger sidecar" sections). This plan implements the lattice-side deliverables; the img-gen-side plan is `img-gen/docs/superpowers/plans/2026-10-28-phase-d-critic.md`.

## Global Constraints

- Source repo for every task below: `/Users/<you>/Projects/LLM-router` (its own git history; commits land here, not in img-gen).
- `.gitignore:14` is a repo-wide `*.py` rule: sidecar `.py` files are tracked via `git add -f` (house pattern, same as rembg/esrgan).
- No real usernames/hostnames/IPs in tracked files (plist/env templates only; this is the repo's standing invariant).
- Never deploy to M6 in these tasks. Deployment (build darwin-arm64 → scp → codesign → bootout/bootstrap, never kickstart) is an explicit-user-authorization step afterward; rollback is `lattice-gateway.bak`.
- Sidecar contracts: errors `{"error": str, "error_type": auth|invalid_argument|busy|timeout|cancelled|provider_error}`; single-flight; 64 MiB body cap; quiet request logging (rembg-sidecar.py conventions).
- Port: tagger = **8904**; unauthenticated 0.0.0.0 bind matching esrgan/sdxl/rembg posture.

---

### Task 1: Chat content-part translation

**Files:**
- Modify: `cmd/lattice-gateway/main.go` (structs near line 95–143, `ollamaMessages` signature, call sites at lines 761 and 884)
- Test: `cmd/lattice-gateway/chat_vision_test.go` (new)

**Interfaces:**
- Consumes: existing `ollamaMessages([]interface{}) []interface{}` (main.go:99) and its two call sites — `"messages": ollamaMessages(req.Messages)` inside the non-streaming native-request body (main.go:761) and the identical expression in the streaming handler (main.go:884).
- Produces: `ollamaMessages(messages []interface{}) ([]interface{}, error)` — same normalization semantics, plus content-part handling. `imageURLB64(u string) string` (data-URL payload extractor). Both callers must propagate the error as a 400.

- [ ] **Step 1: Write the failing tests**

```go
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const tinyPNGb64 = "aGVsbG8=" // shape only; ollamaMessages never decodes bytes

func partsMsg(parts ...map[string]any) map[string]any {
	arr := make([]any, len(parts))
	for i, p := range parts {
		arr[i] = p
	}
	return map[string]any{"role": "user", "content": arr}
}

func imgPart(dataURL string) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}}
}

func textPart(t string) map[string]any { return map[string]any{"type": "text", "text": t} }

func TestOllamaMessagesContentPartsImage(t *testing.T) {
	in := []any{partsMsg(textPart("judge this"), imgPart("data:image/png;base64,"+tinyPNGb64))}
	out, err := ollamaMessages(in)
	if err != nil {
		t.Fatal(err)
	}
	msg := out[0].(map[string]any)
	if got := msg["content"]; got != "judge this" {
		t.Fatalf("content = %v", got)
	}
	imgs, ok := msg["images"].([]string)
	if !ok || len(imgs) != 1 || imgs[0] != tinyPNGb64 {
		t.Fatalf("images = %#v", msg["images"])
	}
	// The original OpenAI part shape must not survive on the translated message.
	if _, ok := msg["content"].([]any); ok {
		t.Fatal("content still an array")
	}
}

func TestOllamaMessagesMultiImageAndTextJoin(t *testing.T) {
	in := []any{partsMsg(textPart("one"), imgPart("data:image/png;base64,A"), imgPart("data:image/jpeg;base64,B"), textPart("two"))}
	out, err := ollamaMessages(in)
	if err != nil {
		t.Fatal(err)
	}
	msg := out[0].(map[string]any)
	if msg["content"] != "onetwo" {
		t.Fatalf("content = %v", msg["content"])
	}
	imgs := msg["images"].([]string)
	if len(imgs) != 2 || imgs[0] != "A" || imgs[1] != "B" {
		t.Fatalf("images = %#v", imgs)
	}
}

func TestOllamaMessagesStringContentUntouched(t *testing.T) {
	in := []any{map[string]any{"role": "user", "content": "plain"}, map[string]any{"role": "user", "content": []any{textPart("x")}}}
	out, err := ollamaMessages(in)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].(map[string]any)["images"] != nil {
		t.Fatal("string content must not gain images")
	}
	if out[1].(map[string]any)["content"] != "x" {
		t.Fatal("parts content was not flattened")
	}
}

func TestOllamaMessagesRejectsNonDataURL(t *testing.T) {
	in := []any{partsMsg(imgPart("https://example.com/cat.png"))}
	_, err := ollamaMessages(in)
	if err == nil || !strings.Contains(err.Error(), "data:") {
		t.Fatalf("want error naming data: URLs, got %v", err)
	}
}

func TestOllamaMessagesToolCallsStillNormalize(t *testing.T) {
	in := []any{map[string]any{
		"role": "assistant", "content": "",
		"tool_calls": []any{map[string]any{
			"function": map[string]any{"name": "f", "arguments": `{"a":1}`},
		}},
	}}
	out, err := ollamaMessages(in)
	if err != nil {
		t.Fatal(err)
	}
	call := out[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	var args map[string]any
	if err := json.Unmarshal(fn["arguments"].(json.RawMessage), &args); err != nil {
		t.Fatalf("arguments not re-emitted as object: %v", fn["arguments"])
	}
	if args["a"] != float64(1) {
		t.Fatal("arguments value lost")
	}
}

func TestEstimateTokensCountsImages(t *testing.T) {
	base := estimateTokens([]any{map[string]any{"role": "user", "content": "hello"}})         // 5 chars -> 2
	withImg := estimateTokens([]any{partsMsg(textPart("hello"), imgPart("data:image/png;base64,"+strings.Repeat("x", 8000)))})
	if withImg <= base {
		t.Fatalf("image part not counted: base=%d with=%d", base, withImg)
	}
	// (8000+3)/4 = 2000 for the payload plus the fixed 1000-token image allowance.
	if withImg-base < 3000 || withImg-base > 3000+2 {
		t.Fatalf("image estimate %d, want ~%d", withImg-base, 2000+1000)
	}
}
```

- [ ] **Step 2: Run the tests, verify they fail**

Run: `go test ./cmd/lattice-gateway/ -run 'TestOllamaMessages|TestEstimateTokensCountsImages' -v`
Expected: FAIL — `ollamaMessages` returns one value (compile error), content parts ignored, tools test may pass pre-change.

- [ ] **Step 3: Implement the translation**

In `cmd/lattice-gateway/main.go`, add beside `ollamaArguments` (around line 93):

```go
// visionPart is one OpenAI-style content part. Only the two shapes the
// gateway understands are modeled: text and image_url.
type visionPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url"` // object {url} in JSON; decoded manually below
}

// imageURLB64 extracts the base64 payload of a data: URL, "" when the URL
// is not one.
func imageURLB64(u string) string {
	const marker = "base64,"
	if !strings.HasPrefix(u, "data:") {
		return ""
	}
	if i := strings.Index(u, marker); i >= 0 {
		return u[i+len(marker):]
	}
	return ""
}
```

Replace `ollamaMessages` (main.go:99–143) — the signature gains an error, everything else keeps its semantics:

```go
// ollamaMessages normalizes an OpenAI-shaped history for Ollama's native
// /api/chat (see ollamaArguments). Beyond tool-call arguments it now also
// flattens OpenAI-style content-part arrays: text parts concatenate into
// content, and each image part's data URL becomes an entry on the Ollama
// message's native images array. A parts message with a non-data image URL
// is an error — the gateway cannot fetch remote URLs, and silently dropping
// the image would be a correctness lie. Messages without arrays or images
// pass through unchanged, so a body identical to the pre-image protocol
// produces byte-identical output.
func ollamaMessages(messages []interface{}) ([]interface{}, error) {
	out := make([]interface{}, len(messages))
	for i, rawMsg := range messages {
		msg, ok := rawMsg.(map[string]interface{})
		if !ok {
			out[i] = rawMsg
			continue
		}
		// Content-part array flattening.
		if rawParts, isArr := msg["content"].([]interface{}); isArr {
			copyMsg := make(map[string]interface{}, len(msg)+1)
			var text strings.Builder
			var images []string
			for _, rp := range rawParts {
				p, ok := rp.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("message content part %d is not an object", i)
				}
				switch p["type"] {
				case "text":
					if s, ok := p["text"].(string); ok {
						text.WriteString(s)
					}
				case "image_url":
					iu, _ := p["image_url"].(map[string]interface{})
					u, _ := iu["url"].(string)
					b64 := imageURLB64(u)
					if b64 == "" {
						return nil, fmt.Errorf("image content part URL must be a data: URL (the gateway cannot fetch remote URLs)")
					}
					images = append(images, b64)
				default:
					// Unknown part types are ignored: OpenAI adds part types
					// over time and a text-only consumer must keep working.
				}
			}
			copyMsg["content"] = text.String()
			if len(images) > 0 {
				copyMsg["images"] = images
			}
			for k, v := range msg {
				if k == "content" {
					continue
				}
				copyMsg[k] = v
			}
			out[i] = copyMsg
			continue
		}
		// Tool-call argument normalization, unchanged (see ollamaArguments).
		rawCalls, ok := msg["tool_calls"].([]interface{})
		// [the existing body of the tool_calls branch, verbatim, ending with
		//  out[i] = copyMsg  — kept byte-for-byte from the pre-change file]
		if !ok {
			out[i] = rawMsg
			continue
		}
		// ... existing tool-call copy/normalize code unchanged ...
	}
	return out, nil
}
```

Update the two call sites to propagate the error as a 400 (use each handler's existing error-writing helper; both sites currently build the request body inline, so hoist the call above the body literal):

```go
// Both call sites (main.go:761 native path, main.go:884 streaming path):
msgs, err := ollamaMessages(req.Messages)
if err != nil {
	// <existing 400/error helper of that handler, e.g. how it rejects bad bodies>
	writeErr(w, http.StatusBadRequest, err.Error())
	return
}
// then inside the request map: "messages": msgs,
```

Also extend `estimateTokens` (main.go:1287–1297) so part arrays and image payloads count:

```go
func estimateTokens(messages []interface{}) int {
	total := 0
	for _, m := range messages {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		switch c := mm["content"].(type) {
		case string:
			total += (len(c) + 3) / 4
		case []interface{}:
			for _, rp := range c {
				p, ok := rp.(map[string]interface{})
				if !ok {
					continue
				}
				switch p["type"] {
				case "text":
					if s, ok := p["text"].(string); ok {
						total += (len(s) + 3) / 4
					}
				case "image_url":
					total += 1000 // fixed allowance per image, erroring toward safety
					if iu, _ := p["image_url"].(map[string]interface{}); iu != nil {
						if u, _ := iu["url"].(string); u != "" {
							total += (len(imageURLB64(u)) + 3) / 4
						}
					}
				}
			}
		}
	}
	return total
}
```

- [ ] **Step 4: Run the tests, verify they pass**

Run: `go test ./cmd/lattice-gateway/ -run 'TestOllamaMessages|TestEstimateTokensCountsImages' -v`
Expected: PASS. Then the full package: `go test ./cmd/lattice-gateway/` — the existing suite (router coverage, tool translation, streaming) must stay green; if a test asserted `ollamaMessages` directly with one return value, update that call site to the two-value form (semantics unchanged).

- [ ] **Step 5: gofmt + vet + commit**

```bash
gofmt -w cmd/lattice-gateway/main.go cmd/lattice-gateway/chat_vision_test.go
go vet ./cmd/lattice-gateway/
git add cmd/lattice-gateway/main.go cmd/lattice-gateway/chat_vision_test.go
git commit -m "feat(gateway): translate chat image content parts to ollama images

OpenAI-style content part arrays flatten to text + the native images array;
non-data image URLs fail loudly; estimateTokens counts image payloads (they
contributed zero to num_ctx sizing before).

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: WD14 tagger sidecar

**Files:**
- Create: `deploy/tagger-sidecar.py`
- Create: `deploy/com.lattice.tagger.plist.in`
- Test: live smoke (local, no M6 deploy — see Step 5)

**Interfaces:**
- Consumes: the rembg-sidecar.py HTTP shape (single-flight lock, `_send_json`, error taxonomy, `_lock.acquire(blocking=False)` busy path).
- Produces: exactly the contract img-gen's `internal/lattice.Tagger` speaks —
  `GET /health → {status, model, session_ready}`; `GET /meta → {tags: [<vocabulary names>]}`; `POST /tag {"image": <b64 png>, "threshold"?: f} → {"tags": [{"name", "score"}...], "seconds"}`. Errors: `{"error","error_type"}`.

- [ ] **Step 1: Write `deploy/tagger-sidecar.py`**

```python
#!/usr/bin/env python3
"""WD14 danbooru tagger HTTP sidecar — image tagging behind img-gen's TAGGER_URL.

Thin wrapper around the SmilingWolf/wd-v1-4-convnextv2-tagger-v2 ONNX model,
exposing exactly the contract img-gen's internal/lattice Client.Tagger speaks,
mirroring the rembg sidecar's HTTP shape (single-flight, error taxonomy,
resident in-process session). Runs from a uv venv: onnxruntime + Pillow +
numpy + huggingface_hub. The model files (~0.5 GB) download from Hugging Face
on first use into HF's own cache and are reused after.

Endpoints:
  GET  /health -> {status, model, session_ready}
  GET  /meta   -> {tags: [name, ...]}           # the full tag vocabulary (cached)
  POST /tag    -> {image: b64 png, threshold?: f}
               -> {tags: [{name, score}, ...], seconds} | {error}

Configuration (environment):
  TAGGER_BIND  bind address (default 0.0.0.0 — tailnet reach, same posture as
               the mflux/esrgan/rembg sidecars: unauthenticated)
  TAGGER_PORT  port (default 8904 — 8903 rembg is the last neighbor)
  TAGGER_REPO  Hugging Face repo id (default SmilingWolf/wd-v1-4-convnextv2-tagger-v2)
"""

from __future__ import annotations

import base64
import csv
import io
import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
import onnxruntime as ort
from huggingface_hub import hf_hub_download
from PIL import Image

BIND = os.environ.get("TAGGER_BIND", "0.0.0.0")
PORT = int(os.environ.get("TAGGER_PORT", "8904"))
REPO = os.environ.get("TAGGER_REPO", "SmilingWolf/wd-v1-4-convnextv2-tagger-v2")

_lock = threading.Lock()        # single-flight inference
_load_lock = threading.Lock()   # serializes the first model load
_session = None
_vocab: list[str] = []          # selected tag names, model input order
_input_name = ""
_input_size = 448               # wd-v1-4-family input resolution


def _load() -> None:
    """Load the ONNX session + vocabulary on first use (downloads ~0.5 GB)."""
    global _session, _vocab, _input_name, _input_size
    if _session is not None:
        return
    with _load_lock:
        if _session is not None:
            return
        model_path = hf_hub_download(REPO, "model.onnx")
        csv_path = hf_hub_download(REPO, "selected_tags.csv")
        _vocab = []
        with open(csv_path, newline="", encoding="utf-8") as f:
            for row in csv.DictReader(f):
                _vocab.append(row["name"])
        so = ort.SessionOptions()
        so.inter_op_num_threads, so.intra_op_num_threads = 2, 2
        _session = ort.InferenceSession(model_path, sess_options=so, providers=["CPUExecutionProvider"])
        _input_name = _session.get_inputs()[0].name
        _input_size = _session.get_inputs()[0].shape[-1] or 448


def _preprocess(raw: bytes) -> np.ndarray:
    """Canonical WD14 input: RGBA canvas of the input size, image pasted
    (never stretched) centered on white, RGB float32 [0,1], 1xHxWxC."""
    img = Image.open(io.BytesIO(raw)).convert("RGBA")
    canvas = Image.new("RGBA", (_input_size, _input_size), (255, 255, 255, 255))
    img.thumbnail((_input_size, _input_size), Image.LANCZOS)
    canvas.paste(img, ((_input_size - img.width) // 2, (_input_size - img.height) // 2), img)
    arr = np.asarray(canvas.convert("RGB"), dtype=np.float32) / 255.0
    return arr[np.newaxis, :, :, :]


def _tag(raw: bytes, threshold: float) -> list[dict]:
    _load()
    out = _session.run(None, {_input_name: _preprocess(raw)})[0][0]
    # The model emits logits-per-tag in _vocab order; the WD14 convention is a
    # plain sigmoid per tag (no scipy dependency).
    scores = 1.0 / (1.0 + np.exp(-out.astype(np.float64)))
    tags = [
        {"name": name, "score": round(float(s), 4)}
        for name, s in zip(_vocab, scores)
        if float(s) >= threshold
    ]
    tags.sort(key=lambda t: -t["score"])
    return tags


class Handler(BaseHTTPRequestHandler):
    def _send_json(self, code: int, obj: dict) -> None:
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        if self.path.rstrip("/") == "/health":
            self._send_json(200, {"status": "ok", "model": REPO, "session_ready": _session is not None})
        elif self.path.rstrip("/") == "/meta":
            try:
                _load()
            except Exception as exc:  # noqa: BLE001
                self._send_json(500, {"error": str(exc)[-800:], "error_type": "provider_error"}); return
            self._send_json(200, {"tags": _vocab})
        else:
            self._send_json(404, {"error": "not found"})

    def do_POST(self) -> None:
        if self.path.rstrip("/") != "/tag":
            self._send_json(404, {"error": "not found"}); return
        try:
            length = int(self.headers.get("Content-Length", 0))
            if length <= 0 or length > 64 << 20:
                self._send_json(400, {"error": "body size out of range", "error_type": "invalid_argument"}); return
            body = json.loads(self.rfile.read(length) or b"{}")
        except (ValueError, json.JSONDecodeError):
            self._send_json(400, {"error": "invalid JSON body", "error_type": "invalid_argument"}); return
        if not isinstance(body.get("image"), str) or not body["image"].strip():
            self._send_json(400, {"error": "image is required", "error_type": "invalid_argument"}); return
        try:
            raw = base64.b64decode(body["image"], validate=True)
        except Exception:
            self._send_json(400, {"error": "image is not valid base64", "error_type": "invalid_argument"}); return
        threshold = body.get("threshold", 0.35)
        if not isinstance(threshold, (int, float)) or not 0.0 < threshold < 1.0:
            self._send_json(400, {"error": "threshold must be in (0,1)", "error_type": "invalid_argument"}); return
        if not _lock.acquire(blocking=False):
            self._send_json(409, {"error": "tagging already in progress", "error_type": "busy"}); return
        try:
            t0 = time.time()
            try:
                tags = _tag(raw, float(threshold))
            except Exception as exc:  # noqa: BLE001 — surface the model error text
                self._send_json(500, {"error": str(exc)[-800:], "error_type": "provider_error"}); return
            self._send_json(200, {"tags": tags, "seconds": round(time.time() - t0, 2)})
        finally:
            _lock.release()

    def log_message(self, *args) -> None:  # quiet; the LaunchAgent captures stdout/stderr
        pass


def main() -> None:
    print(f"tagger sidecar on {BIND}:{PORT} repo={REPO}", flush=True)
    ThreadingHTTPServer((BIND, PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
```

- [ ] **Step 2: Write `deploy/com.lattice.tagger.plist.in`** — mirror `com.lattice.rembg.plist.in` byte-shape with these substitutions: Label `com.lattice.tagger`; ProgramArguments `__TAGGER_PYTHON__` + `__REPO_ROOT__/deploy/tagger-sidecar.py`; EnvironmentVariables `TAGGER_BIND=0.0.0.0`, `TAGGER_PORT=8904`, `TAGGER_REPO=SmilingWolf/wd-v1-4-convnextv2-tagger-v2`; log paths `__LOG_DIR__/lattice-tagger.{out,err}.log`; header comment describing the WD14 model + the uv venv (`tagger-venv/bin/python`, onnxruntime + pillow + numpy + huggingface_hub) + the 8904 port rationale + the unauthenticated tailnet posture. A template reinstall on M6 needs the install-time substitutions `__TAGGER_PYTHON__`, `__REPO_ROOT__`, and `__LOG_DIR__` (the rembg template's extra third placeholder).

- [ ] **Step 3: Local smoke (venv + real weights)**

```bash
cd /Users/<you>/Projects/LLM-router/deploy
uv venv tagger-venv --python 3.12
uv pip install --python tagger-venv/bin/python "onnxruntime" "pillow" "numpy" "huggingface_hub"
TAGGER_BIND=127.0.0.1 TAGGER_PORT=8904 tagger-venv/bin/python tagger-sidecar.py &
sleep 2
curl -sf localhost:8904/health && echo
curl -sf localhost:8904/meta | python3 -c 'import json,sys; t=json.load(sys.stdin)["tags"]; print(len(t), t[:5])'
# first /tag downloads ~0.5 GB on cold cache:
curl -sf -X POST localhost:8904/tag -d '{"image":"'"$(base64 -i img-gen/docs/superpowers/specs/wd14-smoke.png 2>/dev/null || python3 - <<'EOF'
import base64, io
from PIL import Image, ImageDraw
img = Image.new("RGB", (512, 512), "white")
d = ImageDraw.Draw(img)
d.ellipse([128, 128, 384, 384], fill=(30, 30, 30))
buf = io.BytesIO(); img.save(buf, "PNG")
print(base64.b64encode(buf.getvalue()).decode(), end="")
EOF
)'"}' | python3 -m json.tool | head -20
kill %1
```

Expected: `/health` ok; `/meta` names ≥ 9 000 tags (WD14 vocabulary); `/tag` returns score-descending danbooru names. Keep the generated smoke PNG in the workspace only (do NOT commit it).

- [ ] **Step 4: Commit (the .py needs force-add)**

```bash
git add deploy/com.lattice.tagger.plist.in
git add -f deploy/tagger-sidecar.py
git commit -m "feat(deploy): add WD14 danbooru tagger sidecar (8904)

img-gen Phase D critic's objective drift detector: /health, /meta (vocabulary),
/tag {image, threshold} -> scored tags. u2netp-rembg deployment shape.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: Lattice-side spec note

**Files:**
- Create: `docs/specs/lattice-chat-image-content.md`

**Interfaces:** documentation only — names the two lattice-side Phase D contracts so the img-gen spec's cross-repo references resolve inside this repo (the B3 house pattern).

- [ ] **Step 1: Write the spec note.** Sections: (1) *Chat image content* — the `ollamaMessages` contract from Task 1 (parts flatten, data-URL-only images, non-array content untouched, both streaming and not; the estimateTokens image allowance), explicitly noting that a pre-upgrade gateway 400s content-part bodies and img-gen detects that; (2) *Tagger sidecar* — port 8904, the three endpoints, the uv-venv install + plist placeholder substitutions from Task 2, TAGGER_NAME env keys; (3) *Consumers* — img-gen Phase D (link the img-gen spec path). Keep it under ~60 lines; the specs dir's existing files are the style anchor.

- [ ] **Step 2: Commit**

```bash
git add docs/specs/lattice-chat-image-content.md
git commit -m "docs(spec): chat image content parts + WD14 tagger sidecar contracts

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

- [ ] **Step 3: Whole-repo suite**

Run: `go test ./...` in `/Users/<you>/Projects/LLM-router`
Expected: PASS (the modelrepo working-tree changes are pre-existing and not this plan's concern — leave them untouched).

**Deployment (out of plan scope, explicit user authorization):** build `GOOS=darwin GOARCH=arm64 go build -o bin/darwin-arm64/lattice-gateway ./cmd/lattice-gateway`, scp → `.new` → mv, `codesign -s - -f`, `launchctl bootout` + `bootstrap` (never kickstart), curl-verify with an image content part against the upgraded gateway. Tag sidecar install follows the rembg runbook (uv venv on M6 at `/Users/<you>/lattice/deploy/tagger-venv`, plist sed for the three placeholders, bootout-guarded bootstrap).