"""Stage 1: ingest — mime sniff, sha256, WORM store original (SPEC A §4).

>20MB scans are downscaled to 2000px longest edge before downstream stages
(SPEC A §6) but the ORIGINAL bytes are what goes to the WORM bucket.
"""
from __future__ import annotations

import io

from ..adapters.storage import get_storage, sha256_hex
from ..config import get_settings
from ..models.db import get_session
from ..models.tables import KycDocument

MAGIC = {
    b"\x89PNG": "image/png",
    b"\xff\xd8\xff": "image/jpeg",
    b"%PDF": "application/pdf",
}


def sniff_mime(data: bytes) -> str:
    for magic, mime in MAGIC.items():
        if data.startswith(magic):
            return mime
    return "application/octet-stream"


def maybe_downscale(data: bytes, mime: str) -> bytes:
    """SPEC §6: downscale huge scans for the CPU pipeline."""
    s = get_settings()
    if len(data) <= s.max_scan_bytes or not mime.startswith("image/"):
        return data
    from PIL import Image
    img = Image.open(io.BytesIO(data))
    w, h = img.size
    edge = max(w, h)
    if edge <= s.downscale_long_edge:
        return data
    scale = s.downscale_long_edge / edge
    img = img.resize((int(w * scale), int(h * scale)))
    buf = io.BytesIO()
    img.save(buf, format="PNG")
    return buf.getvalue()


def ingest(case_id: str, filename: str, data: bytes, doc_type: str = "unknown") -> KycDocument:
    s = get_settings()
    mime = sniff_mime(data)
    digest = sha256_hex(data)
    key = f"{case_id}/{digest}"
    storage = get_storage()
    if not storage.exists(s.minio_bucket_raw, key):  # idempotent (dup upload safe)
        storage.put(s.minio_bucket_raw, key, data)
    sess = get_session()
    try:
        doc = KycDocument(case_id=case_id, doc_type=doc_type, sha256=digest,
                          minio_key=key, mime=mime, pages=1)
        sess.add(doc)
        sess.commit()
        sess.refresh(doc)
        return doc
    finally:
        sess.close()
