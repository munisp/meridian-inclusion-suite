"""Unit: vNIN credential validation + NIMC adapter vNIN/legacy paths (K1)."""
from __future__ import annotations

import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from kyc_engine.adapters.nin_verify import (NinVerifySim, mask_credential,
                                            vnin_expired, vnin_format_valid)

NOW = datetime(2026, 1, 10, 12, 0, 0, tzinfo=timezone.utc)


def test_vnin_format():
    assert vnin_format_valid("AB012345678910YZ")
    assert vnin_format_valid("ab012345678910yz")
    assert not vnin_format_valid("AB01234567891YZ")     # 15 chars
    assert not vnin_format_valid("AB0123456789101YZ")   # 17 chars
    assert not vnin_format_valid("12012345678910YZ")    # digits in letter frame
    assert not vnin_format_valid("")


def test_vnin_ttl_72h():
    issued = NOW - timedelta(hours=71, minutes=59)
    assert not vnin_expired(issued, now=NOW)
    assert vnin_expired(NOW - timedelta(hours=72, minutes=1), now=NOW)


def test_sim_vnin_primary_path_verified():
    sim = NinVerifySim()
    out = sim.verify(vnin="AB012345678910YZ",
                     vnin_issued_at=datetime.now(timezone.utc) - timedelta(hours=1))
    assert out["credential_type"] == "vnin"
    assert out["sim"] is True
    assert out["verified"] in (True, False)  # deterministic hash outcome
    assert out["reason"] != "token_expired"
    assert out["credential_masked"] == "AB0****0YZ"
    assert "AB012345678910YZ" not in str(out)  # raw token never echoed


def test_sim_vnin_expired_by_ttl():
    sim = NinVerifySim()
    out = sim.verify(vnin="CD012345678910YZ",
                     vnin_issued_at=datetime.now(timezone.utc) - timedelta(hours=80))
    assert out["verified"] is False
    assert out["reason"] == "token_expired"


def test_sim_vnin_expired_persona_prefix():
    sim = NinVerifySim()
    out = sim.verify(vnin="EX012345678910YZ")
    assert out["verified"] is False
    assert out["reason"] == "token_expired"


def test_sim_vnin_not_found_persona():
    sim = NinVerifySim()
    out = sim.verify(vnin="NF012345678910YZ")
    assert out["verified"] is False
    assert out["reason"] == "not_found"


def test_sim_vnin_invalid_format():
    sim = NinVerifySim()
    out = sim.verify(vnin="not-a-token")
    assert out["verified"] is False
    assert out["reason"] == "invalid_format"


def test_sim_legacy_nin_path_retained():
    sim = NinVerifySim()
    out = sim.verify(nin="12345678901")
    assert out["credential_type"] == "nin_legacy"
    assert out["credential_masked"] == "123****901"
    assert "12345678901" not in str(out)
    bad = sim.verify(nin="123")
    assert bad["verified"] is False
    assert bad["reason"] == "invalid_format"


def test_mask_credential():
    assert mask_credential("12345678901") == "123****901"
    assert mask_credential("12345") == "*****"
    assert mask_credential("") == ""
