"""Shared pipeline types."""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Optional


@dataclass
class OcrToken:
    text: str
    conf: float
    bbox: list[list[float]] = field(default_factory=list)


@dataclass
class ParsedDocument:
    """docling output abstraction: layout text, tables, page PNG bytes."""
    page_images: list[bytes] = field(default_factory=list)
    text: str = ""
    tables: list[list[list[str]]] = field(default_factory=list)
    meta: dict[str, Any] = field(default_factory=dict)


@dataclass
class OcrResult:
    tokens: list[OcrToken] = field(default_factory=list)
    conf_avg: float = 0.0
    engine: str = "paddleocr"   # or "stub" when PaddleOCR unavailable
    sim: bool = False


@dataclass
class StageContext:
    case_id: str
    document_id: Optional[str] = None
    doc_type: str = "unknown"
    raw_bytes: bytes = b""
    mime: str = "application/octet-stream"
    parsed: Optional[ParsedDocument] = None
    ocr: Optional[OcrResult] = None
    fields: dict[str, Any] = field(default_factory=dict)
    extras: dict[str, Any] = field(default_factory=dict)
