#!/usr/bin/env python3
"""DWPose ONNX inference pipeline — person detect + 133-keypoint WholeBody pose.

Vendored for the lattice dwpose sidecar. Follows the reference DWPose ONNX
inference (mmpose's dw-ll_ucoco_384 export behind a YOLOX-l detector, the
yzd-v/DWPose model zoo): YOLOX multi-level decode happens client-side over the
graph's raw per-anchor rows, and the pose SimCC distributions decode by
integral argmax then un-affine back into source-image pixels.

Keypoint layout: COCO-WholeBody-133, whose first 17 indices are the plain
COCO-17 body keypoints (nose .. right ankle) — exactly what img-gen's
compose.PoseSkeleton consumes as [x, y, conf] triplets.
"""

from __future__ import annotations

import cv2
import numpy as np
import onnxruntime as ort

# det config (yzd-v/DWPose, per the controlnet_aux DWPose reference)
DET_INPUT_W = DET_INPUT_H = 640
DET_MEAN = np.array([123.675, 116.28, 103.53], dtype=np.float32)
DET_STD = np.array([58.395, 57.12, 57.375], dtype=np.float32)
DET_NMS_THR = 0.45
DET_SCORE_THR = 0.3

# pose config
POSE_INPUT_W = 288
POSE_INPUT_H = 384
POSE_SCORE_THR = 0.5  # mmpose's default kpt_thr for the DWPose detector
SIMCC_SPLIT_RATIO = 2.0  # simcc bins are 2x the input dims (576/288, 768/384)
COCO17 = list(range(17))  # WholeBody-133 body indices are COCO-17 order

# Both onnx graphs are RGB + ImageNet-stat normalized (mmpose training config);
# cv2 decodes BGR, so every model input goes through _model_input.
POSE_MEAN = np.array([0.485, 0.456, 0.406], dtype=np.float32)
POSE_STD = np.array([0.229, 0.224, 0.225], dtype=np.float32)


def _model_input(img_bgr: np.ndarray) -> np.ndarray:
    """BGR uint8 image → RGB float32 NCHW with ImageNet normalization."""
    rgb = cv2.cvtColor(img_bgr, cv2.COLOR_BGR2RGB)
    return ((rgb.astype(np.float32) / 255.0 - POSE_MEAN) / POSE_STD).transpose(2, 0, 1)[None]


def _letterbox(img: np.ndarray, size: int) -> tuple[np.ndarray, float, int, int]:
    h, w = img.shape[:2]
    scale = min(size / w, size / h)
    nw, nh = int(round(w * scale)), int(round(h * scale))
    resized = cv2.resize(img, (nw, nh), interpolation=cv2.INTER_LINEAR)
    canvas = np.full((size, size, 3), 114, dtype=np.uint8)
    ox, oy = (size - nw) // 2, (size - nh) // 2
    canvas[oy:oy + nh, ox:ox + nw] = resized
    return canvas, scale, ox, oy


def detect_persons(sess: ort.InferenceSession,
                   img: np.ndarray) -> list[tuple[float, float, float, float]]:
    """Run the detector; return person boxes (x1, y1, x2, y2) in source pixels,
    score- and NMS-filtered."""
    h, w = img.shape[:2]
    canvas, scale, ox, oy = _letterbox(img, DET_INPUT_W)
    inp = _model_input(canvas)

    raw = sess.run(None, {"images": inp})[0][0]  # (8400, 85)

    # YOLOX multi-level decode: raw rows carry (reg_xy_reg_wh, obj, cls).
    grids = []
    strides = []
    for level, (size_l, stride) in enumerate(
            ((DET_INPUT_W // 8, 8), (DET_INPUT_W // 16, 16), (DET_INPUT_W // 32, 32))):
        ys, xs = np.meshgrid(np.arange(size_l), np.arange(size_l), indexing="ij")
        grids.append(np.stack((xs, ys), axis=2).reshape(-1, 2))
        strides.append(np.full(size_l * size_l, stride, dtype=np.float32))
    grids = np.concatenate(grids).astype(np.float32)
    strides = np.concatenate(strides)

    xy = (raw[:, :2] + grids) * strides[:, None]
    wh = np.exp(raw[:, 2:4]) * strides[:, None]
    cls = raw[:, 5:]
    # (n,1) objectness × (n,) class max — keep the outer shape EXPLICIT,
    # since (n,) * (n,1) silently broadcasts into a (n,n) matrix and
    # scores[:,0] becomes anchor-0's objectness row.
    scores = (raw[:, 4] * cls.max(axis=1))[:, None]
    picked = _nms_dets(np.concatenate([xy - wh / 2, xy + wh / 2], axis=1),
                       scores, cls, DET_SCORE_THR, DET_NMS_THR)

    boxes = []
    for x1, y1, x2, y2 in picked:
        # undo letterbox; keep persons only (COCO class 0)
        sx1, sy1 = (x1 - ox) / scale, (y1 - oy) / scale
        sx2, sy2 = (x2 - ox) / scale, (y2 - oy) / scale
        if sx2 <= sx1 or sy2 <= sy1:
            continue
        boxes.append((
            max(0.0, sx1), max(0.0, sy1),
            min(float(w) - 1, sx2), min(float(h) - 1, sy2),
        ))
    return boxes


def _nms_dets(boxes: np.ndarray, scores: np.ndarray, cls: np.ndarray,
              score_thr: float, nms_thr: float) -> np.ndarray:
    """Class-aware NMS on the person class (COCO 0); returns xyxy rows."""
    cand = scores[:, 0] > score_thr
    b, s, c = boxes[cand], scores[cand, 0], cls[cand].argmax(axis=1)
    keep_mask = c == 0
    b, s = b[keep_mask], s[keep_mask]
    if len(b) == 0:
        return np.empty((0, 4), np.float32)
    order = s.argsort()[::-1]
    keep: list[int] = []
    while order.size:
        i = order[0]
        keep.append(i)
        if order.size == 1:
            break
        rest = order[1:]
        xx1 = np.maximum(b[i, 0], b[rest, 0])
        yy1 = np.maximum(b[i, 1], b[rest, 1])
        xx2 = np.minimum(b[i, 2], b[rest, 2])
        yy2 = np.minimum(b[i, 3], b[rest, 3])
        inter = np.maximum(0.0, xx2 - xx1) * np.maximum(0.0, yy2 - yy1)
        area_i = (b[i, 2] - b[i, 0]) * (b[i, 3] - b[i, 1])
        area_r = (b[rest, 2] - b[rest, 0]) * (b[rest, 3] - b[rest, 1])
        iou = inter / (area_i + area_r - inter + 1e-9)
        order = rest[iou <= nms_thr]
    return np.stack([(b[i][0:4]) for i in keep])


def estimate_pose(sess: ort.InferenceSession, img: np.ndarray,
                  box: tuple[float, float, float, float]
                  ) -> tuple[np.ndarray, np.ndarray]:
    """Run the pose model on the largest person box. Returns (133, 3) keypoints
    in source pixels with confidences, or (None, None) on degenerate crops."""
    x1, y1, x2, y2 = box
    cx, cy = (x1 + x2) / 2, (y1 + y2) / 2
    bw, bh = max(float(x2 - x1), 1.0), max(float(y2 - y1), 1.0)
    # mmpose top-down affine: the 1.25-padded box maps ANISOTROPICALLY onto
    # the exact input resolution (a square-warp jumbles the pose — verified
    # visually, which is why this is not the isotropic variant).
    sx = POSE_INPUT_W / (bw * 1.25)
    sy = POSE_INPUT_H / (bh * 1.25)
    src = np.array([
        [cx, cy],
        [cx + 0.5, cy],
        [cx, cy + 0.5],
    ], dtype=np.float32)
    dst = np.array([
        [POSE_INPUT_W / 2, POSE_INPUT_H / 2],
        [POSE_INPUT_W / 2 + 0.5 * sx, POSE_INPUT_H / 2],
        [POSE_INPUT_W / 2, POSE_INPUT_H / 2 + 0.5 * sy],
    ], dtype=np.float32)
    m = cv2.getAffineTransform(src, dst)
    warp = cv2.warpAffine(img, m, (POSE_INPUT_W, POSE_INPUT_H),
                          flags=cv2.INTER_LINEAR)
    inp = _model_input(warp)

    simcc_x, simcc_y = sess.run(None, {"input": inp})
    # SimCC decode: integral (soft-argmax) over each axis's distribution —
    # bins map to input pixels via the split ratio (576/2 = 288, 768/2 = 384).
    pts, px_peak, py_peak = _simcc_decode(simcc_x[0], simcc_y[0])

    # Un-affine back to source pixels.
    m_inv = cv2.invertAffineTransform(m)
    src_pts = cv2.transform(pts[None, :, :], m_inv)[0]

    # Confidence = product of the normalized distribution peaks, in [0,1]:
    # a sharp confident joint peaks near 1 on both axes.
    conf = np.clip(px_peak * py_peak, 0.0, 1.0)
    return np.concatenate([src_pts, conf[:, None]], axis=1).astype(np.float32)


def _simcc_decode(simcc_x: np.ndarray, simcc_y: np.ndarray) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Integral (soft-argmax) SimCC decode → ((n, 2) input-pixel coords,
    (n,) per-axis normalized distribution peaks)."""
    wx, pkx = _integral(simcc_x)
    wy, pky = _integral(simcc_y)
    return np.stack([wx / SIMCC_SPLIT_RATIO, wy / SIMCC_SPLIT_RATIO], axis=1), pkx, pky


def _integral(d: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    """Distribution-normalized expectation and peak over the bin axis,
    ((n,) coordinates, (n,) peaks in [0,1])."""
    e = np.exp(d - d.max(axis=1, keepdims=True))
    e /= e.sum(axis=1, keepdims=True) + 1e-9
    bins = np.arange(d.shape[1], dtype=np.float32)
    return (e * bins[None, :]).sum(axis=1), e.max(axis=1)


def run_pipeline(det: ort.InferenceSession, pose: ort.InferenceSession,
                 img_bgr: np.ndarray) -> tuple[list, np.ndarray]:
    """Full detect+pose on one BGR image. Returns (boxes, keypoints) where
    keypoints is (133, 3) for the LARGEST person (conf>0 filtered upstream) or
    (0, 3) when no person was found."""
    boxes = detect_persons(det, img_bgr)
    if not boxes:
        return [], np.zeros((0, 3), np.float32)
    area = [(x2 - x1) * (y2 - y1) for x1, y1, x2, y2 in boxes]
    main = boxes[int(np.argmax(area))]
    kps = estimate_pose(pose, img_bgr, main)
    return boxes, kps