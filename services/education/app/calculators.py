"""T14 calculators: PIT, CIT, VAT, rent relief, WHT quick calc.

Every calculator returns the amount (integer kobo), the provision citation,
a disclaimer and a full calculation trace — audit-defensibility by design.
Money is integer kobo only (SPEC §1.3); inputs are kobo.
"""
from __future__ import annotations

from typing import Any

from .packs import get_rule, load_pack, pack_ref

DISCLAIMER = (
    "This estimate is for taxpayer education only and is not a tax assessment. "
    "Figures follow the cited provisions of the Nigeria Tax Act 2025 / WHT "
    "Regulations 2024 as published in the rp-education-ng pack "
    "(subject_to_regazette: figures may change on gazetting). Consult the NRS "
    "or a licensed tax practitioner for advice on your specific circumstances."
)


def _result(amount_kobo: int, citation: str, trace: list[str], extra: dict[str, Any] | None = None) -> dict[str, Any]:
    out: dict[str, Any] = {
        "amount_kobo": int(amount_kobo),
        "amount_naira": round(amount_kobo / 100, 2),
        "provision_citation": citation,
        "disclaimer": DISCLAIMER,
        "trace": trace,
        "rule_pack": pack_ref(load_pack()),
    }
    if extra:
        out.update(extra)
    return out


def calc_pit(annual_gross_income_kobo: int, annual_rent_paid_kobo: int = 0) -> dict[str, Any]:
    """Personal income tax under NTA 2025 bands, with rent relief applied
    as a deduction from chargeable income."""
    pack = load_pack()
    bands_rule = get_rule(pack, "pit.bands.2026")
    rent_rule = get_rule(pack, "pit.rent_relief.2026")
    trace: list[str] = [f"Gross annual income: N{annual_gross_income_kobo/100:,.2f}"]

    relief = 0
    if annual_rent_paid_kobo > 0:
        relief = min(annual_rent_paid_kobo * rent_rule["rate_bps"] // 10000, rent_rule["cap_kobo"])
        trace.append(
            f"Rent relief: 20% of rent paid N{annual_rent_paid_kobo/100:,.2f} = "
            f"N{annual_rent_paid_kobo * rent_rule['rate_bps'] // 10000 / 100:,.2f}, "
            f"capped at N{rent_rule['cap_kobo']/100:,.2f} -> relief N{relief/100:,.2f}"
        )
    chargeable = max(0, annual_gross_income_kobo - relief)
    trace.append(f"Chargeable income: N{chargeable/100:,.2f}")

    tax = 0
    lower = 0
    for band in bands_rule["bands"]:
        upper = band["up_to_kobo"]
        rate = band["rate_bps"]
        if chargeable <= lower:
            trace.append(f"Band ({band['narrate']}): no income in this band")
            continue
        band_ceiling = upper if upper is not None else chargeable
        taxable_here = min(chargeable, band_ceiling) - lower
        tax_here = taxable_here * rate // 10000
        tax += tax_here
        trace.append(
            f"Band ({band['narrate']}): N{taxable_here/100:,.2f} x {rate/100}% = N{tax_here/100:,.2f}"
        )
        if upper is None or chargeable <= upper:
            break
        lower = upper
    effective = (tax / chargeable * 100) if chargeable else 0.0
    trace.append(f"Total PIT: N{tax/100:,.2f} (effective rate {effective:.2f}%)")
    citation = bands_rule["citation"] + ("; " + rent_rule["citation"] if relief else "")
    return _result(tax, citation, trace, {
        "chargeable_income_kobo": chargeable,
        "rent_relief_kobo": relief,
        "effective_rate_pct": round(effective, 4),
    })


def calc_rent_relief(annual_rent_paid_kobo: int) -> dict[str, Any]:
    rule = get_rule(load_pack(), "pit.rent_relief.2026")
    uncapped = annual_rent_paid_kobo * rule["rate_bps"] // 10000
    relief = min(uncapped, rule["cap_kobo"])
    trace = [
        f"Annual rent paid: N{annual_rent_paid_kobo/100:,.2f}",
        f"20% of rent: N{uncapped/100:,.2f}",
        f"Cap: N{rule['cap_kobo']/100:,.2f}",
        f"Rent relief (deductible from income): N{relief/100:,.2f}",
    ]
    return _result(relief, rule["citation"], trace, {"capped": uncapped > rule["cap_kobo"]})


def calc_cit(annual_turnover_kobo: int, assessable_profit_kobo: int, total_fixed_assets_kobo: int = 0) -> dict[str, Any]:
    std = get_rule(load_pack(), "cit.standard.2026")
    small = get_rule(load_pack(), "cit.small_company.2026")
    trace = [
        f"Annual turnover: N{annual_turnover_kobo/100:,.2f}",
        f"Total fixed assets: N{total_fixed_assets_kobo/100:,.2f}",
    ]
    is_small = (
        annual_turnover_kobo <= small["turnover_ceiling_kobo"]
        and total_fixed_assets_kobo <= small["fixed_assets_ceiling_kobo"]
    )
    if is_small:
        trace.append(
            f"Small-company test: turnover <= N{small['turnover_ceiling_kobo']/100:,.0f} "
            f"AND fixed assets <= N{small['fixed_assets_ceiling_kobo']/100:,.0f} -> PASSES"
        )
        trace.append("Small company CIT rate: 0%")
        return _result(0, small["citation"], trace, {"small_company": True, "rate_bps": 0})
    trace.append("Small-company test: FAILS -> standard rate applies")
    tax = max(0, assessable_profit_kobo) * std["rate_bps"] // 10000
    trace.append(
        f"Assessable profit N{assessable_profit_kobo/100:,.2f} x {std['rate_bps']/100}% = N{tax/100:,.2f}"
    )
    return _result(tax, std["citation"], trace, {"small_company": False, "rate_bps": std["rate_bps"]})


def calc_vat(net_amount_kobo: int, inclusive: bool = False) -> dict[str, Any]:
    rule = get_rule(load_pack(), "vat.standard.2026")
    rate = rule["rate_bps"]
    if inclusive:
        vat = net_amount_kobo * rate // (10000 + rate)
        net = net_amount_kobo - vat
        trace = [
            f"Gross (VAT-inclusive) amount: N{net_amount_kobo/100:,.2f}",
            f"VAT fraction {rate/100}/(100+{rate/100}): N{vat/100:,.2f}",
            f"Net amount: N{net/100:,.2f}",
        ]
    else:
        vat = net_amount_kobo * rate // 10000
        trace = [
            f"Net amount: N{net_amount_kobo/100:,.2f}",
            f"VAT at {rate/100}%: N{vat/100:,.2f}",
            f"Gross: N{(net_amount_kobo + vat)/100:,.2f}",
        ]
    return _result(vat, rule["citation"], trace, {"rate_bps": rate, "inclusive": inclusive})


def calc_wht(payment_type: str, amount_kobo: int, beneficiary_has_tin: bool = True) -> dict[str, Any]:
    rule = get_rule(load_pack(), "wht.quick.2024")
    rates = rule["rates"]
    pt = payment_type.lower().strip()
    if pt not in rates:
        raise ValueError(f"unknown payment_type {payment_type!r}; have {sorted(rates)}")
    trace = [f"Payment type: {pt}; amount N{amount_kobo/100:,.2f}"]
    if amount_kobo <= rule["small_transaction_carveout_kobo"] and pt in ("goods", "services"):
        trace.append(
            f"Transaction <= N{rule['small_transaction_carveout_kobo']/100:,.0f} small-transaction "
            "carve-out for goods/services (WHT Regs 2024) -> 0%"
        )
        return _result(0, rule["citation"], trace, {"rate_bps": 0, "payment_type": pt})
    entry = rates[pt]
    rate = entry["rate_bps"]
    if not beneficiary_has_tin:
        rate *= 2
        trace.append("Beneficiary has no TIN: rate doubled (WHT Regs 2024 reg. 9)")
    wht = amount_kobo * rate // 10000
    trace.append(f"{entry['narrate']}: N{amount_kobo/100:,.2f} x {rate/100}% = N{wht/100:,.2f}")
    return _result(wht, rule["citation"], trace, {"rate_bps": rate, "payment_type": pt})
