"""Stage 3: OCR — PaddleOCR PP-OCRv4 (ch+en, CPU, mkldnn) -> tokens+bboxes+conf.

Import-guarded. Without paddleocr, a deterministic stub OCR is used that
reads sidecar text embedded by the fixture generator (PNG `tEXt` chunk
"ocr_text") so the rest of the pipeline is fully exercised; results are
honestly tagged sim=True (SPEC A scope rule).

Backend seam: OCR goes through a pluggable backend (``get_backend`` /
``set_backend``). Tests inject ``StubBackend`` (or a mock) so the suite is
deterministic offline — no model downloads, no GPU — regardless of whether
paddleocr happens to be importable in the environment.

PaddleOCR version tolerance: paddleocr 3.x removed the legacy ``use_gpu`` /
``use_angle_cls`` / ``show_log`` kwargs (replaced by ``device`` and
``use_textline_orientation``). ``PaddleBackend`` tries the 3.x signature
first and falls back to the legacy 2.x kwargs, so either major version
works at runtime.
"""
from __future__ import annotations

import io
import json
from typing import Protocol

from ..config import get_settings
from .types import OcrResult, OcrToken

try:
    from paddleocr import PaddleOCR  # type: ignore
    _HAVE_PADDLE = True
except ImportError:
    PaddleOCR = None  # type: ignore
    _HAVE_PADDLE = False


class OcrBackend(Protocol):
    """OCR abstraction seam — anything with an ``ocr(png) -> OcrResult``."""

    def ocr(self, png: bytes) -> OcrResult: ...


class PaddleBackend:
    """Real PaddleOCR engine, constructed lazily and version-tolerantly."""

    def __init__(self) -> None:
        self._engine = None

    def _make_engine(self):
        s = get_settings()
        try:
            # paddleocr >= 3.x: use_gpu/show_log removed; CPU via device arg.
            return PaddleOCR(use_textline_orientation=True, lang="ch",
                             device="cpu", enable_mkldnn=s.paddle_use_mkldnn)
        except (TypeError, ValueError):
            # paddleocr 2.x legacy kwargs.
            return PaddleOCR(use_angle_cls=True, lang="ch", use_gpu=False,
                             enable_mkldnn=s.paddle_use_mkldnn, show_log=False)

    def ocr(self, png: bytes) -> OcrResult:
        import numpy as np
        from PIL import Image
        if self._engine is None:
            self._engine = self._make_engine()
        img = np.array(Image.open(io.BytesIO(png)).convert("RGB"))
        try:
            res = self._engine.ocr(img, cls=True)
        except TypeError:
            res = self._engine.ocr(img)  # 3.x dropped the cls kwarg
        tokens: list[OcrToken] = []
        for line in (res[0] if res and res[0] else []):
            bbox, (text, conf) = line[0], line[1]
            tokens.append(OcrToken(text=text, conf=float(conf),
                                   bbox=[[float(x), float(y)] for x, y in bbox]))
        conf_avg = sum(t.conf for t in tokens) / len(tokens) if tokens else 0.0
        return OcrResult(tokens=tokens, conf_avg=conf_avg,
                         engine="paddleocr:PP-OCRv4", sim=False)


class StubBackend:
    """Deterministic dev OCR: fixture PNGs carry ground-truth text in a tEXt
    chunk (key 'ocr_text', JSON list of {text, conf}). Real scans without the
    chunk yield no tokens — never fabricated."""

    def ocr(self, png: bytes) -> OcrResult:
        from PIL import Image
        img = Image.open(io.BytesIO(png))
        raw = img.info.get("ocr_text")
        tokens: list[OcrToken] = []
        if raw:
            for i, item in enumerate(json.loads(raw)):
                tokens.append(OcrToken(text=item["text"], conf=float(item.get("conf", 0.95)),
                                       bbox=[[0, i * 20], [200, i * 20], [200, i * 20 + 18], [0, i * 20 + 18]]))
        conf_avg = sum(t.conf for t in tokens) / len(tokens) if tokens else 0.0
        return OcrResult(tokens=tokens, conf_avg=conf_avg, engine="stub:sidecar", sim=True)


_backend: OcrBackend | None = None


def get_backend() -> OcrBackend:
    """Lazy default backend: PaddleOCR when importable, else the stub."""
    global _backend
    if _backend is None:
        _backend = PaddleBackend() if _HAVE_PADDLE else StubBackend()
    return _backend


def set_backend(backend: OcrBackend | None) -> None:
    """Inject a backend (tests/dev); ``None`` restores lazy auto-selection."""
    global _backend
    _backend = backend


def run_ocr(page_png: bytes) -> OcrResult:
    return get_backend().ocr(page_png)


OCR_ENGINE = "paddleocr:PP-OCRv4" if _HAVE_PADDLE else "stub:sidecar"
