#!/usr/bin/env python3
"""RIFE HTTP sidecar — frame interpolation behind img-gen's RIFE_URL.

Thin wrapper around the RIFE ONNX pipeline (rife_pipeline.py) exposing exactly
the contract img-gen's internal/lattice Client.Rife speaks, mirroring the
rembg/tagger sidecars' HTTP shape. The rife49.onnx weights (~21 MB) load at
boot and stay resident; CPU inference is ~0.1-0.5 s per intermediate frame at
512x512.

Endpoints:
  GET  /health       -> {status, model, session_ready}
  POST /interpolate  -> {images: [b64 png anchors in order], multiplier: 2..10}
                      -> {frames: [b64 png], count, seconds} | {error}

The response's frame count is (n-1)*multiplier+1, anchors included in order,
all at the first anchor's dimensions — img-gen's GIF keyframes composer passes
its validated 2..16 anchors and takes the count on faith.

Configuration (environment):
  RIFE_BIND      bind address (default 0.0.0.0 — tailnet reach, same
                 unauthenticated posture as the other sidecars)
  RIFE_PORT      port (default 8906 — 8900 speech, 8901 esrgan, 8902 sdxl,
                 8903 rembg, 8904 tagger, 8905 dwpose)
  RIFE_MODEL     path to the onnx weights
                 (default <modeldir>/rife49.onnx)
  RifeModelDir   directory holding rife49.onnx (weights root, ~21 MB)
"""

from __future__ import annotations

import base64
import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BIND = os.environ.get("RIFE_BIND", "0.0.0.0")
PORT = int(os.environ.get("RIFE_PORT", "8906"))
WEIGHTS = os.path.join(os.environ.get("RifeModelDir", ""),
                       "rife49.onnx")

_lock = threading.Lock()      # single-flight inference, like every other sidecar
_session = None
_session_lock = threading.Lock()


def get_session():
    """Load the onnx session at first use; reused after."""
    global _session
    if _session is None:
        with _session_lock:
            if _session is None:
                import onnxruntime as ort
                so = ort.SessionOptions()
                so.log_severity_level = 3
                _session = ort.InferenceSession(
                    WEIGHTS, sess_options=so,
                    providers=["CPUExecutionProvider"])
    return _session


def _decode_png_b64(b64: str):
    import numpy as np
    from PIL import Image
    import io as _io
    raw = base64.b64decode(b64)
    im = Image.open(_io.BytesIO(raw)).convert("RGB")
    return np.asarray(im, np.float32).transpose(2, 0, 1) / 255.0


def _encode_png_b64(frame_chw) -> str:
    import numpy as np
    from PIL import Image
    import io as _io
    arr = (np.clip(frame_chw, 0.0, 1.0).transpose(1, 2, 0) * 255.0).round()
    buf = _io.BytesIO()
    Image.fromarray(arr.astype(np.uint8)).save(buf, format="PNG")
    return base64.b64encode(buf.getvalue()).decode()


class Handler(BaseHTTPRequestHandler):
    def _send_json(self, code: int, obj: dict) -> None:
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        if self.path == "/health":
            self._send_json(200, {
                "status": "ok",
                "model": "rife-v4.9",
                "session_ready": _session is not None,
            })
            return
        self._send_json(404, {"error": "not found"})

    def do_POST(self) -> None:
        if self.path != "/interpolate":
            self._send_json(404, {"error": "not found"})
            return
        try:
            n = int(self.headers.get("Content-Length", "0"))
            req = json.loads(self.rfile.read(n))
        except Exception as exc:
            self._send_json(400, {"error": f"bad request: {exc}"})
            return
        imgs = req.get("images", [])
        mult = req.get("multiplier", 2)
        if not isinstance(imgs, list) or len(imgs) < 2:
            self._send_json(400, {"error": "images must hold 2+ png anchors"})
            return
        if not isinstance(mult, int) or mult < 2 or mult > 10:
            self._send_json(400, {"error": "multiplier must be an integer 2..10"})
            return
        if len(imgs) > 16:
            self._send_json(400, {"error": "at most 16 anchors"})
            return

        t0 = time.time()
        try:
            import numpy as np
            from rife_pipeline import interpolate_sequence
            anchors = [_decode_png_b64(x) for x in imgs]
            sess = get_session()
            with _lock:
                frames = interpolate_sequence(sess, anchors, mult)
            self._send_json(200, {
                "frames": [_encode_png_b64(f) for f in frames],
                "count": len(frames),
                "seconds": round(time.time() - t0, 3),
            })
        except Exception as exc:  # noqa: BLE001 — surfaced to the caller verbatim
            self._send_json(500, {"error": f"interpolate failed: {exc}"})

    def log_message(self, *args) -> None:  # quiet; LaunchAgent captures stdout/stderr
        pass


def main() -> None:
    # Eager-load at boot so /health reports session_ready immediately.
    get_session()
    ThreadingHTTPServer((BIND, PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()