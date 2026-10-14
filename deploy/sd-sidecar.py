#!/usr/bin/env python3
"""SDXL HTTP sidecar — a thin wrapper around the stable-diffusion.cpp CLI for
SDXL-class checkpoints (SDXL base / Pony / Illustrious / merges).

The mflux sidecar frontends the FLUX/MLX runtime; SDXL-class checkpoints have a
different text-encoder + VAE stack, so they load through stable-diffusion.cpp
(ggml/Metal) instead. This sidecar speaks the same HTTP contract as
``mflux-sidecar.py`` (single-flight, sync POST returning the b64 PNG plus
write-then-respond persistence for mid-wait re-attach), so img-gen can treat
both engines interchangeably. Only the modes the SDXL runtime supports are
exposed: text2img (``/generate``) and img2img (``/edit``). Inpaint/fill/redux/pose
are runtime features of the FLUX sidecar and intentionally absent here.

Design notes:
  * The CLI is driven via subprocess; ``-o`` points straight at the persisted
    results path (RESULTS_DIR/<gen-id>.png), preserving the write-then-respond
    invariant: the HTTP response is only sent after the PNG exists.
  * Single-flight: a full SDXL checkpoint holds several GB in RAM/VRAM, so a
    second request while one is running gets 409 — same as the mflux sidecar.
  * LoRAs: stable-diffusion.cpp reads ``<lora:<path>:<scale>>`` tags scanned in
    the prompt itself, resolving relative names against ``--lora-model-dir``.
    We pass img-gen's resolved absolute ``.safetensors`` paths through verbatim
    and always set ``--lora-model-dir`` (the ckpt dir) so extraction activates.
    Missing LoRA files are a non-fatal warning in sd.cpp (tag removed).
  * Conditioning: ``control_net`` and ``ip_adapter`` map to sd.cpp's
    ``--control-net``/``--control-image``/``--control-strength`` and
    ``--ip-adapter``/``--ip-adapter-image``/``--ip-adapter-strength`` (+ its
    required ``--clip_vision`` encoder). Model files are resolved from the ckpt
    dir by convention; a missing model file is a 400 naming the expected path —
    a silently ignored conditioning request would look like an unconstrained
    generation, which is the worst way to fail. net=canny additionally gets
    ``--image-preprocess target=control,canny=true`` (the contract promises
    sidecar-side edge extraction; callers send a photo, not an edge map). With
    no conditioning keys present the command is identical to pre-extension.
  * Page-cache warmer: after a successful render the used files (checkpoint,
    LoRAs, conditioning models) are re-read in chunks once every
    SD_WARM_INTERVAL_S while SD_WARM_KEEP_S hasn't expired, so the next
    sd-cli cold start reads its weights from RAM instead of disk. Reads only —
    never mlock — so warmed pages stay evictable file cache, not pinned
    memory, and cannot recreate the resident-ollama collision the gateway's
    wired budget guards against. A render pauses the warmer (per-request Popen
    would fight it for RAM); it resumes on the next tick.

Endpoints:
  GET  /health  -> {status, bin, default_model, ckpt_dir, control_dir,
                    ip_adapter, clip_vision}
  GET  /status  -> {state: "idle"} | {state: "running", id, step, total, elapsed, cancelled}

  POST /generate -> {prompt, negative_prompt?, width?, height?, steps?,
                     guidance?, seed?, model?, loras?,
                     control_net?: {net: pose|depth|canny, image: b64,
                                    strength? 0.1..1.0},  # absent/0 -> 0.7
                     ip_adapter?: {image: b64, strength? 0.1..1.0}}  # absent/0 -> 0.6
                    -> {image: b64, seed, width, height, seconds} | {error}
  POST /edit     -> same as /generate plus {init_image: b64, strength?}
                   (no conditioning keys — control/ip-adapter are generate-mode)

  GET  /result/<id> -> the finished generation's PNG (raw bytes, "image/png"),
                      404 when unknown — same re-attach semantics as mflux.
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

BIN = os.environ.get("SD_BIN", os.path.expanduser("~/sd.cpp/build/bin/sd-cli"))
CKPT_DIR = os.environ.get("SD_CKPT_DIR", os.path.expanduser("~/mflux-models/sdxl"))
# Per-request override (`model` body key) is a full path to a .safetensors; when
# absent, SD_DEFAULT_MODEL (or the lexicographically first ckpt in SD_CKPT_DIR) runs.
DEFAULT_MODEL = os.environ.get("SD_EXTRA_DEFAULT_MODEL", "")
LORA_MODEL_DIR = os.environ.get("SD_LORA_MODEL_DIR", CKPT_DIR)
# Conditioning models, resolved per request: net -> <CONTROL_MODEL_DIR>/<net>.safetensors;
# IP-Adapter takes its model + the clip-vision image encoder it requires.
CONTROL_MODEL_DIR = os.environ.get("SD_CONTROL_MODEL_DIR", os.path.join(CKPT_DIR, "control"))
IPADAPTER_PATH = os.environ.get("SD_IPADAPTER_PATH", os.path.join(CKPT_DIR, "ip-adapter.safetensors"))
CLIP_VISION_PATH = os.environ.get("SD_CLIP_VISION_PATH", os.path.join(CKPT_DIR, "clip_vision", "model.safetensors"))
EXTRA = os.environ.get("SD_EXTRA_ARGS", "").split()
BIND = os.environ.get("SD_BIND", "127.0.0.1")
PORT = int(os.environ.get("SD_PORT", "8902"))
TOKEN = os.environ.get("SD_TOKEN", "")  # optional shared secret
GEN_TIMEOUT = int(os.environ.get("SD_GEN_TIMEOUT", "3600"))
# Page-cache warmer (see design notes): SD_WARM_KEEP_S=0 disables.
WARM_INTERVAL_S = float(os.environ.get("SD_WARM_INTERVAL_S", "30"))
WARM_KEEP_S = float(os.environ.get("SD_WARM_KEEP_S", "300"))

RESULTS_DIR = os.path.expanduser(os.environ.get("SD_RESULTS_DIR", "~/sd-outputs"))
RESULTS_TTL_HOURS = float(os.environ.get("SD_RESULTS_TTL_HOURS", "24"))
RESULTS_MAX = int(os.environ.get("SD_RESULTS_MAX", "300"))

_lock = threading.Lock()
_cur_lock = threading.Lock()
_current = None  # dict: {id, proc, step, total, started, cancelled, stderr, stdout}

# Page-cache warmer state: the paths the last successful render used and the
# time after which warming stops until the next render re-arms it.
_warm_lock = threading.Lock()
_warm_paths: list[str] = []
_warm_deadline = 0.0


def _render_active() -> bool:
    with _cur_lock:
        return _current is not None


def _touch_file(path: str) -> None:
    """Read the file's blocks into the page cache in large chunks — without
    mlock, so the pages remain evictable file cache the OS can drop under
    pressure instead of pinned memory."""
    try:
        with open(path, "rb") as fh:
            while fh.read(8 * 1024 * 1024):
                pass
    except OSError:
        pass


def _warm_targets(params: dict) -> list[str]:
    """The files this render actually loaded: checkpoint, LoRAs, and the
    conditioning models a conditioned generate resolves."""
    targets = []
    try:
        targets.append(_resolve_model(params))
    except Exception:
        pass
    for lora in params.get("loras") or []:
        targets.append(lora.get("name"))
    ctrl = params.get("control_net")
    if ctrl:
        targets.append(os.path.join(CONTROL_MODEL_DIR, ctrl["net"] + ".safetensors"))
    if params.get("ip_adapter"):
        targets.extend([IPADAPTER_PATH, CLIP_VISION_PATH])
    return targets


def _arm_warm(paths: list) -> None:
    """Arm the warmer for the files a successful render used. Snapshot only —
    the touching happens on the warm thread, WARM_INTERVAL_S apart, until
    WARM_KEEP_S elapses."""
    if WARM_KEEP_S <= 0:
        return
    existing = [p for p in paths if p and os.path.isfile(p)]
    with _warm_lock:
        global _warm_paths, _warm_deadline
        _warm_paths = existing
        _warm_deadline = time.time() + WARM_KEEP_S
    print(f"warm: armed {len(existing)} file(s) for {WARM_KEEP_S:g}s", flush=True)


def _warm_loop() -> None:
    while True:
        time.sleep(WARM_INTERVAL_S)
        with _warm_lock:
            paths = list(_warm_paths)
            deadline = _warm_deadline
        if not paths or time.time() >= deadline or _render_active():
            continue
        t0 = time.time()
        for p in paths:
            _touch_file(p)
        print(f"warm: touched {len(paths)} file(s) in {time.time() - t0:.1f}s", flush=True)

# sd.cpp draws its per-step progress bar on stderr with \r separators
# ("[ 42%|  3/20 ...]" style); same parse as the mflux sidecar's tqdm feed.
_PROGRESS_RE = re.compile(r"(\d+)/(\d+)")


def _read_progress(proc: subprocess.Popen, state: dict) -> None:
    """Drain stderr, parsing per-step N/total into `state` as it streams."""
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
        raise RuntimeError(detail[-800:] or "sd failed")
    if not os.path.exists(out):
        raise RuntimeError("sd exited 0 but wrote no output image")
    with open(out, "rb") as fh:
        return fh.read()


def _sweep_results() -> None:
    """Bounded results store: delete files past the TTL, then the oldest beyond
    a newest-N cap."""
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
    return os.path.join(RESULTS_DIR, str(state["id"]) + ".png")


def _resolve_model(params: dict) -> str:
    model = params.get("model") or DEFAULT_MODEL
    if not model:
        try:
            ckpts = sorted(f for f in os.listdir(CKPT_DIR) if f.endswith(".safetensors"))
        except OSError:
            ckpts = []
        if not ckpts:
            raise RuntimeError(f"no SDXL checkpoint found in {CKPT_DIR}")
        model = os.path.join(CKPT_DIR, ckpts[0])
    return model


# Conditioning contract (img-gen storage.ControlParams/IPAdapterParams): net
# names the map kind; image is base64; strength absent or 0 falls back to the
# sidecar default (0.7 control / 0.6 ip-adapter), 0.1..1.0 passes through.
CONTROL_NETS = ("pose", "depth", "canny")
DEFAULT_CONTROL_STRENGTH = 0.7
DEFAULT_IPADAPTER_STRENGTH = 0.6


def _strength(value, default: float) -> float:
    if value is None:
        return default
    s = float(value)
    if s == 0:
        return default
    if not 0.1 <= s <= 1.0:
        raise ValueError(f"strength must be 0 (sidecar default) or 0.1..1.0, got {s}")
    return s


def _b64_image(value, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"{label} is required (base64)")
    try:
        base64.b64decode(value, validate=True)
    except Exception as exc:
        raise ValueError(f"{label} is not valid base64") from exc
    return value


def _control_ref(ctrl) -> dict:
    if not isinstance(ctrl, dict):
        raise ValueError("control_net must be an object")
    net = ctrl.get("net")
    if net not in CONTROL_NETS:
        raise ValueError(f"control_net.net must be one of {'/'.join(CONTROL_NETS)}, got {net!r}")
    path = os.path.join(CONTROL_MODEL_DIR, net + ".safetensors")
    if not os.path.isfile(path):
        raise ValueError(f"no controlnet model for net {net!r}: expected file {path} (set SD_CONTROL_MODEL_DIR to override)")
    return {
        "net": net,
        "image": _b64_image(ctrl.get("image"), "control_net.image"),
        "strength": _strength(ctrl.get("strength"), DEFAULT_CONTROL_STRENGTH),
    }


def _ip_adapter_ref(ipa) -> dict:
    if not isinstance(ipa, dict):
        raise ValueError("ip_adapter must be an object")
    if not os.path.isfile(IPADAPTER_PATH):
        raise ValueError(f"no ip-adapter model: expected file {IPADAPTER_PATH} (set SD_IPADAPTER_PATH to override)")
    if not os.path.isfile(CLIP_VISION_PATH):
        raise ValueError(f"no clip-vision encoder: expected file {CLIP_VISION_PATH} (set SD_CLIP_VISION_PATH to override)")
    return {
        "image": _b64_image(ipa.get("image"), "ip_adapter.image"),
        "strength": _strength(ipa.get("strength"), DEFAULT_IPADAPTER_STRENGTH),
    }


def run_generation(params: dict, state: dict) -> bytes:
    """Run one sd-cli invocation (txt2img, or img2img with init_image); the binary
    writes the PNG straight to the persisted results path."""
    out = _result_path(state)
    with tempfile.TemporaryDirectory(prefix="sd-") as td:
        cmd = [BIN, "-M", "img_gen", "-m", _resolve_model(params), "--output", out]
        prompt = params["prompt"]
        loras = params.get("loras")
        if loras:
            # stable-diffusion.cpp scans <lora:<path>:<scale>> tags out of the
            # prompt; absolute paths bypass the dir join.
            tags = "".join(f"<lora:{l['name']}:{l.get('scale', 1.0)}>" for l in loras)
            prompt = tags + prompt
        cmd += ["--lora-model-dir", LORA_MODEL_DIR]
        cmd += EXTRA
        cmd += ["--prompt", prompt]
        if params.get("negative_prompt"):
            cmd += ["--negative-prompt", params["negative_prompt"]]
        cmd += ["--width", str(params.get("width", 1024)), "--height", str(params.get("height", 1024))]
        if params.get("steps"):
            cmd += ["--steps", str(params["steps"])]
        if params.get("guidance"):
            cmd += ["--cfg-scale", str(params["guidance"])]
        cmd += ["--seed", str(params["seed"])]
        if params.get("init_image"):
            init = os.path.join(td, "init.png")
            with open(init, "wb") as fh:
                fh.write(base64.b64decode(params["init_image"]))
            cmd += ["--init-img", init, "--strength", str(params.get("strength", 0.4))]
        ctrl = params.get("control_net")
        if ctrl:
            ctl = os.path.join(td, "control.png")
            with open(ctl, "wb") as fh:
                fh.write(base64.b64decode(ctrl["image"]))
            cmd += [
                "--control-net", os.path.join(CONTROL_MODEL_DIR, ctrl["net"] + ".safetensors"),
                "--control-image", ctl,
                "--control-strength", str(ctrl["strength"]),
            ]
            if ctrl["net"] == "canny":
                cmd += ["--image-preprocess", "target=control,canny=true"]
        ipa = params.get("ip_adapter")
        if ipa:
            ref = os.path.join(td, "ip_adapter.png")
            with open(ref, "wb") as fh:
                fh.write(base64.b64decode(ipa["image"]))
            cmd += [
                "--ip-adapter", IPADAPTER_PATH,
                "--clip_vision", CLIP_VISION_PATH,
                "--ip-adapter-image", ref,
                "--ip-adapter-strength", str(ipa["strength"]),
            ]

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
        return not TOKEN or self.headers.get("X-Sd-Token") == TOKEN

    def do_GET(self) -> None:
        if self.path.rstrip("/") == "/health":
            self._send_json(200, {
                "status": "ok",
                "bin": os.path.exists(BIN),
                "default_model": DEFAULT_MODEL or None,
                "ckpt_dir": CKPT_DIR,
                "control_dir": CONTROL_MODEL_DIR,
                "ip_adapter": IPADAPTER_PATH,
                "clip_vision": CLIP_VISION_PATH,
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
        if path not in ("/generate", "/edit"):
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
        except (ValueError, TypeError) as exc:
            msg = str(exc) or "width/height/steps/guidance/seed/strength must be numeric and image inputs present"
            self._send_json(400, {"error": msg, "error_type": "invalid_argument"}); return

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
                img = run_generation(params, state)
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
            # Arming happens here while _lock is still held: the snapshot reads
            # only params (render-complete state), and _current is cleared in
            # the finally below, so the first warm tick can't race the render.
            _arm_warm(_warm_targets(params))
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
            "negative_prompt": body.get("negative_prompt"),
            "width": int(body.get("width", 1024)),
            "height": int(body.get("height", 1024)),
            "steps": int(body["steps"]) if body.get("steps") else None,
            "guidance": float(body["guidance"]) if body.get("guidance") else None,
            "model": body.get("model"),
            "loras": body.get("loras"),
        }
        if control_net := body.get("control_net"):
            common["control_net"] = _control_ref(control_net)
        if ip_adapter := body.get("ip_adapter"):
            common["ip_adapter"] = _ip_adapter_ref(ip_adapter)
        if path == "/edit":
            if not body.get("init_image"):
                raise TypeError("init_image required")
            common["init_image"] = body["init_image"]
            common["strength"] = float(body.get("strength", 0.4))
        return common

    def log_message(self, *args) -> None:  # quiet; the LaunchAgent captures stdout/stderr
        pass


def main() -> None:
    os.makedirs(RESULTS_DIR, exist_ok=True)
    _sweep_results()
    threading.Thread(target=_warm_loop, daemon=True).start()
    default = DEFAULT_MODEL
    if not default:
        try:
            first = sorted(f for f in os.listdir(CKPT_DIR) if f.endswith(".safetensors"))
            default = os.path.join(CKPT_DIR, first[0]) if first else "-"
        except OSError:
            default = "-"
    print(
        f"sd sidecar on {BIND}:{PORT} bin={BIN} ckpt_dir={CKPT_DIR} default_model={default} "
        f"results_dir={RESULTS_DIR}",
        flush=True,
    )
    ThreadingHTTPServer((BIND, PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()