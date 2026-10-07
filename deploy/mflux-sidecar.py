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
  GET  /status   -> {state: "idle"} | {state: "running", id, step, total, elapsed, cancelled}

  POST /generate -> {prompt, negative_prompt?, width?, height?, steps?, guidance?,
                     seed?} -> {image: b64, seed, width, height, seconds} | {error}
  POST /edit     -> same as /generate plus {init_image: b64, strength?}
  POST /fill     -> {prompt, image: b64, mask: b64, steps?, guidance?, seed?}
  POST /redux    -> {prompt, images: [b64...], strengths: [f...], width?, height?,
                     steps?, seed?}
  POST /pose     -> {prompt, image: b64, strength?, negative_prompt?, width?, height?,
                     steps?, guidance?, seed?, model?, loras?}

  GET  /result/<id> -> the finished generation's PNG (raw bytes, "image/png"),
                     404 when unknown. Every generation's output is persisted
                     to RESULTS_DIR/<gen-id>.png (the `id` /status reports), so
                     a client that dies mid-wait can re-attach and fetch the
                     finished image instead of regenerating. GETs are not
                     token-gated (only POSTs are): gen ids are uuid4, so this
                     exposes nothing guessable.

"""

from __future__ import annotations

import base64
import json
import os
import random
import re
import subprocess
import tempfile
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BIN = os.environ.get("MFLUX_BIN", os.path.expanduser("~/.local/bin/mflux-generate"))
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
GEN_TIMEOUT = int(os.environ.get("MFLUX_GEN_TIMEOUT", "3600"))

# Persisted results: every generation writes its PNG to RESULTS_DIR/<gen-id>.png
# rather than a tempdir, so an interrupted client can still fetch it (GET
# /result/<id>). A sweep at startup and before each generation keeps the store
# bounded: TTL first, then a newest-N count cap.
RESULTS_DIR = os.path.expanduser(os.environ.get("MFLUX_RESULTS_DIR", "~/mflux-outputs"))
RESULTS_TTL_HOURS = float(os.environ.get("MFLUX_RESULTS_TTL_HOURS", "24"))
RESULTS_MAX = int(os.environ.get("MFLUX_RESULTS_MAX", "300"))

FILL_BIN = os.environ.get("MFLUX_FILL_BIN", os.path.expanduser("~/.local/bin/mflux-generate-fill"))
REDUX_BIN = os.environ.get("MFLUX_REDUX_BIN", os.path.expanduser("~/.local/bin/mflux-generate-redux"))
# Baked 4-bit models (mflux-save output), used via --model <path>. Unlike generate,
# the fill/redux CLIs hardcode their model_config (dev-fill / dev-redux), so a baked
# path needs no --base-model: --model alone selects the weights. When FILL_MODEL /
# REDUX_MODEL point at a baked model, the matching *_QUANTIZE must be empty (the
# model is already 4-bit; re-quantizing a baked model OOMs).
FILL_MODEL = os.environ.get("MFLUX_FILL_MODEL", "dev-fill")
REDUX_MODEL = os.environ.get("MFLUX_REDUX_MODEL", "dev-redux")
FILL_QUANTIZE = os.environ.get("MFLUX_FILL_QUANTIZE", "8")
REDUX_QUANTIZE = os.environ.get("MFLUX_REDUX_QUANTIZE", "8")

# ControlNet (pose). Like the fill/redux CLIs, mflux-generate-controlnet hardcodes
# its model_config (dev-controlnet-canny, or schnell-controlnet-canny when --model is
# "schnell"), so a baked fine-tune is selected by pointing --model at its local path
# (which the CLI reads as model_path) — no --base-model needed, it is ignored here.
# Defaulting CONTROLNET_MODEL to MODEL keeps pose on the same uncensored base as
# generate. The controlnet adapter (InstantX Canny) is downloaded on first use.
CONTROLNET_BIN = os.environ.get("MFLUX_CONTROLNET_BIN", os.path.expanduser("~/.local/bin/mflux-generate-controlnet"))
CONTROLNET_MODEL = os.environ.get("MFLUX_CONTROLNET_MODEL", MODEL)
CONTROLNET_QUANTIZE = os.environ.get("MFLUX_CONTROLNET_QUANTIZE", "")

_lock = threading.Lock()

# Live state of the (single) in-flight generation. `_lock` guarantees at most
# one runs at a time, so `_current` is unambiguous; `_cur_lock` guards the
# pointer itself so /status and /cancel can read it without touching `_lock`.
_cur_lock = threading.Lock()
_current = None  # dict: {id, proc, step, total, started, cancelled, stderr, stdout}

# tqdm (mflux's per-step bar) writes " 33%|...| 2/25 [...]" to stderr with \r
# separators. We only need the latest N/total fraction.
_PROGRESS_RE = re.compile(r"(\d+)/(\d+)")


def _read_progress(proc: subprocess.Popen, state: dict) -> None:
    """Drain stderr, parsing tqdm's per-step N/total into `state` as it streams.

    tqdm rewrites one line per step using ``\\r`` (carriage return) separators,
    so the "current" progress is whatever follows the last separator; every
    earlier update stays in the stream, so we parse only the trailing segment to
    get the latest step rather than the first. The subprocess is opened with
    ``text=True``, which turns on universal-newlines translation — so those
    ``\\r`` bytes actually arrive as ``\\n``. We split on both to be correct
    either way.
    """
    full = ""
    seg = ""
    try:
        while True:
            c = proc.stderr.read(1)
            if not c:
                break
            full += c
            if c in "\r\n":
                seg = ""
                continue
            seg += c
            m = _PROGRESS_RE.search(seg)
            if m:
                state["step"] = int(m.group(1))
                state["total"] = int(m.group(2))
    except Exception:
        pass
    state["stderr"] = full[-32768:]


def _drain_stdout(proc: subprocess.Popen, state: dict) -> None:
    """Drain stdout so a chatty process never blocks on a full pipe buffer."""
    try:
        state["stdout"] = proc.stdout.read()
    except Exception:
        state["stdout"] = ""


def _exec(cmd: list, out: str, state: dict) -> bytes:
    """Run `cmd`, streaming step progress into `state`; return the PNG bytes or raise."""
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    state["proc"] = proc
    threading.Thread(target=_read_progress, args=(proc, state), daemon=True).start()
    threading.Thread(target=_drain_stdout, args=(proc, state), daemon=True).start()
    try:
        proc.wait(timeout=GEN_TIMEOUT)
    except subprocess.TimeoutExpired:
        proc.kill()
        raise
    if proc.returncode != 0:
        detail = (state.get("stderr") or state.get("stdout") or "").strip()
        raise RuntimeError(detail[-800:] or "mflux failed")
    with open(out, "rb") as fh:
        return fh.read()


def _sweep_results() -> None:
    """Keep the persisted results store bounded: delete files past the TTL,
    then the oldest beyond a newest-N cap."""
    try:
        entries = [f for f in os.listdir(RESULTS_DIR) if f.endswith(".png")]
    except OSError:
        return

    def _mtime(name: str) -> float:
        try:
            return os.path.getmtime(os.path.join(RESULTS_DIR, name))
        except OSError:
            return 0.0

    entries.sort(key=_mtime)  # oldest first
    now = time.time()
    ttl = RESULTS_TTL_HOURS * 3600
    kept = []
    for f in entries:
        if now - _mtime(f) > ttl:
            try:
                os.remove(os.path.join(RESULTS_DIR, f))
            except OSError:
                pass
        else:
            kept.append(f)
    for f in kept[: max(0, len(kept) - RESULTS_MAX)]:
        try:
            os.remove(os.path.join(RESULTS_DIR, f))
        except OSError:
            pass


def _result_path(state: dict) -> str:
    """The persistent output path for this generation: results/<gen-id>.png."""
    return os.path.join(RESULTS_DIR, str(state["id"]) + ".png")


def run_generation(params: dict, state: dict) -> bytes:
    """Run one mflux-generate invocation; return the PNG bytes, or raise."""
    out = _result_path(state)  # persisted; aux inputs still land in a tempdir
    with tempfile.TemporaryDirectory(prefix="mflux-") as td:
        model = params.get("model") or MODEL
        cmd = [BIN, "--model", model, "--output", out]
        loras = params.get("loras")
        if loras:
            for ref in loras:
                cmd += ["--lora", ref["name"], str(ref.get("scale", 1.0))]
        elif LORA:
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

        return _exec(cmd, out, state)


def run_fill(params: dict, state: dict) -> bytes:
    """Run one mflux-generate-fill invocation (masked inpainting)."""
    out = _result_path(state)
    with tempfile.TemporaryDirectory(prefix="mflux-") as td:
        img_path = os.path.join(td, "image.png")
        mask_path = os.path.join(td, "mask.png")
        with open(img_path, "wb") as fh:
            fh.write(base64.b64decode(params["image"]))
        with open(mask_path, "wb") as fh:
            fh.write(base64.b64decode(params["mask"]))

        cmd = [
            FILL_BIN,
            "--model", FILL_MODEL,
            "--prompt", params["prompt"],
            "--image-path", img_path,
            "--masked-image-path", mask_path,
            "--output", out,
            "--seed", str(params["seed"]),
            "--steps", str(params.get("steps", 25)),
            "--guidance", str(params.get("guidance", 30.0)),
            "--vae-tiling",
        ]
        if FILL_QUANTIZE:
            cmd += ["-q", FILL_QUANTIZE]
        return _exec(cmd, out, state)


def run_redux(params: dict, state: dict) -> bytes:
    """Run one mflux-generate-redux invocation (multi-reference)."""
    with tempfile.TemporaryDirectory(prefix="mflux-") as td:
        paths = []
        for i, b64 in enumerate(params["images"]):
            p = os.path.join(td, f"ref{i}.png")
            with open(p, "wb") as fh:
                fh.write(base64.b64decode(b64))
            paths.append(p)
        out = _result_path(state)

        cmd = [
            REDUX_BIN,
            "--model", REDUX_MODEL,
            "--prompt", params["prompt"],
            "--redux-image-paths", *paths,
            "--redux-image-strengths", *[str(s) for s in params["strengths"]],
            "--output", out,
            "--seed", str(params["seed"]),
            "--width", str(params.get("width", 1024)),
            "--height", str(params.get("height", 1024)),
            "--steps", str(params.get("steps", 20)),
            "--vae-tiling",
        ]
        if REDUX_QUANTIZE:
            cmd += ["-q", REDUX_QUANTIZE]
        return _exec(cmd, out, state)


def run_controlnet(params: dict, state: dict) -> bytes:
    """Run one mflux-generate-controlnet invocation (edge-guided pose)."""
    out = _result_path(state)
    with tempfile.TemporaryDirectory(prefix="mflux-") as td:
        ref_path = os.path.join(td, "ref.png")
        with open(ref_path, "wb") as fh:
            fh.write(base64.b64decode(params["image"]))

        model = params.get("model") or CONTROLNET_MODEL
        cmd = [
            CONTROLNET_BIN,
            "--model", model,
            "--controlnet-image-path", ref_path,
            "--controlnet-strength", str(params.get("strength", 0.7)),
            "--output", out,
        ]
        loras = params.get("loras")
        if loras:
            for ref in loras:
                cmd += ["--lora", ref["name"], str(ref.get("scale", 1.0))]
        if CONTROLNET_QUANTIZE:
            cmd += ["--quantize", CONTROLNET_QUANTIZE]
        cmd += ["--prompt", params["prompt"]]
        if params.get("negative_prompt"):
            cmd += ["--negative-prompt", params["negative_prompt"]]
        cmd += ["--width", str(params.get("width", 1024)), "--height", str(params.get("height", 1024))]
        if params.get("steps"):
            cmd += ["--steps", str(params["steps"])]
        if params.get("guidance"):
            cmd += ["--guidance", str(params["guidance"])]
        cmd += ["--seed", str(params["seed"]), "--vae-tiling"]

        return _exec(cmd, out, state)


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
        elif self.path.rstrip("/") == "/status":
            with _cur_lock:
                cur = _current
            if cur is None:
                self._send_json(200, {"state": "idle"})
            else:
                self._send_json(200, {
                    "state": "running",
                    "id": cur["id"],
                    "step": cur.get("step"),
                    "total": cur.get("total"),
                    "elapsed": round(time.time() - cur["started"], 1),
                    "cancelled": cur.get("cancelled", False),
                })
        elif self.path.startswith("/result/"):
            gen_id = self.path[len("/result/"):].split("?")[0].rstrip("/")
            # uuid4 hex only: doubles as the path-traversal guard for building
            # the file path below.
            if not re.fullmatch(r"[0-9a-f]{32}", gen_id or ""):
                self._send_json(404, {"error": "not found"}); return
            path = os.path.join(RESULTS_DIR, gen_id + ".png")
            try:
                with open(path, "rb") as fh:
                    data = fh.read()
            except FileNotFoundError:
                self._send_json(404, {"error": "no result for this gen id"}); return
            except OSError as exc:
                self._send_json(500, {"error": str(exc)}); return
            self.send_response(200)
            self.send_header("Content-Type", "image/png")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        else:
            self._send_json(404, {"error": "not found"})

    def do_POST(self) -> None:
        global _current

        if not self._authorized():
            self._send_json(401, {"error": "unauthorized", "error_type": "auth"}); return

        path = self.path.rstrip("/")
        if path == "/cancel":
            self._do_cancel()
            return
        if path not in ("/generate", "/edit", "/fill", "/redux", "/pose"):
            self._send_json(404, {"error": "not found"}); return

        try:
            length = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(length) or b"{}")
        except (ValueError, json.JSONDecodeError):
            self._send_json(400, {"error": "invalid JSON body", "error_type": "invalid_argument"}); return

        if not isinstance(body.get("prompt"), str) or not body["prompt"].strip():
            self._send_json(400, {"error": "prompt is required", "error_type": "invalid_argument"}); return

        try:
            params = self._build_params(path, body)
        except (ValueError, TypeError):
            self._send_json(400, {"error": "width/height/steps/guidance/seed/strength must be numeric and image inputs present", "error_type": "invalid_argument"}); return

        if not _lock.acquire(blocking=False):
            self._send_json(409, {"error": "generation already in progress", "error_type": "busy"}); return

        _sweep_results()  # inside the single-flight slot, so sweeps never interleave

        state = {
            "id": uuid.uuid4().hex,
            "proc": None,
            "step": None,
            "total": None,
            "started": time.time(),
            "cancelled": False,
            "stderr": "",
            "stdout": "",
        }
        with _cur_lock:
            _current = state

        try:
            t0 = time.time()
            try:
                if path in ("/generate", "/edit"):
                    img = run_generation(params, state)
                elif path == "/fill":
                    img = run_fill(params, state)
                elif path == "/redux":
                    img = run_redux(params, state)
                else:
                    img = run_controlnet(params, state)
            except subprocess.TimeoutExpired:
                self._send_json(504, {"error": "generation timed out", "error_type": "timeout"}); return
            except Exception as exc:  # noqa: BLE001 — surface the subprocess's own error text
                if state.get("cancelled"):
                    self._send_json(499, {"error": "generation cancelled", "error_type": "cancelled"}); return
                self._send_json(500, {"error": str(exc), "error_type": "provider_error"}); return
            self._send_json(200, {
                "image": base64.b64encode(img).decode("ascii"),
                "seed": params.get("seed"),
                "width": params.get("width"),
                "height": params.get("height"),
                "seconds": round(time.time() - t0, 1),
            })
        finally:
            with _cur_lock:
                if _current is state:
                    _current = None
            _lock.release()

    def _do_cancel(self) -> None:
        with _cur_lock:
            cur = _current
        if cur is None:
            self._send_json(200, {"state": "idle", "cancelled": False}); return
        cur["cancelled"] = True
        proc = cur.get("proc")
        if proc is not None and proc.poll() is None:
            proc.terminate()

            def _kill():
                time.sleep(2)
                if proc.poll() is None:
                    proc.kill()
            threading.Thread(target=_kill, daemon=True).start()
        self._send_json(200, {"state": "running", "cancelled": True, "id": cur["id"]})

    def _build_params(self, path: str, body: dict) -> dict:
        common = {
            "prompt": body["prompt"].strip(),
            "seed": int(body["seed"]) if body.get("seed") is not None else random.randrange(0, 1_000_000_000),
        }
        if path in ("/generate", "/edit"):
            p = dict(common,
                     negative_prompt=body.get("negative_prompt"),
                     width=int(body.get("width", 1024)),
                     height=int(body.get("height", 1024)),
                     steps=int(body["steps"]) if body.get("steps") else None,
                     guidance=float(body["guidance"]) if body.get("guidance") else None,
                     model=body.get("model"),
                     loras=body.get("loras"))
            if path == "/edit":
                if not body.get("init_image"):
                    raise TypeError("init_image required")
                p["init_image"] = body["init_image"]
                p["strength"] = float(body.get("strength", 0.4))
            return p
        if path == "/fill":
            if not body.get("image") or not body.get("mask"):
                raise TypeError("image and mask required")
            return dict(common,
                        image=body["image"], mask=body["mask"],
                        steps=int(body["steps"]) if body.get("steps") else 25,
                        guidance=float(body["guidance"]) if body.get("guidance") else 30.0)
        if path == "/pose":
            if not body.get("image"):
                raise TypeError("image required")
            return dict(common,
                        image=body["image"],
                        strength=float(body.get("strength", 0.7)),
                        negative_prompt=body.get("negative_prompt"),
                        width=int(body.get("width", 1024)),
                        height=int(body.get("height", 1024)),
                        steps=int(body["steps"]) if body.get("steps") else None,
                        guidance=float(body["guidance"]) if body.get("guidance") else None,
                        model=body.get("model"),
                        loras=body.get("loras"))
        # /redux
        images = body.get("images")
        if not isinstance(images, list) or not images:
            raise TypeError("images required")
        strengths = body.get("strengths") or [1.0] * len(images)
        if len(strengths) < len(images):
            strengths = list(strengths) + [1.0] * (len(images) - len(strengths))
        return dict(common,
                    images=images,
                    strengths=[float(s) for s in strengths[: len(images)]],
                    width=int(body.get("width", 1024)),
                    height=int(body.get("height", 1024)),
                    steps=int(body["steps"]) if body.get("steps") else 20)

    def log_message(self, *args) -> None:  # quiet; the LaunchAgent captures stdout/stderr
        pass


def main() -> None:
    os.makedirs(RESULTS_DIR, exist_ok=True)
    _sweep_results()
    print(
        f"mflux sidecar on {BIND}:{PORT} model={MODEL} lora={LORA or '-'} quantize={QUANTIZE} "
        f"results_dir={RESULTS_DIR}",
        flush=True,
    )
    ThreadingHTTPServer((BIND, PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
