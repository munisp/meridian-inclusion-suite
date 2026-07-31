"""Unit: field extractors + validators (SPEC A §7 golden-field accuracy)."""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from kyc_engine.pipeline.stage_fields import (EXTRACTORS, cac_number_kind,
                                              extract_cac, extract_fields,
                                              extract_nin, mrz_check_digit,
                                              nin_format_valid, rc_format_valid)
from kyc_engine.pipeline.stage_ocr import run_ocr
from tests.make_fixtures import cac_cert, nin_slip, valid_rc


def _tokens(png):
    return run_ocr(png).tokens


def test_extract_nin_golden():
    f = extract_nin(_tokens(nin_slip("12345678901")))
    assert f["nin"] == "12345678901"
    assert f["nin_format_ok"] is True
    assert f["surname"] == "ADEYEMI"
    assert f["first_name"] == "CHIAMAKA"
    assert f["dob"] == "1990-05-14"


def test_nin_validator():
    assert nin_format_valid("12345678901")
    assert not nin_format_valid("02345678901")   # leading zero
    assert not nin_format_valid("11111111111")   # repeated digit
    assert not nin_format_valid("1234567890")    # 10 digits
    assert not nin_format_valid("")


def test_cac_number_formats_rc_bn_it():
    # CAC assigns RC (companies), BN (business names), IT (incorporated
    # trustees); all must pass KYB format validation.
    for num, kind in [("RC123456", "RC"), ("BN1234567", "BN"), ("IT12345", "IT"),
                      ("rc12345678", "RC")]:
        assert rc_format_valid(num), num
        assert cac_number_kind(num) == kind


def test_cac_legacy_free_format_accepted():
    # Older certificates carry free-format numbers: never false-reject.
    assert rc_format_valid("RC/0123/ABUJA")
    assert cac_number_kind("RC/0123/ABUJA") == "legacy"
    assert rc_format_valid("XX1234567")
    assert cac_number_kind("XX1234567") == "legacy"


def test_rc_format_rejects_junk():
    assert not rc_format_valid("")
    assert not rc_format_valid(None)
    assert not rc_format_valid("!!!")
    assert cac_number_kind("") is None


def test_extract_cac_golden():
    rc = valid_rc("765432")
    f = extract_cac(_tokens(cac_cert(rc)))
    assert f["rc_number"] == rc
    assert f["rc_format_ok"] is True
    assert f["rc_kind"] == "RC"
    assert f["company_name"] == "MERIDIAN TEST VENTURES LTD"
    assert f["reg_date"] == "2019-03-22"
    assert f["directors"] == ["ADAEZE OKAFOR", "IBRAHIM MUSA"]


def test_mrz_check_digit_icao():
    # ICAO 9303 worked example fragment
    assert mrz_check_digit("520727") in "0123456789"
    assert mrz_check_digit("ABCDEFG") == str(sum([10*7, 11*3, 12*1, 13*7, 14*3, 15*1, 16*7]) % 10)


def test_unknown_doctype_generic():
    f = extract_fields("utility_bill", _tokens(nin_slip()))
    assert f["_unknown_doctype"] is True
    assert "raw_text" in f


def test_all_known_doctypes_have_extractors():
    for dt in ("nin_slip", "cac_cert", "passport", "drivers_license"):
        assert dt in EXTRACTORS
