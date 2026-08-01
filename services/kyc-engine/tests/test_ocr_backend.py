"""Unit: OCR backend seam + PaddleOCR version-tolerant construction."""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from kyc_engine.pipeline import stage_ocr
from kyc_engine.pipeline.types import OcrResult, OcrToken
from tests.make_fixtures import nin_slip


def test_backend_seam_mock():
    """A mock backend injected via set_backend is used by run_ocr."""

    class Fake:
        def ocr(self, png: bytes) -> OcrResult:
            return OcrResult(tokens=[OcrToken(text="MOCK", conf=0.99,
                                              bbox=[[0, 0], [1, 0], [1, 1], [0, 1]])],
                             conf_avg=0.99, engine="mock", sim=True)

    stage_ocr.set_backend(Fake())
    try:
        res = stage_ocr.run_ocr(b"not-a-png")
        assert res.engine == "mock" and res.tokens[0].text == "MOCK"
    finally:
        stage_ocr.set_backend(None)


def test_stub_backend_reads_sidecar():
    res = stage_ocr.StubBackend().ocr(nin_slip("12345678901"))
    assert res.sim is True
    assert any("12345678901" in t.text for t in res.tokens)


def test_paddle_construction_accepts_3x_signature(monkeypatch):
    """paddleocr 3.x (no use_gpu/show_log) constructs on the first attempt."""
    calls = []

    class FakePaddle3x:
        def __init__(self, **kwargs):
            if "use_gpu" in kwargs or "show_log" in kwargs:
                raise TypeError("unexpected keyword argument 'use_gpu'")
            calls.append(kwargs)

    monkeypatch.setattr(stage_ocr, "PaddleOCR", FakePaddle3x)
    engine = stage_ocr.PaddleBackend()._make_engine()
    assert isinstance(engine, FakePaddle3x)
    assert len(calls) == 1 and "device" in calls[0]


def test_paddle_construction_falls_back_to_2x(monkeypatch):
    """paddleocr 2.x rejects the 3.x kwargs (device/use_textline_orientation);
    the backend must fall back to the legacy signature."""
    calls = []

    class FakePaddle2x:
        def __init__(self, **kwargs):
            calls.append(kwargs)
            if "device" in kwargs or "use_textline_orientation" in kwargs:
                raise TypeError("unexpected keyword argument 'device'")

        def ocr(self, img, cls=True):
            return [[([[0, 0], [1, 0], [1, 1], [0, 1]], ("TXT", 0.9))]]

    monkeypatch.setattr(stage_ocr, "PaddleOCR", FakePaddle2x)
    engine = stage_ocr.PaddleBackend()._make_engine()
    assert isinstance(engine, FakePaddle2x)
    # new 3.x-style signature tried first, legacy fallback second
    assert "use_gpu" not in calls[0] and "device" in calls[0]
    assert "use_gpu" in calls[1]
