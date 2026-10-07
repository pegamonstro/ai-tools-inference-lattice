#!/usr/bin/env python3
"""Real-ESRGAN HTTP sidecar — a thin wrapper around the ``realesrgan-mlx`` CLI.

Super-resolution is a separate, deterministic model pass (no prompt, no diffusion)
from the mflux sidecar, so it gets its own small service. It runs on the Mac — the
only host with MLX — and img-gen POSTs to it over the tailnet.

Design notes:
  * The CLI is driven via subprocess, mirroring mflux-sidecar.py: the CLI is the
    stable surface, and a resident-model backend can replace it later without
    touching the HTTP layer.
  * Single-flight: upscaling a large source to 4x holds the model plus two full-size
    frames in RAM; a second concurrent request risks pressure on the host.
  * The CLI's output filename is not part of its documented contract, so the sidecar
    globs the output directory for the single produced image rather than guessing.
  * Tiling is enabled by default (256 px) to cap Metal memory on a 4x pass; the
    CLI's tile=0 would run the whole frame at once and OOM under a concurrent mflux.

Endpoints:
  GET  /health  -> {status, model, bin}
  POST /upscale -> {image: b64, model?} -> {image: b64, width, height} | {error}

Configuration (environment):
  ESRGAN_BIN    path to the realesrgan-mlx CLI (default "realesrgan-mlx")
  ESRGAN_MODEL  model variant (default "RealESRGAN_x4plus"; also x2plus, anime_6B, …)
  ESRGAN_TILE   tile size in px, 0 to disable (default "256")
  ESRGAN_BIND   bind address (default 127.0.0.1)
  ESRGAN_PORT   port (default 8901)
"""

from __future__ import annotations

import base64
import json
import os
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BIN = os.environ.get("ESRGAN_BIN", "realesrgan-mlx")
MODEL = os.environ.get("ESRGAN_MODEL", "RealESRGAN_x4plus")
TILE = os.environ.get("ESRGAN_TILE", "256")
BIND = os.environ.get("ESRGAN_BIND", "127.0.0.1")
PORT = int(os.environ.get("ESRGAN_PORT", "8901"))
TIMEOUT = int(os.environ.get("ESRGAN_TIMEOUT", "900"))

_lock = threading.Lock()

_IMG_EXTS = (".png", ".jpg", ".jpeg", ".webp")


def run_upscale(image_b64: str, model: str) -> bytes:
    """Run one realesrgan-mlx invocation; return the upscaled image bytes, or raise."""
    with tempfile.TemporaryDirectory(prefix="esrgan-") as td:
        img_path = os.path.join(td, "in.png")
        with open(img_path, "wb") as fh:
            fh.write(base64.b64decode(image_b64))
        outdir = os.path.join(td, "out")
        os.makedirs(outdir, exist_ok=True)

        cmd = [BIN, "-i", img_path, "-o", outdir, "-n", model]
        if TILE not in ("", "0"):
            cmd += ["-t", TILE]
        proc = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=TIMEOUT)
        if proc.returncode != 0:
            detail = (proc.stderr or proc.stdout or "").strip()
            raise RuntimeError(detail[-800:] or "realesrgan-mlx failed")

        produced = [
            f for f in os.listdir(outdir)
            if f.lower().endswith(_IMG_EXTS)
        ]
        if not produced:
            raise RuntimeError("realesrgan-mlx produced no output image")
        # Prefer the largest file if the CLI wrote more than one.
        produced.sort(key=lambda f: os.path.getsize(os.path.join(outdir, f)), reverse=True)
        with open(os.path.join(outdir, produced[0]), "rb") as fh:
            return fh.read()


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
                "bin": os.path.exists(BIN),
            })
        else:
            self._send_json(404, {"error": "not found"})

    def do_POST(self) -> None:
        if self.path.rstrip("/") != "/upscale":
            self._send_json(404, {"error": "not found"}); return

        try:
            length = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(length) or b"{}")
        except (ValueError, json.JSONDecodeError):
            self._send_json(400, {"error": "invalid JSON body", "error_type": "invalid_argument"}); return

        if not isinstance(body.get("image"), str) or not body["image"].strip():
            self._send_json(400, {"error": "image is required", "error_type": "invalid_argument"}); return

        model = body.get("model") or MODEL

        if not _lock.acquire(blocking=False):
            self._send_json(409, {"error": "upscale already in progress", "error_type": "busy"}); return

        try:
            t0 = time.time()
            try:
                img = run_upscale(body["image"], model)
            except subprocess.TimeoutExpired:
                self._send_json(504, {"error": "upscale timed out", "error_type": "timeout"}); return
            except Exception as exc:  # noqa: BLE001 — surface the CLI's own error text
                self._send_json(500, {"error": str(exc), "error_type": "provider_error"}); return
            self._send_json(200, {
                "image": base64.b64encode(img).decode("ascii"),
                "model": model,
                "seconds": round(time.time() - t0, 1),
            })
        finally:
            _lock.release()

    def log_message(self, *args) -> None:  # quiet; the LaunchAgent captures stdout/stderr
        pass


def main() -> None:
    print(f"esrgan sidecar on {BIND}:{PORT} model={MODEL} tile={TILE}", flush=True)
    ThreadingHTTPServer((BIND, PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
