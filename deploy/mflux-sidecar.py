#!/usr/bin/env python3
"""MFLUX HTTP sidecar — a thin wrapper around ``mflux-generate`` for image generation.

Lattice routes text (chat / embeddings / tool-calling) only. Image generation is a
different modality with a different runtime (a diffusion model), so it does not go
through the gateway. Hermes already owns an ``image_gen`` plugin surface; this
sidecar is the local *backend* for one such plugin. It runs on the Mac — the only
host with MLX and the RAM to hold a diffusion model — and Hermes (on the Pi) POSTs
to it over the tailnet.

Design notes:
  * The CLI is driven via subprocess rather than mflux's Python API. That API is not
    stable across versions (the top-level ``Flux1`` import is gone in current mflux),
    and the CLI is a stable, version-independent surface. The one real cost is a
    model reload per request; ``run_generation`` is isolated so a resident-model
    backend can replace it later without touching the HTTP layer.
  * Single-flight: mflux holds the whole model in RAM, so concurrent generations
    would OOM the 16 GB host. A second request while one is running gets 409.

Endpoints:
  GET  /health   -> {status, model, lora, quantize, bin}
  POST /generate -> {prompt, negative_prompt?, width?, height?, steps?, guidance?,
                     seed?} -> {image: b64, seed, width, height, seconds} | {error}
  POST /edit     -> same as /generate plus {init_image: b64, strength?}
"""

from __future__ import annotations

import base64
import json
import os
import random
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BIN = os.environ.get("MFLUX_BIN", os.path.expanduser("~/lattice-mflux/venv/bin/mflux-generate"))
MODEL = os.environ.get("MFLUX_MODEL", "dev")
LORA = os.environ.get("MFLUX_LORA", "")  # e.g. shauray/flux-uncensored-lora
LORA_SCALE = os.environ.get("MFLUX_LORA_SCALE", "1.0")
# When MODEL is a baked mflux-save output (already 4-bit), QUANTIZE must be empty
# and EXTRA carries "--base-model dev --no-bake-lora --low-ram" — re-quantizing a
# baked model, or baking the LoRA into fp16, is what OOMs the 16 GB host.
QUANTIZE = os.environ.get("MFLUX_QUANTIZE", "4")
EXTRA = os.environ.get("MFLUX_EXTRA_ARGS", "").split()
BIND = os.environ.get("MFLUX_BIND", "127.0.0.1")
PORT = int(os.environ.get("MFLUX_PORT", "8899"))
TOKEN = os.environ.get("MFLUX_TOKEN", "")  # optional shared secret
GEN_TIMEOUT = int(os.environ.get("MFLUX_GEN_TIMEOUT", "1800"))

_lock = threading.Lock()


def run_generation(params: dict) -> bytes:
    """Run one mflux-generate invocation; return the PNG bytes, or raise."""
    with tempfile.TemporaryDirectory(prefix="mflux-") as td:
        out = os.path.join(td, "out.png")
        cmd = [BIN, "--model", MODEL, "--output", out]
        if LORA:
            cmd += ["--lora", LORA, LORA_SCALE]
        if QUANTIZE:
            cmd += ["--quantize", QUANTIZE]
        cmd += EXTRA
        cmd += ["--prompt", params["prompt"]]
        if params.get("negative_prompt"):
            cmd += ["--negative-prompt", params["negative_prompt"]]
        cmd += ["--width", str(params.get("width", 1024)), "--height", str(params.get("height", 1024))]
        if params.get("steps"):
            cmd += ["--steps", str(params["steps"])]
        if params.get("guidance"):
            cmd += ["--guidance", str(params["guidance"])]
        cmd += ["--seed", str(params["seed"])]
        if params.get("init_image"):
            init = os.path.join(td, "init.png")
            with open(init, "wb") as fh:
                fh.write(base64.b64decode(params["init_image"]))
            cmd += ["--image", init, str(params.get("strength", 0.4))]

        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=GEN_TIMEOUT)
        if proc.returncode != 0:
            detail = (proc.stderr or proc.stdout or "mflux-generate failed").strip()
            raise RuntimeError(detail[-800:])

        with open(out, "rb") as fh:
            return fh.read()


class Handler(BaseHTTPRequestHandler):
    def _send_json(self, code: int, obj: dict) -> None:
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _authorized(self) -> bool:
        return not TOKEN or self.headers.get("X-Mflux-Token") == TOKEN

    def do_GET(self) -> None:
        if self.path.rstrip("/") == "/health":
            self._send_json(200, {
                "status": "ok",
                "model": MODEL,
                "lora": LORA or None,
                "quantize": QUANTIZE,
                "bin": os.path.exists(BIN),
            })
        else:
            self._send_json(404, {"error": "not found"})

    def do_POST(self) -> None:
        if not self._authorized():
            self._send_json(401, {"error": "unauthorized", "error_type": "auth"}); return

        path = self.path.rstrip("/")
        if path not in ("/generate", "/edit"):
            self._send_json(404, {"error": "not found"}); return

        try:
            length = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(length) or b"{}")
        except (ValueError, json.JSONDecodeError):
            self._send_json(400, {"error": "invalid JSON body", "error_type": "invalid_argument"}); return

        if not isinstance(body.get("prompt"), str) or not body["prompt"].strip():
            self._send_json(400, {"error": "prompt is required", "error_type": "invalid_argument"}); return
        if path == "/edit" and not body.get("init_image"):
            self._send_json(400, {"error": "init_image is required for /edit", "error_type": "invalid_argument"}); return

        try:
            params = {
                "prompt": body["prompt"].strip(),
                "negative_prompt": body.get("negative_prompt"),
                "width": int(body.get("width", 1024)),
                "height": int(body.get("height", 1024)),
                "steps": int(body["steps"]) if body.get("steps") else None,
                "guidance": float(body["guidance"]) if body.get("guidance") else None,
                "seed": int(body["seed"]) if body.get("seed") is not None else random.randrange(0, 1_000_000_000),
                "init_image": body.get("init_image") if path == "/edit" else None,
                "strength": float(body.get("strength", 0.4)) if path == "/edit" else None,
            }
        except (ValueError, TypeError):
            self._send_json(400, {"error": "width/height/steps/guidance/seed/strength must be numeric", "error_type": "invalid_argument"}); return

        if not _lock.acquire(blocking=False):
            self._send_json(409, {"error": "generation already in progress", "error_type": "busy"}); return

        try:
            t0 = time.time()
            try:
                img = run_generation(params)
            except subprocess.TimeoutExpired:
                self._send_json(504, {"error": "generation timed out", "error_type": "timeout"}); return
            except Exception as exc:  # noqa: BLE001 — surface the subprocess's own error text
                self._send_json(500, {"error": str(exc), "error_type": "provider_error"}); return
            self._send_json(200, {
                "image": base64.b64encode(img).decode("ascii"),
                "seed": params["seed"],
                "width": params["width"],
                "height": params["height"],
                "seconds": round(time.time() - t0, 1),
            })
        finally:
            _lock.release()

    def log_message(self, *args) -> None:  # quiet; the LaunchAgent captures stdout/stderr
        pass


def main() -> None:
    print(f"mflux sidecar on {BIND}:{PORT} model={MODEL} lora={LORA or '-'} quantize={QUANTIZE}", flush=True)
    ThreadingHTTPServer((BIND, PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
