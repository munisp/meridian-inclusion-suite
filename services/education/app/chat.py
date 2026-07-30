"""T14 chat endpoint: grounded on the local FAQ corpus.

Retrieval-based answering only — no external LLM key required. The best FAQ
match is composed into a templated answer; calculator intents are routed to
the deterministic calculators so numeric answers are exact.
"""
from __future__ import annotations

import re

from . import faq

# Minimum retrieval score below which we decline to guess.
_MIN_SCORE = 0.05

_NUM_RE = re.compile(r"(?:n|ngn|₦)?\s*([0-9][0-9,]*(?:\.\d+)?)\s*(million|m|billion|bn|k)?\b", re.IGNORECASE)


def _parse_amounts(text: str) -> list[int]:
    """Extract naira amounts (with optional m/bn/k suffixes) as kobo."""
    amounts = []
    for m in _NUM_RE.finditer(text):
        val = float(m.group(1).replace(",", ""))
        suffix = (m.group(2) or "").lower()
        if suffix in ("million", "m"):
            val *= 1_000_000
        elif suffix in ("billion", "bn"):
            val *= 1_000_000_000
        elif suffix == "k":
            val *= 1_000
        if val >= 100:  # ignore tiny numbers (rates, counts)
            amounts.append(int(val * 100))  # kobo
    return amounts


def _calculator_intent(message: str) -> dict | None:
    """Route numeric questions to the calculators for exact answers."""
    from . import calculators

    text = message.lower()
    amounts = _parse_amounts(message)
    if not amounts:
        return None
    if any(w in text for w in ("pit", "personal income tax", "paye", "income tax")):
        calc = calculators.calc_pit(amounts[0])
        return {
            "answer": (
                f"On a gross annual income of ₦{amounts[0]/100:,.2f}, estimated personal income tax under the "
                f"NTA 2025 bands is ₦{calc['amount_naira']:,.2f} (effective rate {calc['effective_rate_pct']}%). "
                f"Band-by-band trace: " + " | ".join(calc["trace"])
            ),
            "calculator": "pit",
            "calculation": calc,
        }
    if "vat" in text:
        calc = calculators.calc_vat(amounts[0])
        return {
            "answer": (
                f"VAT at 7.5% on ₦{amounts[0]/100:,.2f} is ₦{calc['amount_naira']:,.2f} "
                f"(gross ₦{(amounts[0] + calc['amount_kobo'])/100:,.2f}). {calc['provision_citation']}."
            ),
            "calculator": "vat",
            "calculation": calc,
        }
    if any(w in text for w in ("cit", "companies income tax", "company tax", "corporate tax")):
        calc = calculators.calc_cit(amounts[0], amounts[0])
        if calc["small_company"]:
            verdict = "small company — 0% CIT."
        else:
            verdict = f"standard 30% — ₦{calc['amount_naira']:,.2f} on assessable profit."
        return {
            "answer": (
                f"CIT check on ₦{amounts[0]/100:,.2f}: {verdict} "
                + " | ".join(calc["trace"])
            ),
            "calculator": "cit",
            "calculation": calc,
        }
    if "rent" in text and "relief" in text:
        calc = calculators.calc_rent_relief(amounts[0])
        return {
            "answer": (
                f"Rent relief on annual rent of ₦{amounts[0]/100:,.2f} is ₦{calc['amount_naira']:,.2f} "
                f"(20% of rent, capped at ₦500,000). {calc['provision_citation']}."
            ),
            "calculator": "rent_relief",
            "calculation": calc,
        }
    return None


def answer(message: str, history: list[dict] | None = None) -> dict:
    """Compose a grounded answer for a user message."""
    message = (message or "").strip()
    if not message:
        return {"answer": "Please type a question about Nigerian tax.", "sources": [], "grounded": False}

    # 1) exact numeric intents -> calculators
    intent = _calculator_intent(message)
    if intent:
        intent["sources"] = ["rp-education-ng calculators"]
        intent["grounded"] = True
        intent["disclaimer"] = (
            "Estimates are educational, not an assessment. Consult the NRS or a licensed practitioner."
        )
        return intent

    # 2) FAQ retrieval
    hits = faq.search(message, limit=3)
    if hits and hits[0]["score"] >= _MIN_SCORE:
        best = hits[0]
        related = [h for h in hits[1:] if h["score"] >= _MIN_SCORE]
        text = best["answer"]
        if related:
            text += "\n\nRelated: " + "; ".join(h["question"] for h in related)
        return {
            "answer": text,
            "sources": [h["id"] for h in hits if h["score"] >= _MIN_SCORE],
            "matched_question": best["question"],
            "grounded": True,
            "score": best["score"],
        }

    # 3) honest fallback — never hallucinate
    return {
        "answer": (
            "I don't have a grounded answer for that in the Nigerian tax FAQ corpus. "
            "Try asking about PIT bands, the ₦800,000 tax-free threshold, rent relief, "
            "CIT and small companies, VAT at 7.5%, WHT rates, presumptive levy, filing "
            "deadlines or certificate verification — or contact the NRS directly."
        ),
        "sources": [],
        "grounded": False,
    }
