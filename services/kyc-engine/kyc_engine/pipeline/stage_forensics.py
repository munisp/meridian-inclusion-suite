"""Stage 5: forensics — VLM (Ollama) + classical signals (ELA, noise, exif).

Classical forensics are REAL numpy/PIL implementations:
- ELA (error level analysis): re-save at q=90, per-region mean absolute
  error; flag regions whose z-score exceeds the configured threshold.
- Noise variance: Laplacian variance; spliced/compressed regions shift it.
- EXIF: real camera documents usually carry metadata (weak signal).

Ollama down -> classical-only, degraded=True, tamper score from classical
signals only (SPEC A §6).
"""
from __future__ import annotations

import io
from typing import Any

import numpy as np

from ..adapters.ollama_vlm import OllamaDown, get_vlm
from ..config import get_settings

ELA_QUALITY = 90
GRID = 8  # 8x8 ELA regions


def ela_region_means(png: bytes) -> np.ndarray:
    from PIL import Image
    img = Image.open(io.BytesIO(png)).convert("RGB")
    buf = io.BytesIO()
    img.save(buf, format="JPEG", quality=ELA_QUALITY)
    resaved = Image.open(io.BytesIO(buf.getvalue())).convert("RGB")
    diff = np.abs(np.asarray(img, dtype=np.float32) - np.asarray(resaved, dtype=np.float32))
    diff = diff.mean(axis=2)
    h, w = diff.shape
    gh, gw = max(h // GRID, 1), max(w // GRID, 1)
    regions = np.zeros((GRID, GRID), dtype=np.float32)
    for i in range(GRID):
        for j in range(GRID):
            regions[i, j] = diff[i * gh:(i + 1) * gh, j * gw:(j + 1) * gw].mean()
    return regions


def ela_max_zscore(png: bytes) -> float:
    """Max |z| over the region grid: spliced regions deviate from the
    document's uniform compression-error profile in EITHER direction."""
    r = ela_region_means(png)
    mu, sigma = float(r.mean()), float(r.std())
    if sigma < 1e-6:
        return 0.0
    return float(np.abs(r - mu).max() / sigma)


def noise_variance(png: bytes) -> float:
    from PIL import Image
    img = np.asarray(Image.open(io.BytesIO(png)).convert("L"), dtype=np.float32)
    lap = (-4 * img[1:-1, 1:-1] + img[:-2, 1:-1] + img[2:, 1:-1]
           + img[1:-1, :-2] + img[1:-1, 2:])
    return float(lap.var())


def has_exif(png: bytes) -> bool:
    from PIL import Image
    try:
        return bool(Image.open(io.BytesIO(png)).getexif())
    except Exception:
        return False


def classical_tamper_score(png: bytes) -> dict[str, Any]:
    """Combine classical signals into a 0-1 tamper score."""
    s = get_settings()
    z = ela_max_zscore(png)
    nv = noise_variance(png)
    exif = has_exif(png)
    # ELA z beyond threshold is the dominant signal; very low noise variance
    # on a 'scan' suggests digital compositing; missing EXIF is weak evidence.
    score = 0.0
    signals: list[str] = []
    if z > s.ela_z_flag:
        score += 0.65
        signals.append(f"ela_zscore:{z:.2f}>{s.ela_z_flag}")
    elif z > s.ela_z_flag * 0.7:
        score += 0.3
        signals.append(f"ela_zscore_elevated:{z:.2f}")
    if nv < 5.0:
        score += 0.2
        signals.append(f"noise_var_low:{nv:.2f}")
    if not exif:
        score += 0.05
        signals.append("exif_missing")
    return {"tamper_score": min(score, 1.0), "ela_z": z, "noise_var": nv,
            "exif": exif, "signals": signals}


def run_forensics(png: bytes) -> dict[str, Any]:
    """VLM + classical. Ollama down -> degraded classical-only."""
    classical = classical_tamper_score(png)
    out: dict[str, Any] = {"classical": classical, "vlm": None,
                           "degraded": False, "sim": False}
    try:
        vlm = get_vlm().analyze(png)
        out["vlm"] = vlm
        vlm_score = 1.0 - float(vlm.get("template_match") or 1.0)
        vlm_score = max(vlm_score, min(1.0, 0.2 * len(vlm.get("tamper_signals") or [])))
        out["tamper_score"] = min(1.0, 0.5 * classical["tamper_score"] + 0.5 * vlm_score)
    except OllamaDown as e:
        out["degraded"] = True
        out["degraded_reason"] = str(e)
        out["tamper_score"] = classical["tamper_score"]
    return out
