"""services/education (SPEC §4, T14) — Nigerian tax education service.

Effective-dated rules tables (rp-education-ng pack), calculators with full
calc traces + citations + disclaimers, a >=30-item FAQ corpus with local
search, a retrieval-grounded chat endpoint (no external LLM), and an embed
SDK (JS snippet + iframe page).
"""
from __future__ import annotations

import os
from typing import Optional

from fastapi import FastAPI, HTTPException, Query
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import HTMLResponse, PlainTextResponse
from pydantic import BaseModel, Field

from . import calculators, chat, faq
from .otel import TenantBaggageMiddleware, init_otel
from .packs import load_pack, pack_ref

SERVICE = "education"
VERSION = "1.0.0"

app = FastAPI(title="Meridian Education Service (T14)", version=VERSION)
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_methods=["*"],
    allow_headers=["*"],
)
# OTel (DESIGN-CONTRACT.md): tenant baggage first, then init_otel adds the
# FastAPI server-span middleware outermost. Fail-soft: never raises.
app.add_middleware(TenantBaggageMiddleware)
init_otel(app)


@app.get("/healthz")
def healthz():
    return {"status": "ok", "service": SERVICE, "version": VERSION}


@app.get("/readyz")
def readyz():
    pack = load_pack()
    return {"status": "ready", "pack": pack_ref(pack), "faqs": faq.corpus_size()}


@app.get("/v1/pack")
def get_pack():
    return load_pack()


# ---------------------------------------------------------------- calculators

class PITRequest(BaseModel):
    annual_gross_income_kobo: int = Field(ge=0)
    annual_rent_paid_kobo: int = Field(default=0, ge=0)


class CITRequest(BaseModel):
    annual_turnover_kobo: int = Field(ge=0)
    assessable_profit_kobo: int = Field(ge=0)
    total_fixed_assets_kobo: int = Field(default=0, ge=0)


class VATRequest(BaseModel):
    amount_kobo: int = Field(ge=0)
    inclusive: bool = False


class RentReliefRequest(BaseModel):
    annual_rent_paid_kobo: int = Field(ge=0)


class WHTRequest(BaseModel):
    payment_type: str
    amount_kobo: int = Field(ge=0)
    beneficiary_has_tin: bool = True


@app.post("/v1/calc/pit")
def pit(req: PITRequest):
    return calculators.calc_pit(req.annual_gross_income_kobo, req.annual_rent_paid_kobo)


@app.post("/v1/calc/cit")
def cit(req: CITRequest):
    return calculators.calc_cit(req.annual_turnover_kobo, req.assessable_profit_kobo, req.total_fixed_assets_kobo)


@app.post("/v1/calc/vat")
def vat(req: VATRequest):
    return calculators.calc_vat(req.amount_kobo, req.inclusive)


@app.post("/v1/calc/rent-relief")
def rent_relief(req: RentReliefRequest):
    return calculators.calc_rent_relief(req.annual_rent_paid_kobo)


@app.post("/v1/calc/wht")
def wht(req: WHTRequest):
    try:
        return calculators.calc_wht(req.payment_type, req.amount_kobo, req.beneficiary_has_tin)
    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e))


# ----------------------------------------------------------------------- FAQ

@app.get("/v1/faq")
def list_faq():
    return {"count": faq.corpus_size(), "faqs": [{"id": f["id"], "question": f["question"]} for f in faq.FAQ_CORPUS]}


@app.get("/v1/faq/search")
def search_faq(q: str = Query(min_length=2), limit: int = Query(default=5, ge=1, le=20)):
    return {"query": q, "results": faq.search(q, limit)}


@app.get("/v1/faq/{faq_id}")
def get_faq(faq_id: str):
    for f in faq.FAQ_CORPUS:
        if f["id"] == faq_id:
            return f
    raise HTTPException(status_code=404, detail="faq not found")


# ---------------------------------------------------------------------- chat

class ChatMessage(BaseModel):
    role: str = "user"
    content: str


class ChatRequest(BaseModel):
    message: str
    history: Optional[list[ChatMessage]] = None


@app.post("/v1/chat")
def chat_endpoint(req: ChatRequest):
    history = [h.model_dump() for h in req.history] if req.history else None
    return chat.answer(req.message, history)


# ----------------------------------------------------------------- embed SDK

IFRAME_HTML = """<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>NRS Tax Help</title>
<style>
  :root { color-scheme: light; }
  body { font-family: ui-sans-serif, system-ui, sans-serif; margin: 0; background: #faf7f2; color: #3f3a33; display: flex; flex-direction: column; height: 100vh; }
  header { background: #efe7da; padding: 10px 14px; font-weight: 600; border-bottom: 1px solid #ddd2c0; }
  #log { flex: 1; overflow-y: auto; padding: 12px; }
  .msg { max-width: 85%; margin: 6px 0; padding: 8px 12px; border-radius: 10px; line-height: 1.45; white-space: pre-wrap; }
  .user { background: #d9c9ae; margin-left: auto; }
  .bot { background: #ffffff; border: 1px solid #e3d9c8; }
  form { display: flex; gap: 8px; padding: 10px; border-top: 1px solid #ddd2c0; background: #f4eee3; }
  input { flex: 1; padding: 10px; border: 1px solid #cbbfa8; border-radius: 8px; }
  button { padding: 10px 16px; border: 0; border-radius: 8px; background: #8a6d3b; color: #fff; font-weight: 600; cursor: pointer; }
  .hint { font-size: 12px; color: #8c8375; padding: 0 12px 8px; }
</style>
</head>
<body>
<header>NRS Tax Help — education assistant</header>
<div id="log"></div>
<div class="hint">Answers are grounded in the NRS FAQ corpus and NTA 2025 calculators. Educational only, not an assessment.</div>
<form id="f"><input id="q" placeholder="Ask about PIT, VAT, presumptive levy..." autocomplete="off"><button>Send</button></form>
<script>
const API = new URL(document.currentScript?.src || location.href).origin;
const log = document.getElementById('log');
function add(cls, text) { const d = document.createElement('div'); d.className = 'msg ' + cls; d.textContent = text; log.appendChild(d); log.scrollTop = log.scrollHeight; }
add('bot', 'Hello! Ask me anything about Nigerian tax — PIT bands, VAT, presumptive levy, WHT, deadlines, certificates.');
document.getElementById('f').addEventListener('submit', async (e) => {
  e.preventDefault();
  const q = document.getElementById('q');
  const text = q.value.trim(); if (!text) return;
  add('user', text); q.value = '';
  try {
    const r = await fetch(API + '/v1/chat', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({message: text}) });
    const j = await r.json();
    add('bot', j.answer + (j.grounded ? '' : '\\n\\n(Not found in the FAQ corpus — answer unverified.)'));
  } catch (err) { add('bot', 'Service unavailable. Please try again.'); }
});
</script>
</body>
</html>
"""

SDK_JS = """/* Meridian T14 embed SDK: injects the NRS tax-help iframe.
   Usage: <script src="https://<education-host>/embed/sdk.js" data-target="#tax-help"></script> */
(function () {
  var script = document.currentScript;
  var origin = new URL(script.src).origin;
  var targetSel = script.getAttribute('data-target') || '#meridian-tax-help';
  function mount() {
    var host = document.querySelector(targetSel);
    if (!host) {
      host = document.createElement('div');
      host.id = targetSel.replace(/^#/, '');
      document.body.appendChild(host);
    }
    var iframe = document.createElement('iframe');
    iframe.src = origin + '/embed/chat';
    iframe.title = 'NRS Tax Help';
    iframe.style.cssText = 'width:100%;height:560px;border:1px solid #ddd2c0;border-radius:12px;background:#faf7f2';
    iframe.loading = 'lazy';
    host.appendChild(iframe);
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', mount);
  else mount();
})();
"""


@app.get("/embed/sdk.js", response_class=PlainTextResponse)
def embed_sdk():
    return PlainTextResponse(SDK_JS, media_type="application/javascript")


@app.get("/embed/chat", response_class=HTMLResponse)
def embed_chat():
    return HTMLResponse(IFRAME_HTML)


def main():
    import uvicorn

    uvicorn.run("app.main:app", host="0.0.0.0", port=int(os.getenv("PORT", "8103")), reload=False)


if __name__ == "__main__":
    main()
