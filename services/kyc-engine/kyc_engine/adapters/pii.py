"""PII protection at rest (NDPA s.24/s.39 TOMs; NIMC no-raw-storage policy).

Extracted subject PII must never persist unmasked in ``KycExtraction.fields``.
``protect_fields`` splits extractor output into:
- sanitized fields: identifiers HMAC-pseudonymised (``*_hmac``) + masked
  display form (e.g. ``123****890``); names masked; DOB masked;
- vault: the raw values, stored ONLY in the restricted ``pii_vault`` column
  (tokenisation-vault pattern: reversible lookup for legitimate processing
  such as re-screening; never serialised by the API or written to logs).

The HMAC key comes from ``PII_HMAC_KEY``. FAIL-CLOSED in prod: when
``AUTH_MODE != dev`` and no key is configured, protection raises rather than
persisting weakly-pseudonymised PII. Dev/test uses a fixed inert key.
"""
from __future__ import annotations

import hashlib
import hmac
from typing import Any

from ..config import get_settings
from .nin_verify import mask_credential

# identifier fields: masked + HMAC-pseudonymised, raw -> vault
_IDENTIFIER_KEYS = ("nin", "vnin", "passport_no", "license_no")
# subject name fields: masked, raw -> vault
_NAME_KEYS = ("surname", "first_name", "given_names", "name")
# date of birth: fully masked, raw -> vault
_DOB_KEYS = ("dob",)

_DEV_KEY = b"kyc-engine-dev-pii-key"  # inert, dev/test only


class PiiKeyMissing(RuntimeError):
    """Prod refuses to persist PII without an HMAC key (fail-closed)."""


def _hmac_key() -> bytes:
    s = get_settings()
    if s.pii_hmac_key:
        return s.pii_hmac_key.encode()
    if s.auth_mode != "dev":
        raise PiiKeyMissing("PII_HMAC_KEY required when AUTH_MODE != dev")
    return _DEV_KEY


def hmac_pseudonym(value: str) -> str:
    return hmac.new(_hmac_key(), value.encode(), hashlib.sha256).hexdigest()


def _mask_name(value: str) -> str:
    v = value or ""
    if len(v) <= 2:
        return "*" * len(v)
    return v[0] + "*" * (len(v) - 2) + v[-1]


def protect_fields(fields: dict[str, Any]) -> tuple[dict[str, Any], dict[str, Any]]:
    """Split extractor fields into (sanitized, vault). Non-PII keys pass
    through unchanged (company name, RC number, reg_date, directors — public
    registry data)."""
    sanitized: dict[str, Any] = {}
    vault: dict[str, Any] = {}
    for k, v in fields.items():
        if v is None or k.startswith("_"):
            sanitized[k] = v
        elif k in _IDENTIFIER_KEYS:
            sanitized[k] = mask_credential(str(v))
            sanitized[f"{k}_hmac"] = hmac_pseudonym(str(v))
            vault[k] = v
        elif k in _NAME_KEYS:
            sanitized[k] = _mask_name(str(v))
            vault[k] = v
        elif k in _DOB_KEYS:
            sanitized[k] = "****-**-**"
            vault[k] = v
        else:
            sanitized[k] = v
    if vault:
        sanitized["_pii_protected"] = True
    return sanitized, vault
