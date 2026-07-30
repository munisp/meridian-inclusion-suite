"""Tests for T14 calculators (NTA 2025 tables)."""
from app import calculators


def test_pit_below_threshold_is_zero():
    r = calculators.calc_pit(80_000_000)  # N800,000
    assert r["amount_kobo"] == 0
    assert "First N800,000 at 0%" in " ".join(r["trace"])
    assert "NTA 2025" in r["provision_citation"]
    assert r["disclaimer"]
    assert r["trace"]


def test_pit_band_progression():
    # N3,000,000: 0% on first 800k, 15% on next 2.2m -> N330,000
    r = calculators.calc_pit(300_000_000)
    assert r["amount_kobo"] == 33_000_000
    # N5,000,000: 330k + 18% of 2m = 330k + 360k = N690,000
    r = calculators.calc_pit(500_000_000)
    assert r["amount_kobo"] == 69_000_000


def test_pit_top_rate_above_50m():
    # N60,000,000: 330k + 1.62m + 2.73m + 5.75m + 25% of 10m = 12.93m
    r = calculators.calc_pit(6_000_000_000)
    assert r["amount_kobo"] == 1_293_000_000
    assert "25%" in " ".join(r["trace"])


def test_pit_rent_relief_capped():
    # rent N4,000,000 -> 20% = 800k but capped at 500k
    r = calculators.calc_pit(500_000_000, annual_rent_paid_kobo=400_000_000)
    assert r["rent_relief_kobo"] == 50_000_000
    # chargeable 4.5m -> 330k + 18% of 1.5m = 330k + 270k = N600,000
    assert r["amount_kobo"] == 60_000_000


def test_rent_relief_standalone():
    r = calculators.calc_rent_relief(100_000_000)  # rent N1m -> 200k
    assert r["amount_kobo"] == 20_000_000
    assert not r["capped"]
    r = calculators.calc_rent_relief(500_000_000)  # rent N5m -> capped 500k
    assert r["amount_kobo"] == 50_000_000
    assert r["capped"]


def test_cit_small_company_zero():
    r = calculators.calc_cit(annual_turnover_kobo=9_000_000_000, assessable_profit_kobo=1_000_000_000,
                             total_fixed_assets_kobo=20_000_000_000)
    assert r["small_company"] is True
    assert r["amount_kobo"] == 0


def test_cit_standard_30pct():
    r = calculators.calc_cit(annual_turnover_kobo=20_000_000_000, assessable_profit_kobo=5_000_000_000,
                             total_fixed_assets_kobo=30_000_000_000)
    assert r["small_company"] is False
    assert r["amount_kobo"] == 1_500_000_000  # 30% of N50m = N15m


def test_cit_fixed_assets_boundary():
    # turnover ok but assets above N250m -> not small
    r = calculators.calc_cit(annual_turnover_kobo=9_000_000_000, assessable_profit_kobo=1_000_000_000,
                             total_fixed_assets_kobo=30_000_000_000)
    assert r["small_company"] is False


def test_vat_exclusive_and_inclusive():
    r = calculators.calc_vat(100_000)  # N1,000 net -> N75
    assert r["amount_kobo"] == 7_500
    r = calculators.calc_vat(107_500, inclusive=True)  # N1,075 gross -> N75
    assert r["amount_kobo"] == 7_500
    assert "7.5" in " ".join(r["trace"])


def test_wht_rates_and_carveout():
    r = calculators.calc_wht("dividend", 1_000_000_000)
    assert r["amount_kobo"] == 100_000_000  # 10%
    r = calculators.calc_wht("services", 1_000_000_000)
    assert r["amount_kobo"] == 50_000_000  # 5%
    # small transaction carve-out <= N2m for services
    r = calculators.calc_wht("services", 200_000_000)
    assert r["amount_kobo"] == 0
    # no TIN -> double rate
    r = calculators.calc_wht("dividend", 1_000_000_000, beneficiary_has_tin=False)
    assert r["amount_kobo"] == 200_000_000


def test_every_calculator_has_citation_disclaimer_trace():
    for r in (
        calculators.calc_pit(1_000_000_000),
        calculators.calc_cit(1_000_000_000, 100_000_000),
        calculators.calc_vat(100_000),
        calculators.calc_rent_relief(100_000_000),
        calculators.calc_wht("rent", 1_000_000_000),
    ):
        assert r["provision_citation"]
        assert "not a tax assessment" in r["disclaimer"]
        assert len(r["trace"]) >= 1
        assert r["rule_pack"].startswith("rp-education-ng@")
