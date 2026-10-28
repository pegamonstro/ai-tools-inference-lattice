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
        # Model input shape is [1, 448, 448, 3] (NHWC); height/width are at index 1,2
        _input_size = _session.get_inputs()[0].shape[1] or 448


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
