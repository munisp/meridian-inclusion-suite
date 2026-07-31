"""Unit: classical forensics (ELA/noise — REAL numpy) + face match."""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from kyc_engine.pipeline.stage_face import match_faces
from kyc_engine.pipeline.stage_forensics import (classical_tamper_score,
                                                 ela_max_zscore, noise_variance,
                                                 run_forensics)
from tests.make_fixtures import cac_cert, face_pair, valid_rc


def test_clean_doc_low_tamper():
    png = cac_cert(valid_rc("123456"))
    r = classical_tamper_score(png)
    assert r["tamper_score"] < 0.4
    assert r["ela_z"] <= 2.5


def test_tampered_doc_ela_flag():
    png = cac_cert(valid_rc("123456"), tamper=True)
    z = ela_max_zscore(png)
    r = classical_tamper_score(png)
    assert z > 2.5
    assert r["tamper_score"] >= 0.4
    assert any("ela" in s for s in r["signals"])


def test_noise_variance_positive():
    assert noise_variance(cac_cert(valid_rc("123456"))) >= 0.0


def test_run_forensics_degraded_when_ollama_down():
    # no ollama in test env -> degraded classical-only path (SPEC A §6)
    r = run_forensics(cac_cert(valid_rc("123456")))
    assert r["degraded"] is True
    assert r["vlm"] is None
    assert 0.0 <= r["tamper_score"] <= 1.0


def test_face_match_pair():
    id_png, sf_png = face_pair(match=True)
    r = match_faces(id_png, sf_png)
    assert r["faces_found"] == [True, True]
    assert r["cosine"] >= 0.45   # buffalo_l pass band per SPEC A §5


def test_face_nonmatch_pair():
    id_png, _ = face_pair(match=True)
    _, sf_bad = face_pair(match=False)
    r = match_faces(id_png, sf_bad)
    assert r["cosine"] < 0.32    # reject band


def test_face_low_light_penalty():
    id_png, sf_png = face_pair(match=True)
    # crush selfie brightness
    import io
    import numpy as np
    from PIL import Image, PngImagePlugin
    import json
    img = Image.open(io.BytesIO(sf_png)).convert("RGB")
    dark = Image.fromarray((np.asarray(img) * 0.1).astype("uint8"))
    buf = io.BytesIO()
    meta = PngImagePlugin.PngInfo()
    meta.add_text("embed", img.info.get("embed", json.dumps([0]*64)))
    dark.save(buf, format="PNG", pnginfo=meta)
    r = match_faces(id_png, buf.getvalue())
    assert r["low_light"] is True
    assert r["cosine"] < r["cosine_raw"]
