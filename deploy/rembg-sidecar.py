#!/usr/bin/env python3
"""rembg HTTP sidecar — U²-Net subject extraction behind img-gen's REMBG_URL.

Thin wrapper around the ``rembg`` package (u2netp weights) exposing exactly the
contract img-gen's internal/lattice Client.Rembg speaks, mirroring the ESRGAN
sidecar's HTTP shape. Unlike the ESRGAN sidecar (stdlib-only, CLI subprocess),
this one needs onnxruntime + Pillow, so it runs from a uv venv and keeps the
model session resident in-process — created lazily on first request (weights
download on first use) and reused after.

Endpoints:
  GET  /health   -> {status, model, session_ready}
  POST /segment  -> {image: b64 png} -> {png: b64 png rgba, model, seconds} | {error}

Configuration (environment):
  REMBG_BIND   bind address (default 0.0.0.0 — tailnet reach, same posture as
               the mflux/esrgan sidecars: unauthenticated)
  REMBG_PORT   port (default 8903 — 8900 speech, 8901 esrgan, 8902 sdxl)
  REMBG_MODEL  rembg model name (default "u2netp" — the portable U²-Net)
"""

from __future__ import annotations

import base64
import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from rembg import new_session, remove

BIND = os.environ.get("REMBG_BIND", "0.0.0.0")
PORT = int(os.environ.get("REMBG_PORT", "8903"))
MODEL = os.environ.get("REMBG_MODEL", "u2netp")

_lock = threading.Lock()      # single-flight: one inference at a time
_session_lock = threading.Lock()
_session = None


def get_session():
    """Create the u2netp session on first use (downloads weights), reuse after.

    Session creation is not documented thread-safe and downloads ~5 MB on first
    run, so it is serialized; inference on the returned session also runs under
    the single-flight lock in the handler.
    """
    global _session
    if _session is None:
        with _session_lock:
            if _session is None:
                _session = new_session(MODEL)
    return _session


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
            self._send_json(200, {
                "status": "ok",
                "model": MODEL,
                "session_ready": _session is not None,
            })
        else:
            self._send_json(404, {"error": "not found"})

    def do_POST(self) -> None:
        if self.path.rstrip("/") != "/segment":
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

        if not _lock.acquire(blocking=False):
            self._send_json(409, {"error": "segmentation already in progress", "error_type": "busy"}); return

        try:
            t0 = time.time()
            try:
                session = get_session()
                out = remove(raw, session=session)
            except Exception as exc:  # noqa: BLE001 — surface rembg's own error text
                self._send_json(500, {"error": str(exc)[-800:], "error_type": "provider_error"}); return
            self._send_json(200, {
                "png": base64.b64encode(out).decode("ascii"),
                "model": MODEL,
                "seconds": round(time.time() - t0, 1),
            })
        finally:
            _lock.release()

    def log_message(self, *args) -> None:  # quiet; the LaunchAgent captures stdout/stderr
        pass


def main() -> None:
    print(f"rembg sidecar on {BIND}:{PORT} model={MODEL}", flush=True)
    ThreadingHTTPServer((BIND, PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()