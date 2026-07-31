"""Stage 3: OCR — PaddleOCR PP-OCRv4 (ch+en, CPU, mkldnn) -> tokens+bboxes+conf.

Import-guarded. Without paddleocr, a deterministic stub OCR is used that
reads sidecar text embedded by the fixture generator (PNG `tEXt` chunk
"ocr_text") so the rest of the pipeline is fully exercised; results are
honestly tagged sim=True (SPEC A scope rule).
"""
from __future__ import annotations

import io
import json

from ..config import get_settings
from .types import OcrResult, OcrToken

try:
    from paddleocr import PaddleOCR  # type: ignore
    _HAVE_PADDLE = True
except ImportError:
    _HAVE_PADDLE = False

_ocr_engine = None


def _engine():
    global _ocr_engine
    if _ocr_engine is None:
        s = get_settings()
        _ocr_engine = PaddleOCR(use_angle_cls=True, lang="ch", use_gpu=False,
                                enable_mkldnn=s.paddle_use_mkldnn, show_log=False)
    return _ocr_engine


def _paddle_ocr(png: bytes) -> OcrResult:
    import numpy as np
    from PIL import Image
    img = np.array(Image.open(io.BytesIO(png)).convert("RGB"))
    res = _engine().ocr(img, cls=True)
    tokens: list[OcrToken] = []
    for line in (res[0] if res and res[0] else []):
        bbox, (text, conf) = line[0], line[1]
        tokens.append(OcrToken(text=text, conf=float(conf), bbox=[[float(x), float(y)] for x, y in bbox]))
    conf_avg = sum(t.conf for t in tokens) / len(tokens) if tokens else 0.0
    return OcrResult(tokens=tokens, conf_avg=conf_avg, engine="paddleocr:PP-OCRv4", sim=False)


def _stub_ocr(png: bytes) -> OcrResult:
    """Deterministic dev OCR: fixture PNGs carry ground-truth text in a tEXt
    chunk (key 'ocr_text', JSON list of {text, conf}). Real scans without the
    chunk yield no tokens — never fabricated."""
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


def run_ocr(page_png: bytes) -> OcrResult:
    if _HAVE_PADDLE:
        return _paddle_ocr(page_png)
    return _stub_ocr(page_png)


OCR_ENGINE = "paddleocr:PP-OCRv4" if _HAVE_PADDLE else "stub:sidecar"
