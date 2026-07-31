"""Stage 4: per-doctype field extractors (regex + layout anchors).

REAL, dependency-free. Doctypes: nin_slip, cac_cert, passport,
drivers_license. Unknown doctype -> generic token dump + classify hint;
callers must never auto-approve unknown doctypes (SPEC A §6).
"""
from __future__ import annotations

import re
from typing import Any, Optional

from .types import OcrToken

EXTRACTOR_VERSION = "1.0.0"

# ---------------------------------------------------------------- validators

def nin_format_valid(nin: str) -> bool:
    """NIN: 11 digits, non-zero first digit, not a repeated-digit sequence."""
    if not re.fullmatch(r"\d{11}", nin or ""):
        return False
    if nin[0] == "0" or len(set(nin)) == 1:
        return False
    return True


# CAC registration-number formats (no checksum is published by CAC — RC/BN/IT
# numbers are sequential registry identifiers, so format-only validation is
# the defensible stance; truth is delegated to the registry cross-check).
#   RC######  companies (registered companies)
#   BN######  business names
#   IT######  incorporated trustees
CAC_NUMBER_RE = re.compile(r"^(RC|BN|IT)(\d{5,8})$", re.IGNORECASE)
# Older certificates carry free-format numbers; accept them (never false-reject)
# and flag as legacy so downstream can note the weaker format signal.
_LEGACY_NUMBER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9 ./-]{2,31}$")


def cac_number_kind(rc: str) -> Optional[str]:
    """'RC' | 'BN' | 'IT' | 'legacy' | None (empty/unusable)."""
    if not rc or not rc.strip():
        return None
    m = CAC_NUMBER_RE.fullmatch(rc.strip())
    if m:
        return m.group(1).upper()
    if _LEGACY_NUMBER_RE.fullmatch(rc.strip()):
        return "legacy"
    return None


def rc_format_valid(rc: str) -> bool:
    """CAC number format check: RC/BN/IT + digits, or free-format legacy.
    Never invents a checksum — any non-empty plausible registry number passes;
    the registry lookup is the source of truth."""
    return cac_number_kind(rc) is not None


def mrz_check_digit(data: str) -> str:
    """ICAO 9303 MRZ check digit (7-3-1 weights)."""
    vals = {str(i): i for i in range(10)}
    vals.update({c: i + 10 for i, c in enumerate("ABCDEFGHIJKLMNOPQRSTUVWXYZ")})
    vals["<"] = 0
    total = sum(vals[c] * [7, 3, 1][i % 3] for i, c in enumerate(data))
    return str(total % 10)


# ---------------------------------------------------------------- matchers

_DATE_RE = re.compile(r"\b(\d{4}[-/]\d{2}[-/]\d{2}|\d{2}[-/]\d{2}[-/]\d{4})\b")


def _text(tokens: list[OcrToken]) -> str:
    return " ".join(t.text for t in tokens)


def _match_date(txt: str) -> Optional[str]:
    m = _DATE_RE.search(txt)
    return m.group(1) if m else None


def _match_label(tokens: list[OcrToken], label: str) -> Optional[str]:
    """Layout anchor: value is the token immediately right of / after the label."""
    for i, t in enumerate(tokens):
        low = t.text.lower()
        if label.lower() in low:
            idx = low.index(label.lower()) + len(label)
            rest = t.text[idx:].strip(" :")
            if rest:
                return rest
            if i + 1 < len(tokens):
                return tokens[i + 1].text
    return None


def _conf_floor(tokens: list[OcrToken]) -> float:
    return min((t.conf for t in tokens), default=0.0)


# ---------------------------------------------------------------- extractors

def extract_nin(tokens: list[OcrToken]) -> dict[str, Any]:
    txt = _text(tokens)
    m = re.search(r"\b(\d{11})\b", txt)
    nin = m.group(1) if m else None
    fields: dict[str, Any] = {
        "nin": nin,
        "nin_format_ok": nin_format_valid(nin) if nin else False,
        "surname": _match_label(tokens, "surname"),
        "first_name": _match_label(tokens, "first name"),
        "dob": _match_label(tokens, "date of birth") or _match_date(txt),
    }
    fields["_conf"] = _conf_floor(tokens)
    return fields


def extract_cac(tokens: list[OcrToken]) -> dict[str, Any]:
    txt = _text(tokens)
    m = re.search(r"\b((?:RC|BN|IT)\s?\d{5,8})\b", txt, re.IGNORECASE)
    rc = m.group(1).replace(" ", "").upper() if m else None
    directors: list[str] = []
    for t in tokens:
        if t.text.lower().startswith("director:"):
            directors.append(t.text.split(":", 1)[1].strip())
    fields: dict[str, Any] = {
        "rc_number": rc,
        "rc_format_ok": rc_format_valid(rc) if rc else False,
        "rc_kind": cac_number_kind(rc),
        "company_name": _match_label(tokens, "company name") or _match_label(tokens, "name of company"),
        "reg_date": _match_label(tokens, "registered") or _match_date(txt),
        "directors": directors,
    }
    fields["_conf"] = _conf_floor(tokens)
    return fields


def extract_passport(tokens: list[OcrToken]) -> dict[str, Any]:
    txt = _text(tokens)
    m = re.search(r"\b([A-Z]\d{8})\b", txt)
    fields: dict[str, Any] = {
        "passport_no": m.group(1) if m else None,
        "surname": _match_label(tokens, "surname"),
        "given_names": _match_label(tokens, "given names"),
        "dob": _match_label(tokens, "date of birth") or _match_date(txt),
        "expiry": _match_label(tokens, "date of expiry"),
        "nationality": _match_label(tokens, "nationality"),
    }
    fields["_conf"] = _conf_floor(tokens)
    return fields


def extract_drivers_license(tokens: list[OcrToken]) -> dict[str, Any]:
    txt = _text(tokens)
    m = re.search(r"\b([A-Z]{3}\d{9})\b", txt)
    fields: dict[str, Any] = {
        "license_no": m.group(1) if m else None,
        "name": _match_label(tokens, "name"),
        "dob": _match_date(txt),
        "expiry": _match_label(tokens, "exp"),
    }
    fields["_conf"] = _conf_floor(tokens)
    return fields


def extract_generic(tokens: list[OcrToken]) -> dict[str, Any]:
    """Unknown doctype: raw text dump only; classify hint for VLM. Never
    auto-approvable downstream (SPEC A §6)."""
    return {"raw_text": _text(tokens), "_conf": _conf_floor(tokens),
            "_unknown_doctype": True}


EXTRACTORS = {
    "nin_slip": extract_nin,
    "cac_cert": extract_cac,
    "passport": extract_passport,
    "drivers_license": extract_drivers_license,
}


def extract_fields(doc_type: str, tokens: list[OcrToken]) -> dict[str, Any]:
    fn = EXTRACTORS.get(doc_type, extract_generic)
    out = fn(tokens)
    out["_extractor_version"] = EXTRACTOR_VERSION
    return out
