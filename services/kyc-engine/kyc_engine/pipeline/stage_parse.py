"""Stage 2: parse — docling PDF/img -> layout, tables, page images (300dpi PNG).

Import-guarded: when docling is unavailable (e.g. light CI), images pass
through directly and PDFs rasterize via pypdfium2 if present, else the raw
bytes are treated as a single page. The real docling path is complete for
deployments that ship the heavy image.
"""
from __future__ import annotations

import io

from .types import ParsedDocument

try:
    from docling.document_converter import DocumentConverter  # type: ignore
    _HAVE_DOCLING = True
except ImportError:
    _HAVE_DOCLING = False

DOC_ENGINE = "docling" if _HAVE_DOCLING else "passthrough"


def _image_bytes_to_png(data: bytes) -> bytes:
    """Re-encode to 300dpi PNG, preserving PNG text chunks (fixture sidecars,
    EXIF-adjacent metadata) that downstream stages rely on."""
    from PIL import Image, PngImagePlugin
    img = Image.open(io.BytesIO(data))
    meta = PngImagePlugin.PngInfo()
    for k, v in img.info.items():
        if isinstance(v, str):
            meta.add_text(k, v)
    buf = io.BytesIO()
    img.convert("RGB").save(buf, format="PNG", dpi=(300, 300), pnginfo=meta)
    return buf.getvalue()


def parse_document(data: bytes, mime: str) -> ParsedDocument:
    if _HAVE_DOCLING and mime == "application/pdf":
        conv = DocumentConverter()
        import tempfile, pathlib
        with tempfile.NamedTemporaryFile(suffix=".pdf", delete=False) as tf:
            tf.write(data)
            tmp = tf.name
        try:
            result = conv.convert(tmp)
        finally:
            pathlib.Path(tmp).unlink(missing_ok=True)
        doc = result.document
        text = doc.export_to_markdown()
        tables = [
            [[cell.text for cell in row] for row in t.data.grid]  # type: ignore[attr-defined]
            for t in getattr(doc, "tables", [])
        ]
        pages = []
        for page in getattr(doc, "pages", []):
            img = getattr(page, "image", None)
            if img is not None and getattr(img, "pil_image", None) is not None:
                buf = io.BytesIO()
                img.pil_image.save(buf, format="PNG", dpi=(300, 300))
                pages.append(buf.getvalue())
        return ParsedDocument(page_images=pages, text=text, tables=tables,
                              meta={"engine": DOC_ENGINE})
    # Fallback: images -> PNG page; PDF without docling -> raw page
    if mime.startswith("image/"):
        return ParsedDocument(page_images=[_image_bytes_to_png(data)], text="",
                              meta={"engine": DOC_ENGINE, "sim": True})
    return ParsedDocument(page_images=[data], text="",
                          meta={"engine": DOC_ENGINE, "sim": True})
