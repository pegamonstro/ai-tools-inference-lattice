#!/usr/bin/env python3
"""RIFE ONNX interpolation pipeline — intermediate frames between anchors.

Vendored for the lattice rife sidecar, against the yuvraj108c/edgetools
rife49.onnx export (RIFE v4.9): img0/img1 float32 [1,3,H,W] RGB in 0..1 plus
timestep [1] in (0,1], output the same shape. The model only consumes
dimensions that are multiples of 32, so frames are edge-padded per call and
the padding is cropped from every output.
"""

from __future__ import annotations

import numpy as np
import onnxruntime as ort

PAD_TO = 32


def _pad_to_multiple(img: np.ndarray) -> tuple[np.ndarray, int, int]:
    """Zero-pad CHW to H,W multiples of 32; return (padded, orig_h, orig_w)."""
    c, h, w = img.shape
    ph = (PAD_TO - h % PAD_TO) % PAD_TO
    pw = (PAD_TO - w % PAD_TO) % PAD_TO
    if ph == 0 and pw == 0:
        return img, h, w
    padded = np.pad(img, ((0, 0), (0, ph), (0, pw)),
                    mode="edge")
    return padded, h, w


def interp_pair(sess: ort.InferenceSession, img0: np.ndarray, img1: np.ndarray,
                multiplier: int) -> list[np.ndarray]:
    """Interpolate one anchor pair. inputs are RGB CHW float32 in 0..1 (any
    size ≥ 32); returns the (multiplier-1) intermediate frames in order
    (t = j/multiplier for j = 1..multiplier-1), each cropped to img0's extent."""
    _, h, w = img0.shape
    p0, _, _ = _pad_to_multiple(img0)
    p1, _, _ = _pad_to_multiple(img1)
    t0 = np.array([0.5], np.float32)  # placeholder; replaced per step
    frames: list[np.ndarray] = []
    for j in range(1, multiplier):
        t0[0] = float(j) / multiplier
        out = sess.run(None, {"img0": p0[None], "img1": p1[None], "timestep": t0})[0]
        frames.append(np.clip(out[0, :, :h, :w], 0.0, 1.0))
    return frames


def interpolate_sequence(sess: ort.InferenceSession,
                         anchors: list[np.ndarray],
                         multiplier: int) -> list[np.ndarray]:
    """Full sequence: count = (n-1)*multiplier+1, anchors included in order.
    anchors are RGB CHW float32 0..1; all intermediates inherit the first
    anchor's dimensions (img-gen's GIF composer fixes one size per animation)."""
    h, w = anchors[0].shape[1:]
    resized: list[np.ndarray] = []
    for a in anchors:
        if a.shape[1] != h or a.shape[2] != w:
            a = _resize_chw(a, h, w)
        resized.append(a)
    out: list[np.ndarray] = []
    for i, a in enumerate(resized[:-1]):
        out.append(a)
        out.extend(interp_pair(sess, a, resized[i + 1], multiplier))
    out.append(resized[-1])
    return out


def _resize_chw(img: np.ndarray, h: int, w: int) -> np.ndarray:
    # cv2 is the only fast path; import lazily so PIL-only environments stay
    # importable for tests.
    try:
        import cv2
        return cv2.resize(img.transpose(1, 2, 0), (w, h),
                          interpolation=cv2.INTER_AREA).transpose(2, 0, 1)
    except ImportError:
        from PIL import Image
        arr = (img.transpose(1, 2, 0) * 255.0).round().astype(np.uint8)
        im = Image.fromarray(arr).resize((w, h), Image.BILINEAR)
        return np.asarray(im, np.float32).transpose(2, 0, 1) / 255.0