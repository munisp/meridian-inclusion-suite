# Meridian Inclusion Suite

Inclusion-plane services for the Meridian TaxTech platform (Nigerian NRS tax
platform): informal-sector onboarding, presumptive taxation, taxpayer
education, USSD access, and the field-agent / operator PWAs.

All services run **standalone in dev with zero external dependencies**. Every
external rail (NIMC, tin-graph, consent svc, TigerBeetle/ledger svc, PSSPs,
reg-watch, Redpanda, Temporal, SMS, LLM) sits behind an interface with a
working simulator or fallback selected by environment variables — see the
honesty table below.

## Layout

```
internal/platform/        shared Go platform: §1.3 httpx (auth, RFC7807, health),
                          ids (ULID), events (§1.1 envelope + inproc bus),
                          ledger (§1.5 scheme + dev TigerBeetle client + HTTP client),
                          store (embedded JSON store, STORE_FILE persistence)
services/onboarding/      T5  — Go :8101 — operator registry, NIMC verify,
                          TIN provision, consent, offline capture ingest, wf-onb-*
services/presumptive/     T12 — Go :8102 — band engine (embedded rp-* packs),
                          payment intent->pending->PSSP->capture/void,
                          certificates, agent float, gates, wf-psm-*
services/education/       T14 — Python FastAPI :8103 — NTA 2025 calculators,
                          35-FAQ corpus, grounded chat, embed SDK
services/ussd-gateway/    Go :8104 — menu DSL, session engine (180s TTL),
                          onboarding + presumptive trees, AT-style webhook,
                          /v1/simulate full-session simulator
apps/agent-pwa/           React 18 PWA :5201 — offline-first field capture,
                          IndexedDB outbox + background sync, commissions,
                          offline receipts
apps/operator-pwa/        React 18 PWA :5202 — profile, band view, payment ->
                          certificate, education widgets
```

## Quick start

### Go services (Go 1.22+, stdlib only)

```bash
go build ./... && go vet ./... && go test ./...
go run ./services/onboarding      # :8101
go run ./services/presumptive     # :8102
go run ./services/ussd-gateway    # :8104
```

### Education service (Python 3.12)

```bash
cd services/education
python3 -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt
pytest tests/ -q                  # 18 tests
PORT=8103 python -m app.main
```

### PWAs (Node 20)

```bash
cd apps/agent-pwa && npm install && npm run build     # dev: npm run dev -> :5201
cd apps/operator-pwa && npm install && npm run build  # dev: npm run dev -> :5202
```

### Everything at once (optional)

```bash
docker compose up --build
```

## 60-second smoke tour

```bash
# 1. Presumptive collections are gated (regulation not yet gazetted)
curl -s -X POST localhost:8102/v1/payments/intent -H 'X-Dev-Role: operator' \
  -H 'Content-Type: application/json' \
  -d '{"tin_hash":"demo","state":"Lagos","trade_category":"retail","annual_turnover_kobo":300000000,"provider":"remita"}'
# -> 403 intent_rejected (gate G8.presumptive_reg closed)

# 2. Flip the gate (board-authorized dev action)
curl -s -X POST localhost:8102/v1/gates/G8.presumptive_reg/flip \
  -H 'X-Dev-Role: admin' -H 'Content-Type: application/json' -d '{"open":true}'

# 3. Full payment saga -> certificate
curl -s -X POST localhost:8102/v1/workflows/wf-psm-payment/trigger \
  -H 'X-Dev-Role: operator' -H 'Content-Type: application/json' \
  -d '{"tin_hash":"demo","state":"Lagos","trade_category":"retail","annual_turnover_kobo":300000000,"provider":"remita"}'

# 4. Public certificate verification (no auth, rate-limited)
curl -s localhost:8102/v1/certificates/verify/PSM-2026-XXXXXXXXXX

# 5. Onboarding: register + provision TIN
curl -s -X POST localhost:8101/v1/operators -H 'X-Dev-Role: operator' \
  -H 'Content-Type: application/json' \
  -d '{"nin":"12345678901","full_name":"Adaeze Okafor","state":"Lagos","agent_id":"agent-1"}'
curl -s -X POST localhost:8101/v1/tin/provision -H 'X-Dev-Role: operator' \
  -H 'Content-Type: application/json' \
  -d '{"operator_id":"<id>","nin":"12345678901"}'

# 6. Offline batch ingest (idempotent — replay returns status "duplicate")
curl -s -X POST localhost:8101/v1/capture/batch -H 'X-Dev-Role: operator' \
  -H 'Idempotency-Key: demo-1' -H 'Content-Type: application/json' \
  -d '{"agent_id":"agent-1","items":[{"client_ref":"r1","nin":"12345678902","full_name":"Musa Garba","captured_at":"2026-03-01T08:00:00Z"}]}'

# 7. Commission settlement on ledger 700
curl -s -X POST localhost:8101/v1/workflows/wf-onb-commission-settlement/trigger \
  -H 'X-Dev-Role: admin'

# 8. USSD full session (built-in simulator)
curl -s -X POST localhost:8104/v1/simulate -H 'Content-Type: application/json' \
  -d '{"phone":"+2348011111111","inputs":["1","12345678901","Adaeze Okafor","1"]}'

# 9. Education: calculator with trace + grounded chat
curl -s -X POST localhost:8103/v1/calc/pit -H 'Content-Type: application/json' \
  -d '{"annual_gross_income_kobo":500000000,"annual_rent_paid_kobo":400000000}'
curl -s -X POST localhost:8103/v1/chat -H 'Content-Type: application/json' \
  -d '{"message":"What is the tax-free threshold for personal income tax?"}'
```

## Service API maps

### services/onboarding — Go :8101 (T5)

- `POST /v1/operators` · `GET /v1/operators[/{id}]` · `PATCH /v1/operators/{id}`
- `POST /v1/verify/nin` — NIMC adapter (simulator default; `NIMC_URL` for real)
- `POST /v1/tin/provision` · `POST /v1/verify/tin` — core tin-graph with
  deterministic local NIN=TIN-fusion fallback (`TIN_GRAPH_URL`)
- `POST /v1/consents` · `GET /v1/consents/{subject}` · `POST /v1/consents/{id}/revoke`
  — core consent svc with local fallback (`CONSENT_URL`)
- `POST /v1/capture/batch` (requires `Idempotency-Key`) — offline batch ingest:
  idempotency replay (`status: duplicate`), per-item outcomes
  (`created|duplicate_client_ref|conflict_resolved|rejected`), last-writer-wins
  conflict resolution by `captured_at`, >72h offline accepted but flagged
- `GET /v1/workflows` · `POST /v1/workflows/{name}/trigger` · `GET /v1/workflows/runs`
  — wf-onb-tin-provision, wf-onb-capture-ingest, wf-onb-ledger-rollup,
  wf-onb-commission-settlement (₦200/verified, ledger 700), wf-onb-filing-reminders,
  wf-onb-mbs-graduate (>₦25m ceiling)

### services/presumptive — Go :8102 (T12)

- `POST /v1/bands/evaluate` · `GET /v1/packs` — band engine over embedded
  rp-presumptive-federal/lagos/kano + rp-turnover-bands + rp-exemption-nta
  (exemption -> band -> levy, federal fallback for unknown states, full trace)
- `POST /v1/payments/intent` — gate check (403 when closed) -> band eval ->
  pending transfer on ledger 200 (code 1 authorise)
- `POST /v1/payments/{id}/authorise|capture|void` — PSSP adapter
  (remita/etranzact/flutterwave simulators) + post/void on ledger
- `POST /v1/pssp/webhook/{provider}` — HMAC-signed webhook callbacks
- `GET /v1/certificates/verify/{serial}` — public, rate-limited, HMAC-signed
  payload verification (serials `PSM-YYYY-XXXXXXXXXX`)
- `POST /v1/float/accounts|topup|debit` · `GET /v1/float/{agent}/balance|movements`
  — ledger 100 with DEBITS_MUST_NOT_EXCEED_CREDITS (overdraft -> 409)
- `GET /v1/gates` · `POST /v1/gates/{id}/flip` — reg-watch (`REG_WATCH_URL`) or
  local gate file (`GATE_FILE`); G8.presumptive_reg defaults CLOSED
- wf-psm-payment (full saga), wf-psm-float-monitor (₦5k threshold),
  wf-psm-settlement (captured vs ledger recon), wf-psm-simulation (cohorts,
  persisted), wf-psm-pack-rollout, wf-psm-gate-flip

### services/education — Python :8103 (T14)

- `POST /v1/calc/pit|cit|vat|rent-relief|wht` — each returns `amount_kobo`,
  `provision_citation`, `disclaimer`, full `trace`, `rule_pack`
- `GET /v1/pack` — embedded rp-education-ng (NTA 2025: PIT 0% ≤₦800k ... 25%
  >₦50m; rent relief 20% cap ₦500k; CIT 30%; small-co ₦100m/₦250m; VAT 7.5%;
  WHT Regs 2024 quick table)
- `GET /v1/faq` · `GET /v1/faq/search?q=` · `GET /v1/faq/{id}` — 35 real FAQs,
  local TF-IDF search
- `POST /v1/chat` — retrieval-grounded answers; numeric intents routed to the
  deterministic calculators; honest refusal when ungrounded; **no external LLM**
- `GET /embed/sdk.js` · `GET /embed/chat` — embed snippet + iframe page

### services/ussd-gateway — Go :8104

- **Menu graph DSL** (`menus.json`): options/input/action/end nodes, regex
  validation, `{{var}}` templating, option-level `set` variables.
- **Session engine**: in-mem store, **180s sliding TTL** (interface ready for Redis).
- **Onboarding tree**: register → NIN capture → TIN provision status.
- **Presumptive tree**: band lookup → pay → certificate serial via SMS (SMS
  transcript).
- **Africa's-Talking-style webhook**: `POST /webhook/ussd`
  (sessionId/serviceCode/phoneNumber/text, cumulative `*`-separated replay).
- **Simulator**: `POST /v1/simulate` — full-session transcript via curl.

## §1 conventions implemented

- **Event envelope**: ULID id, RFC3339 time, `trace_id`, `rule_pack_version`;
  in-proc bus fallback (Redpanda when wired at the platform level).
- **Ledger (§1.5)**: ledgers 100/200/700, codes 1-7, 128-bit account ids
  (namespace high-64 / entity serial low-64), pending/post/void semantics,
  DEBITS_MUST_NOT_EXCEED_CREDITS on float, integer kobo everywhere. Dev client
  by default; `LEDGER_URL` switches to the core ledger service.
- **Auth (§1.3)**: `X-Dev-Role: admin|operator|auditor` or HS256 dev JWT
  (`MERIDIAN_DEV_JWT_SECRET`); RFC7807 `application/problem+json` errors.
- **Pseudonymisation**: `nin_hash`/`tin_hash` HMAC-SHA256 (`NIN_HMAC_KEY`,
  `TIN_HMAC_KEY`); certificate HMAC (`CERT_HMAC_KEY`); webhook HMAC
  (`PSSP_WEBHOOK_SECRET`). No PII on the ledger or in events.
- **Rule packs (§1.4)**: embedded fallback copies (`subject_to_regazette`
  honesty tag) with effective dates and provenance.

## Simulated vs real (honesty table)

| Rail | Default (dev) | Real when |
| --- | --- | --- |
| NIMC verification | deterministic simulator (`0000` suffix = miss) | `NIMC_URL` |
| tin-graph | local NIN=TIN-fusion fallback | `TIN_GRAPH_URL` |
| consent svc | local store + NDPA receipts | `CONSENT_URL` |
| TigerBeetle / ledger svc | in-memory client with TB semantics | `LEDGER_URL` |
| PSSPs (Remita/eTranzact/Flutterwave) | authorise/capture/void simulators | adapter swap |
| reg-watch gates | local `GATE_FILE` (gate closed by default) | `REG_WATCH_URL` |
| Redpanda | in-proc bus | platform wiring |
| Temporal | in-process recorded workflow runs | platform wiring |
| SMS (cert serial) | simulated (in USSD transcript / response) | SMSC wiring |
| LLM (chat) | not used — TF-IDF retrieval + calculators | n/a |

Education pack figures are `subject_to_regazette` (NTA 2025 as passed; may
change on gazetting).
```
