# services/education (T14)

Nigerian tax education service (Python FastAPI).

- **Rules**: effective-dated tables as the embedded `rp-education-ng` pack
  (`app/packs/rp-education-ng/1.0.0.yaml`, §1.4 format): PIT 0% <= N800k up to
  25% above N50m, rent relief 20% capped N500k, CIT 30%, small-co
  N100m/N250m test, VAT 7.5%, WHT 2024 quick rates.
- **Calculators** (`POST /v1/calc/{pit,cit,vat,rent-relief,wht}`): each returns
  `amount_kobo` + `provision_citation` + `disclaimer` + full `trace`.
- **FAQ**: 35 real Nigerian tax FAQs (`app/faq.py`) with local TF-IDF search
  (`GET /v1/faq/search?q=`).
- **Chat** (`POST /v1/chat`): retrieval-grounded answers; numeric intents are
  routed to the deterministic calculators. No external LLM key required.
- **Embed**: `GET /embed/sdk.js` (JS snippet) + `GET /embed/chat` (iframe page).

## Run

```bash
python3 -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt
PORT=8103 python -m app.main        # or: uvicorn app.main:app --port 8103
```

## Test

```bash
pytest tests/ -q    # 18 tests
```

Honesty tags: pack is `subject_to_regazette` (NTA 2025 figures as passed, may
change on gazetting). Chat is retrieval/template-based (no generative model).

## Prod profile

The service has no privileged endpoints (calculators/FAQ/chat are public
educational content) and stores no PII, so the H1 prod vars are optional:
`PORT` (default 8103) is the only env it consumes. Auth, when the
enclave-gateway fronts it, is enforced at the gateway (Keycloak RS256,
`AUTH_MODE=keycloak` on the Go services).
