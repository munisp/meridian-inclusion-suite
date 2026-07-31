"""Synthetic fixture generator (SPEC A §7) — NO real PII.

Generates deterministic synthetic documents as PNGs. Since PaddleOCR is not
guaranteed in dev/CI, ground-truth text is embedded in a PNG tEXt chunk
('ocr_text', JSON) consumed by the stub OCR; pixels also contain rendered
text so the real PaddleOCR path works on the same fixtures.
"""
from __future__ import annotations

import io
import json
import sys
from pathlib import Path

import numpy as np
from PIL import Image, ImageDraw, ImageFilter, PngImagePlugin

FIX = Path(__file__).parent / "fixtures"


def _png(lines: list[str], conf: float = 0.95, size=(640, 400), seed: int = 0) -> bytes:
    """Render a document page. The base image is given a uniform JPEG error
    profile (like a real scan/photo) so ELA forensics behave realistically;
    pages are text-dense (real IDs/certs are), which keeps clean-fixture ELA
    z-scores below the 2.5 flag threshold."""
    img = Image.new("RGB", size, (245, 244, 238))
    d = ImageDraw.Draw(img)
    y = 16
    for ln in lines:
        d.text((30, y), ln, fill=(20, 20, 20))
        y += 30
    # fill the page with alpha-only body filler (a real cert is text-dense)
    filler = "HEREBY CERTIFIED THAT THE ABOVE NAMED COMPANY IS DULY INCORPORATED"
    while y < size[1] - 24:
        d.text((30, y), filler[: 40 + (y % 30)], fill=(60, 60, 60))
        y += 30
    arr = np.asarray(img, dtype=np.int16)
    arr += np.random.default_rng(seed).integers(-6, 7, arr.shape)
    img = Image.fromarray(np.clip(arr, 0, 255).astype(np.uint8))
    img = img.filter(ImageFilter.GaussianBlur(1.0))
    buf = io.BytesIO()
    img.save(buf, format="JPEG", quality=80)   # uniform compression profile
    img = Image.open(io.BytesIO(buf.getvalue())).convert("RGB")
    out = io.BytesIO()
    meta = PngImagePlugin.PngInfo()
    meta.add_text("ocr_text", json.dumps([{"text": ln, "conf": conf} for ln in lines]))
    img.save(out, format="PNG", pnginfo=meta)
    return out.getvalue()


def nin_slip(nin: str = "12345678901", conf: float = 0.95, seed: int = 1) -> bytes:
    return _png(["NATIONAL IDENTITY MANAGEMENT COMMISSION", f"NIN {nin}",
                 "Surname ADEYEMI", "First Name CHIAMAKA",
                 "Date of Birth 1990-05-14"], conf=conf, seed=seed)


def cac_cert(rc: str = "RC1234562", conf: float = 0.95, seed: int = 2,
             tamper: bool = False) -> bytes:
    lines = ["CORPORATE AFFAIRS COMMISSION", f"RC Number {rc}",
             "Company Name MERIDIAN TEST VENTURES LTD", "Registered 2019-03-22",
             "Director: ADAEZE OKAFOR", "Director: IBRAHIM MUSA"]
    png = _png(lines, conf=conf, seed=seed)
    if tamper:
        # splice: crisp patch (never compressed) pasted into the uniformly-
        # compressed base -> strong ELA outlier region
        img = Image.open(io.BytesIO(png)).convert("RGB")
        patch = Image.new("RGB", (300, 30), (245, 244, 238))
        ImageDraw.Draw(patch).text((5, 6), "RC Number RC9999999", fill=(20, 20, 20))
        img.paste(patch, (150, 166))
        buf = io.BytesIO()
        meta = PngImagePlugin.PngInfo()
        meta.add_text("ocr_text", json.dumps([{"text": ln, "conf": conf} for ln in lines]))
        img.save(buf, format="PNG", pnginfo=meta)
        png = buf.getvalue()
    return png


def valid_rc(body6: str) -> str:
    """Build a standard-format RC number (no checksum exists on CAC numbers)."""
    return "RC" + body6


def face_pair(match: bool = True, seed: int = 10) -> tuple[bytes, bytes]:
    """Synthetic 'faces': gradient blobs with an 'embed' sidecar vector.
    Match pairs share a base vector; non-match pairs are orthogonal-ish."""
    rng = np.random.default_rng(seed)
    base = rng.normal(size=64)
    base /= np.linalg.norm(base)
    other = rng.normal(size=64)
    other /= np.linalg.norm(other)
    e1, e2 = base, (base + 0.05 * rng.normal(size=64)) if match else other
    e2 = e2 / np.linalg.norm(e2)

    def _img(vec, seed2):
        im = Image.new("RGB", (160, 160), (220, 200, 180))
        d = ImageDraw.Draw(im)
        d.ellipse((40, 30, 120, 120), fill=(200, 170, 140))
        d.ellipse((60, 60, 75, 75), fill=(0, 0, 0))
        d.ellipse((90, 60, 105, 75), fill=(0, 0, 0))
        arr = np.asarray(im, dtype=np.int16)
        arr += np.random.default_rng(seed2).integers(-4, 5, arr.shape)
        im = Image.fromarray(np.clip(arr, 0, 255).astype(np.uint8))
        buf = io.BytesIO()
        meta = PngImagePlugin.PngInfo()
        meta.add_text("embed", json.dumps([round(float(x), 6) for x in vec]))
        im.save(buf, format="PNG", pnginfo=meta)
        return buf.getvalue()

    return _img(e1, seed * 2), _img(e2, seed * 2 + 1)


def main():
    FIX.mkdir(parents=True, exist_ok=True)
    (FIX / "nin_slip.png").write_bytes(nin_slip())
    (FIX / "cac_cert.png").write_bytes(cac_cert(valid_rc("123456")))
    (FIX / "cac_cert_tampered.png").write_bytes(cac_cert(valid_rc("123456"), tamper=True))
    (FIX / "nin_lowconf.png").write_bytes(nin_slip(conf=0.6, seed=5))
    m_id, m_sf = face_pair(match=True)
    n_id, n_sf = face_pair(match=False)
    (FIX / "face_id.png").write_bytes(m_id)
    (FIX / "face_match.png").write_bytes(m_sf)
    (FIX / "face_nonmatch.png").write_bytes(n_sf)
    print(f"fixtures written to {FIX}")


if __name__ == "__main__":
    main()
