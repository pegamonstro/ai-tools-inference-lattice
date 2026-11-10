#!/usr/bin/env python3
"""DWPose HTTP sidecar — COCO-WholeBody pose extraction behind img-gen's
DWPOSE_URL.

Thin wrapper around the DWPose ONNX pipeline (dwpose_pipeline.py) exposing
exactly the contract img-gen's internal/lattice Client.DWPose speaks, mirroring
the rembg sidecar's HTTP shape. Both models run in-process from a uv venv
(onnxruntime + opencv) and stay resident; weights live on disk under
DWPoseModelDir and load at boot (~370 MB total, few seconds of CPU).

Endpoints:
  GET  /health  -> {status, det_model, pose_model, session_ready}
  POST /pose    -> {image: b64} -> {keypoints: [x,y,conf, ... x17 triplets],
                                    width, height, seconds, person_count, error?}

keypoints are the COCO-17 triplet subset (WholeBody-133 indices 0..16) in
SOURCE-image pixel coordinates, zero-confidence for undetected joints — the
exact array compose.PoseSkeleton renders into openpose ControlNet conditioning.

Configuration (environment):
  DWPose_BIND     bind address (default 0.0.0.0 — tailnet reach, same posture
                  as the rembg/tagger sidecars: unauthenticated)
  DWPose_PORT     port (default 8905 — 8900 speech, 8901 esrgan, 8902 sdxl,
                  8903 rembg, 8904 tagger)
  DWPoseModelDir  directory holding yolox_l.onnx and dw-ll_ucoco_384.onnx
  DWPoseThreaded  "1" (default) runs each request on a thread pool; inference
                  itself is single-flight under a lock like every other sidecar.
"""

from __future__ import annotations

import base64
import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BIND = os.environ.get("DWPose_BIND", "0.0.0.0")
PORT = int(os.environ.get("DWPose_PORT", "8905"))
MODEL_DIR = os.environ.get("DWPoseModelDir", "")

_lock = threading.Lock()        # single-flight inference, like rembg
_session_lock = threading.Lock()
_states = {"det": None, "pose": None}


def get_sessions():
    """Load both ONNX sessions at first use; reused after."""
    global _states
    if _states["det"] is None or _states["pose"] is None:
        with _session_lock:
            if _states["det"] is None or _states["pose"] is None:
                import onnxruntime as ort
                so = ort.SessionOptions()
                so.log_severity_level = 3
                for key, name in (("det", "yolox_l"), ("pose", "dw-ll_ucoco_384")):
                    if _states[key] is None:
                        _states[key] = ort.InferenceSession(
                            os.path.join(MODEL_DIR, name + ".onnx"),
                            sess_options=so,
                            providers=["CPUExecutionProvider"])
    return _states["det"], _states["pose"]


# COCO-WholeBody-133 groups: face 23-90, left hand 91-111, right hand 112-132.
HAND_GROUPS = (range(91, 112), range(112, 133))


def hand_boxes(kps, conf_thr: float = 0.3, pad: float = 0.15) -> list[list[float]]:
    """Normalized [x, y, w, h] boxes around detected hands (WholeBody hand
    keypoints, padded), the region the auto hand-refine masks paint."""
    boxes: list[list[float]] = []
    if kps is None or kps.shape[0] < 133:
        return boxes
    for grp in HAND_GROUPS:
        pts = [(float(kps[i, 0]), float(kps[i, 1]), float(kps[i, 2]))
               for i in grp if kps[i, 2] >= conf_thr]
        if not pts:
            continue
        xs = [p[0] for p in pts]; ys = [p[1] for p in pts]
        x0, x1 = min(xs), max(xs)
        y0, y1 = min(ys), max(ys)
        cx, cy = (x0 + x1) / 2, (y0 + y1) / 2
        w = max(x1 - x0, 40.0)  # never a sliver: hands need a paintable region
        h = max(y1 - y0, 40.0)
        x0, x1 = cx - w / 2 * (1 + pad * 2), cx + w / 2 * (1 + pad * 2)
        y0, y1 = cy - h / 2 * (1 + pad * 2), cy + h / 2 * (1 + pad * 2)
        boxes.append([round(max(0.0, x0), 4), round(max(0.0, y0), 4),
                      round(min(1.0, x1) - max(0.0, x0), 4),
                      round(min(1.0, y1) - max(0.0, y0), 4)])
    return boxes


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
                "det_model": "yolox_l",
                "pose_model": "dw-ll_ucoco_384",
                "endpoints": ["/pose", "/hands"],
                "session_ready": _states["pose"] is not None,
            })
            return
        self._send_json(404, {"error": "not found"})

    def do_POST(self) -> None:
        if self.path not in ("/pose", "/hands"):
            self._send_json(404, {"error": "not found"})
            return
        try:
            n = int(self.headers.get("Content-Length", "0"))
            req = json.loads(self.rfile.read(n))
        except Exception as exc:
            self._send_json(400, {"error": f"bad request: {exc}"})
            return
        raw_b64 = req.get("image", "")
        if not raw_b64:
            self._send_json(400, {"error": "image is required"})
            return

        t0 = time.time()
        try:
            import numpy as np
            from dwpose_pipeline import run_pipeline
            raw = base64.b64decode(raw_b64)
            arr = np.frombuffer(raw, dtype=np.uint8)
            img = cv2_imdecode(arr)
            if img is None:
                self._send_json(400, {"error": "image did not decode to pixels"})
                return
            h, w = img.shape[:2]
            det, pose = get_sessions()
            with _lock:
                boxes, kps = run_pipeline(det, pose, img)
            # COCO-17 triplet subset in source pixels.
            triplets: list[float] = []
            if kps is not None and kps.shape[0] >= 17:
                for i in range(17):
                    triplets += [float(kps[i, 0]), float(kps[i, 1]), float(kps[i, 2])]
            else:
                triplets = [0.0, 0.0, 0.0] * 17
            hands = hand_boxes(kps)
            if self.path == "/hands":
                self._send_json(200, {
                    "boxes": hands,
                    "width": int(w),
                    "height": int(h),
                    "seconds": round(time.time() - t0, 3),
                    "person_count": len(boxes),
                })
                return
            self._send_json(200, {
                "keypoints": triplets,
                "hands": hands,
                "width": int(w),
                "height": int(h),
                "seconds": round(time.time() - t0, 3),
                "person_count": len(boxes),
            })
        except Exception as exc:  # noqa: BLE001 — surfaced to the caller verbatim
            self._send_json(500, {"error": f"pose inference failed: {exc}"})

    def log_message(self, *args) -> None:  # quiet; LaunchAgent captures stdout/stderr
        pass


def cv2_imdecode(arr):
    import cv2
    return cv2.imdecode(arr, cv2.IMREAD_COLOR)


def main() -> None:
    # Eager-load at boot so /health reports session_ready immediately and the
    # first /pose does not pay the multi-second model load.
    get_sessions()
    ThreadingHTTPServer((BIND, PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()