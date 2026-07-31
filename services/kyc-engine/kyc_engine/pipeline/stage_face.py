"""Stage 6: face match — insightface buffalo_l (onnxruntime CPU): detect +
embed ID photo & selfie, cosine similarity.

Import-guarded. Without insightface, a deterministic stub embedder is used
(fixture faces carry an 'embed' PNG tEXt sidecar); sim=True is set honestly.
Low-light selfie -> brightness penalty (SPEC A §6).
"""
from __future__ import annotations

import io
import json
import math
from typing import Optional

import numpy as np

try:
    from insightface.app import FaceAnalysis  # type: ignore
    _HAVE_IFACE = True
except ImportError:
    _HAVE_IFACE = False

_app = None
FACE_ENGINE = "insightface:buffalo_l" if _HAVE_IFACE else "stub:sidecar"


def _face_app():
    global _app
    if _app is None:
        _app = FaceAnalysis(name="buffalo_l", providers=["CPUExecutionProvider"])
        _app.prepare(ctx_id=-1, det_size=(640, 640))
    return _app


def _cosine(a: np.ndarray, b: np.ndarray) -> float:
    na, nb = np.linalg.norm(a), np.linalg.norm(b)
    if na == 0 or nb == 0:
        return 0.0
    return float(np.dot(a, b) / (na * nb))


def _brightness(png: bytes) -> float:
    from PIL import Image
    return float(np.asarray(Image.open(io.BytesIO(png)).convert("L"), dtype=np.float32).mean()) / 255.0


def embed(png: bytes) -> Optional[np.ndarray]:
    if _HAVE_IFACE:
        img = np.asarray(__import__("PIL.Image", fromlist=["open"]).open(io.BytesIO(png)).convert("RGB"))
        faces = _face_app().get(img)
        if not faces:
            return None
        return np.asarray(max(faces, key=lambda f: f.det_score).normed_embedding, dtype=np.float32)
    from PIL import Image
    raw = Image.open(io.BytesIO(png)).info.get("embed")
    if not raw:
        return None
    return np.asarray(json.loads(raw), dtype=np.float32)


def match_faces(id_photo_png: bytes, selfie_png: bytes) -> dict:
    """Cosine between ID photo and selfie embeddings; low-light penalty."""
    e1, e2 = embed(id_photo_png), embed(selfie_png)
    sim = _HAVE_IFACE is False  # engine tag, not result
    if e1 is None or e2 is None:
        return {"cosine": 0.0, "faces_found": [e1 is not None, e2 is not None],
                "engine": FACE_ENGINE, "sim": sim, "brightness": _brightness(selfie_png)}
    cos = _cosine(e1, e2)
    bright = _brightness(selfie_png)
    penalized = cos
    if bright < 0.25:  # low-light selfie -> penalize, step_up territory
        penalized = cos * (0.5 + bright)
    return {"cosine": penalized, "cosine_raw": cos, "faces_found": [True, True],
            "engine": FACE_ENGINE, "sim": sim, "brightness": bright,
            "low_light": bright < 0.25}
