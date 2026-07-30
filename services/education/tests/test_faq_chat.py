"""Tests for the FAQ corpus, local search and grounded chat endpoint."""
from fastapi.testclient import TestClient

from app import chat, faq
from app.main import app

client = TestClient(app)


def test_corpus_has_at_least_30_faqs():
    assert faq.corpus_size() >= 30
    ids = [f["id"] for f in faq.FAQ_CORPUS]
    assert len(set(ids)) == len(ids)
    for f in faq.FAQ_CORPUS:
        assert f["question"].endswith("?")
        assert len(f["answer"]) > 80


def test_search_relevance():
    hits = faq.search("what is the VAT rate")
    assert hits and "7.5%" in hits[0]["answer"]
    hits = faq.search("how much rent relief can I claim on my house")
    assert hits and "500,000" in hits[0]["answer"]
    hits = faq.search("do market traders pay tax")
    assert hits and "presumptive" in hits[0]["answer"].lower()


def test_chat_grounded_answer():
    r = chat.answer("What is the tax-free threshold for personal income tax?")
    assert r["grounded"] is True
    assert "800,000" in r["answer"]
    assert r["sources"]


def test_chat_calculator_intent_pit():
    r = chat.answer("How much PIT do I pay on ₦5,000,000 a year?")
    assert r.get("calculator") == "pit"
    assert r["calculation"]["amount_kobo"] == 69_000_000
    assert "690,000" in r["answer"]


def test_chat_calculator_intent_vat():
    r = chat.answer("What is the VAT on ₦100,000 worth of goods?")
    assert r.get("calculator") == "vat"
    assert "7,500" in r["answer"]


def test_chat_refuses_ungrounded():
    r = chat.answer("Who won the football match yesterday?")
    assert r["grounded"] is False
    assert "don't have a grounded answer" in r["answer"]


def test_api_endpoints():
    assert client.get("/healthz").json()["service"] == "education"
    ready = client.get("/readyz")
    assert ready.status_code == 200 and ready.json()["faqs"] >= 30
    r = client.post("/v1/calc/pit", json={"annual_gross_income_kobo": 300_000_000})
    assert r.status_code == 200 and r.json()["amount_kobo"] == 33_000_000
    r = client.get("/v1/faq/search", params={"q": "WHT rates"})
    assert r.status_code == 200 and r.json()["results"]
    r = client.post("/v1/chat", json={"message": "When are VAT returns due?"})
    assert r.status_code == 200 and "21st" in r.json()["answer"]
    r = client.get("/embed/sdk.js")
    assert r.status_code == 200 and "iframe" in r.text
    r = client.get("/embed/chat")
    assert r.status_code == 200 and "/v1/chat" in r.text
