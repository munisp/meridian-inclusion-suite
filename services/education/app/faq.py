"""T14 FAQ corpus: >=30 real Nigerian tax FAQs with a local search index.

The corpus reflects the Nigeria Tax Act 2025 (effective 1 Jan 2026), the NTAA
2025, the WHT Regulations 2024 and long-standing FIRS/NRS practice. Search is
a local TF-IDF-style index — no external service required.
"""
from __future__ import annotations

import math
import re
from functools import lru_cache

# Each FAQ: id, question, answer, keywords (domain terms to boost matching).
FAQ_CORPUS: list[dict] = [
    {"id": "faq-001", "question": "What is a TIN and how do I get one?",
     "answer": "A Taxpayer Identification Number (TIN) is your unique identifier with the Nigeria Revenue Service (NRS, formerly FIRS) and state internal revenue services. Individuals are registered free of charge using their NIN (your NIN effectively becomes your TIN under the NIN=TIN fusion), while companies are registered with their CAC registration (RC) number. You can register through an NRS office, an accredited agent, or an approved digital channel such as this platform.",
     "keywords": ["tin", "registration", "nin", "identifier", "register"]},
    {"id": "faq-002", "question": "Is my NIN the same as my TIN now?",
     "answer": "Under the identity-fusion approach adopted by the NRS, an individual's National Identification Number (NIN) is linked to and used as the basis for their TIN. When you are onboarded, your NIN is verified with NIMC and a TIN record is provisioned against it, so you do not need a separate manual TIN application.",
     "keywords": ["nin", "tin", "fusion", "identity", "nimc"]},
    {"id": "faq-003", "question": "What is the tax-free threshold for personal income tax from 2026?",
     "answer": "Under the Nigeria Tax Act 2025, the first N800,000 of annual income is taxed at 0%. If your total annual income is N800,000 or less, you pay no personal income tax.",
     "keywords": ["pit", "threshold", "800000", "tax-free", "personal income"]},
    {"id": "faq-004", "question": "What are the personal income tax rates under the Nigeria Tax Act 2025?",
     "answer": "The NTA 2025 bands are: first N800,000 at 0%; next N2,200,000 at 15%; next N9,000,000 at 18%; next N13,000,000 at 21%; next N25,000,000 at 23%; and income above N50,000,000 at 25%. Use the PIT calculator on this page for a full band-by-band trace.",
     "keywords": ["pit", "rates", "bands", "percent", "personal income tax"]},
    {"id": "faq-005", "question": "What is the rent relief and how much can I claim?",
     "answer": "The NTA 2025 replaces the old consolidated relief allowance with a rent relief: you may deduct 20% of your actual annual rent paid, capped at N500,000, from your income before applying the tax bands. Keep evidence of rent paid (receipts/tenancy agreement) as the revenue authority may request it.",
     "keywords": ["rent", "relief", "500000", "20%", "deduction", "cra"]},
    {"id": "faq-006", "question": "I run a small shop. What is presumptive tax?",
     "answer": "Presumptive taxation is a simplified regime for informal-sector operators whose records are not sufficient for standard assessment. Instead of accounting profits, you pay a small fixed levy set by a published schedule based on your trade category and turnover band. Collections under the new presumptive schedules begin once the enabling regulation is gazetted.",
     "keywords": ["presumptive", "informal", "levy", "band", "small business", "shop"]},
    {"id": "faq-007", "question": "How is my presumptive levy calculated?",
     "answer": "Your levy depends on (1) your annual turnover band (micro up to N1m; small N1m-N5m; medium N5m-N25m) and (2) your trade category, using the schedule published for your state (or the federal baseline). Operators below the N800,000 tax-free threshold are exempt. Above N25m turnover you graduate to the standard regime.",
     "keywords": ["presumptive", "levy", "turnover", "band", "calculate", "schedule"]},
    {"id": "faq-008", "question": "What is the CIT rate for companies in Nigeria?",
     "answer": "The standard Companies Income Tax (CIT) rate is 30% of assessable profit under the NTA 2025. Small companies pay 0% (see the small-company definition), and medium-sized companies historically paid 20% under transitional rules — check current guidance for your turnover band.",
     "keywords": ["cit", "companies income tax", "30%", "company", "corporate"]},
    {"id": "faq-009", "question": "What counts as a small company for CIT exemption?",
     "answer": "Under the NTA 2025, a small company is one with annual gross turnover of N100,000,000 or less AND total fixed assets of N250,000,000 or less. Small companies are taxed at 0% CIT and are also exempt from the development levy. Professional-service firms are excluded from small-company status.",
     "keywords": ["small company", "exemption", "100m", "250m", "cit", "turnover"]},
    {"id": "faq-010", "question": "What is the VAT rate in Nigeria?",
     "answer": "VAT is charged at 7.5% on taxable goods and services under the NTA 2025. Basic food items, medical and pharmaceutical products, educational books and materials, and exports are zero-rated or exempt as listed in the Act's schedules.",
     "keywords": ["vat", "7.5%", "rate", "value added tax"]},
    {"id": "faq-011", "question": "When must I register for VAT?",
     "answer": "You must register for VAT when your taxable turnover reaches N25,000,000 in a calendar year (the registration threshold). Voluntary registration is allowed below the threshold. Once registered, you charge 7.5% VAT on taxable supplies and file monthly returns.",
     "keywords": ["vat", "registration", "threshold", "25m", "register"]},
    {"id": "faq-012", "question": "When are VAT returns due?",
     "answer": "VAT returns are due monthly, on or before the 21st day of the month following the transaction month, filed with the NRS through the approved electronic filing channels.",
     "keywords": ["vat", "deadline", "21st", "filing", "monthly", "returns"]},
    {"id": "faq-013", "question": "What is withholding tax (WHT)?",
     "answer": "WHT is an advance payment of income tax deducted at source when certain payments (dividends, interest, rent, royalties, services, goods, commissions) are made. The payer deducts the prescribed percentage and remits it to the revenue authority; the payee receives a credit against their final tax liability.",
     "keywords": ["wht", "withholding", "deduction", "source", "advance"]},
    {"id": "faq-014", "question": "What are the WHT rates under the 2024 Regulations?",
     "answer": "Key rates under the Deduction of Tax at Source (Withholding) Regulations 2024: dividends 10%, interest 10%, rent 10%, royalties 5%, services/professional fees 5%, goods/supplies 2%, commission 5%. Rates double where the beneficiary has no TIN, and small transactions (up to N2,000,000) in goods/services are carved out.",
     "keywords": ["wht", "rates", "dividend", "interest", "rent", "services", "2024"]},
    {"id": "faq-015", "question": "What happens if I don't have a TIN as a supplier?",
     "answer": "Under the WHT Regulations 2024, the withholding rate on payments to a beneficiary without a TIN is doubled. Obtaining a TIN is free, so register before invoicing customers.",
     "keywords": ["wht", "no tin", "double", "rate", "supplier"]},
    {"id": "faq-016", "question": "What is the deadline for filing personal income tax returns?",
     "answer": "Self-employed individuals must file annual returns by 31 March of the following year. Employers file PAYE remittances monthly (by the 10th of the following month) and annual employer returns (Form H1) by 31 January.",
     "keywords": ["pit", "deadline", "filing", "march", "returns", "paye"]},
    {"id": "faq-017", "question": "What is PAYE?",
     "answer": "Pay-As-You-Earn is the system under which your employer deducts personal income tax from your salary each month using the statutory bands and remits it to your state internal revenue service. From 2026 the NTA 2025 bands apply, including the N800,000 tax-free threshold and rent relief.",
     "keywords": ["paye", "salary", "employer", "deduction", "monthly"]},
    {"id": "faq-018", "question": "What penalties apply for late filing?",
     "answer": "Under the NTAA 2025, late filing attracts administrative penalties: for individuals typically N50,000 for the first month and N25,000 for each subsequent month; for companies N100,000 for the first month and N50,000 monthly thereafter. Interest also accrues on unpaid tax at the prescribed rate.",
     "keywords": ["penalty", "late", "filing", "fine", "interest"]},
    {"id": "faq-019", "question": "What is a Tax Clearance Certificate (TCC)?",
     "answer": "A TCC is issued by the revenue authority confirming that your taxes are up to date for the three years preceding the application. It is required for many government transactions — loans, contracts, land transactions, and some licences. You apply through your tax office or the electronic portal once your filings and payments are current.",
     "keywords": ["tcc", "clearance", "certificate", "three years"]},
    {"id": "faq-020", "question": "Do I pay tax on my side hustle if I already pay PAYE?",
     "answer": "Yes. Employment income and business income are aggregated for personal income tax. Your employer handles PAYE on salary; you must file an annual self-assessment declaring the additional business income, and the tax already paid via PAYE is credited against the total.",
     "keywords": ["side hustle", "paye", "self-assessment", "business income", "aggregate"]},
    {"id": "faq-021", "question": "What is e-invoicing / MBS and does it affect me?",
     "answer": "The Merchant-Buyer Solution (MBS) is the NRS e-invoicing system: invoices are transmitted electronically and receive an Invoice Reference Number (IRN) and cryptographic stamp before or at issue. Large and medium taxpayers are onboarded first; small businesses join in later phases. Informal-sector operators under the presumptive regime are not required to e-invoice until they graduate.",
     "keywords": ["e-invoicing", "mbs", "irn", "invoice", "preclearance"]},
    {"id": "faq-022", "question": "Are pension and health insurance contributions deductible?",
     "answer": "Yes. Statutory pension contributions (minimum 8% employee / 10% employer under the Pension Reform Act), National Health Insurance Scheme contributions, National Housing Fund contributions and life assurance premiums remain deductible in computing chargeable income under the NTA 2025.",
     "keywords": ["pension", "nhf", "nhis", "deductible", "contributions"]},
    {"id": "faq-023", "question": "How is capital gains taxed?",
     "answer": "Under the NTA 2025, gains on the disposal of chargeable assets by individuals are taxed at the applicable PIT bands (integrated into income tax), while companies pay at their applicable CIT rate. Gains on Nigerian government bonds and small disposals below the annual threshold remain exempt.",
     "keywords": ["capital gains", "cgt", "assets", "disposal"]},
    {"id": "faq-024", "question": "Are cryptocurrencies and digital assets taxed?",
     "answer": "Yes. Under the NTA 2025 digital and virtual assets are chargeable assets: gains on disposal are taxable, and Virtual Asset Service Providers (VASPs) have reporting duties aligned with the OECD Crypto-Asset Reporting Framework. Keep records of acquisition cost and disposal proceeds.",
     "keywords": ["crypto", "digital assets", "vasp", "carf", "bitcoin"]},
    {"id": "faq-025", "question": "What is the development levy?",
     "answer": "The NTA 2025 consolidates several earmarked levies (education tax, NITDA levy, NASENI and police fund levies) into a single development levy charged on assessable profits of companies — 4% for large and medium companies. Small companies are exempt.",
     "keywords": ["development levy", "education tax", "nitda", "4%"]},
    {"id": "faq-026", "question": "What is stamp duty and who pays it?",
     "answer": "Stamp duty is charged on certain instruments (agreements, leases, share transfers, receipts above N10,000 for electronic transfers — the N50 electronic money transfer levy on inflows of N10,000 and above). The duty on electronic transfers is borne by the recipient; duties on instruments depend on the document type.",
     "keywords": ["stamp duty", "emtl", "n50", "instruments", "10000"]},
    {"id": "faq-027", "question": "How do I pay my presumptive levy and get a certificate?",
     "answer": "You can pay through an accredited field agent (cash, with an offline receipt), via USSD (*347*88#), or through the operator self-service web app. Payment is processed through a licensed payment switch (Remita, eTranzact or Flutterwave). On success a digitally signed payment certificate with a verifiable serial number is issued instantly, and the serial is also sent by SMS.",
     "keywords": ["pay", "certificate", "presumptive", "ussd", "agent", "remita"]},
    {"id": "faq-028", "question": "How can I verify a payment certificate?",
     "answer": "Every presumptive payment certificate carries a serial in the form PSM-YYYY-XXXXXXXXXX. Enter the serial on the public verification page (or dial *347*88# and choose 'Verify certificate'). The service checks the serial and the HMAC signature over the certificate payload — a genuine certificate verifies instantly; a forged or tampered one does not.",
     "keywords": ["verify", "certificate", "serial", "psm", "genuine"]},
    {"id": "faq-029", "question": "What records should a small business keep?",
     "answer": "Keep daily sales records, purchase invoices, bank statements, rent receipts, staff payroll records and asset purchase documents for at least six years. Good records let you prove your turnover band, claim deductions, and graduate smoothly to the standard regime as you grow.",
     "keywords": ["records", "bookkeeping", "six years", "small business"]},
    {"id": "faq-030", "question": "What is the difference between zero-rated and exempt goods for VAT?",
     "answer": "Zero-rated supplies (e.g. basic foods, exports, medicines, educational materials) are taxed at 0% and the seller can still recover input VAT. Exempt supplies (e.g. certain financial and medical services, land/lease transactions) attract no output VAT but the seller generally cannot recover related input VAT.",
     "keywords": ["zero-rated", "exempt", "vat", "input", "output"]},
    {"id": "faq-031", "question": "I trade in multiple states. Which state do I pay presumptive levy to?",
     "answer": "The presumptive levy is a state-level charge administered with the state internal revenue service where you are resident or principally operate. If you operate in more than one state, the levy schedule of your principal place of business applies; interstate attribution of consumption taxes follows the NTAA place-of-consumption rules.",
     "keywords": ["state", "multiple", "presumptive", "attribution", "residence"]},
    {"id": "faq-032", "question": "What is the role of the Tax Ombud?",
     "answer": "The Office of the Tax Ombud (established under the NTAA 2025) is an independent complaint-resolution body for taxpayers. You can lodge complaints about assessments, delays, refunds, conduct of tax officers and levies. Certain appeals require a deposit of 20% of the disputed amount while the matter is determined.",
     "keywords": ["ombud", "complaint", "appeal", "dispute", "20% deposit"]},
    {"id": "faq-033", "question": "Do informal workers like market traders pay income tax?",
     "answer": "Yes, but through the simplified presumptive regime rather than full accounts-based assessment. If your annual turnover is below N800,000 you are exempt. Otherwise you pay the small scheduled levy for your trade and band, and you receive a payment certificate as evidence of compliance.",
     "keywords": ["informal", "market", "trader", "presumptive", "exempt"]},
    {"id": "faq-034", "question": "What changed in Nigerian tax law in 2025/2026?",
     "answer": "Four Acts took effect on 1 January 2026: the Nigeria Tax Act (consolidating tax imposition laws), the Nigeria Tax Administration Act (common administration rules), the Nigeria Revenue Service Act (FIRS becomes the NRS), and the Joint Revenue Board Act. Headline changes: N800,000 PIT tax-free threshold, rent relief replacing CRA, small-company thresholds raised to N100m/N250m, a top PIT rate of 25%, a development levy, and new digital-asset and minimum-tax rules.",
     "keywords": ["nta", "2025", "2026", "changes", "new law", "nrs"]},
    {"id": "faq-035", "question": "How does the minimum tax / effective tax rate rule affect large companies?",
     "answer": "Large multinationals and domestic companies with turnover of N50 billion and above (or EUR750m for MNEs) are subject to a 15% minimum effective tax rate under the NTA 2025, with a top-up tax where their computed ETR falls below 15%. This aligns Nigeria with the OECD Pillar Two framework.",
     "keywords": ["minimum tax", "etr", "15%", "pillar two", "multinational", "top-up"]},
]

_STOPWORDS = {
    "the", "a", "an", "is", "are", "i", "my", "me", "do", "does", "what", "how",
    "when", "where", "who", "why", "can", "to", "of", "in", "on", "for", "and",
    "or", "it", "this", "that", "with", "be", "by", "at", "as", "if", "your",
    "you", "we", "they", "their", "much", "many", "from", "about", "which",
}

# Synonym groups to bridge informal phrasing -> domain terms.
_SYNONYMS = {
    "shop": "retail", "store": "retail", "kiosk": "retail", "market": "retail",
    "salary": "paye", "wage": "paye", "wages": "paye", "payroll": "paye",
    "house": "rent", "landlord": "rent", "apartment": "rent",
    "crypto": "digital assets", "bitcoin": "digital assets",
    "business": "company", "firm": "company", "enterprise": "company",
    "certificate": "tcc certificate",
    "fine": "penalty", "punishment": "penalty", "surcharge": "penalty",
    "levy": "presumptive levy",
    "vat": "vat value added tax",
    "number": "tin",
}


def _tokens(text: str) -> list[str]:
    raw = re.findall(r"[a-z0-9%']+", text.lower())
    out: list[str] = []
    for tok in raw:
        if tok in _STOPWORDS:
            continue
        out.append(tok)
        if tok in _SYNONYMS:
            out.extend(_SYNONYMS[tok].split())
    return out


@lru_cache(maxsize=1)
def _index() -> tuple[list[dict], dict[str, float]]:
    docs = []
    df: dict[str, int] = {}
    for faq in FAQ_CORPUS:
        toks = _tokens(faq["question"] + " " + faq["answer"] + " " + " ".join(faq["keywords"]))
        tf: dict[str, int] = {}
        for t in toks:
            tf[t] = tf.get(t, 0) + 1
        for t in set(toks):
            df[t] = df.get(t, 0) + 1
        # keyword boosts: terms in question/keywords weigh more
        boost = set(_tokens(faq["question"])) | set(faq["keywords"])
        docs.append({"faq": faq, "tf": tf, "boost": boost, "norm": math.sqrt(sum(v * v for v in tf.values())) or 1.0})
    n = len(docs)
    idf = {t: math.log(1 + n / (1 + c)) for t, c in df.items()}
    return docs, idf


def search(query: str, limit: int = 5) -> list[dict]:
    """Local TF-IDF-style search over the FAQ corpus."""
    docs, idf = _index()
    qtf: dict[str, int] = {}
    for t in _tokens(query):
        qtf[t] = qtf.get(t, 0) + 1
    qnorm = math.sqrt(sum(v * v for v in qtf.values())) or 1.0
    results = []
    for doc in docs:
        score = 0.0
        for t, qc in qtf.items():
            if t not in doc["tf"]:
                continue
            w = idf.get(t, 0.0)
            score += (qc / qnorm) * (doc["tf"][t] / doc["norm"]) * w * w
            if t in doc["boost"]:
                score += 0.5 * w
        if score > 0:
            results.append({"faq": doc["faq"], "score": round(score, 4)})
    results.sort(key=lambda r: -r["score"])
    return [
        {
            "id": r["faq"]["id"],
            "question": r["faq"]["question"],
            "answer": r["faq"]["answer"],
            "score": r["score"],
        }
        for r in results[:limit]
    ]


def corpus_size() -> int:
    return len(FAQ_CORPUS)
