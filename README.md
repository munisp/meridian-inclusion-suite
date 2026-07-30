# meridian-inclusion-suite

Meridian TaxTech platform — **Inclusion Plane** (Market Zone) for the Nigerian
NRS unified tax platform. Covers SPEC §4 items **T5** (informal-sector
onboarding), **T12** (presumptive taxation), **T14** (taxpayer education) plus
the USSD gateway and the two field PWAs. Implements the binding conventions of
SPEC §1 (event envelopes, service conventions, rule packs, TigerBeetle ledger
scheme). Pins core contracts v1 (see SPEC.md at suite root; no cross-repo
source imports — needed schema types are copied locally).

## Layout

```
inclusion-suite/
├── go.mod                       # one Go module for all Go services (stdlib only)
├── internal/platform/           # shared §1 plumbing
│   ├── httpx/                   # healthz/readyz, RFC7807, CORS, dev auth (HS256 JWT / X-Dev-Role)
│   ├── ids/                     # ULID-style ids
│   ├── events/                  # §1.1 envelope + inproc bus (EVENT_BUS=inproc fallback)
│   ├── ledger/                  # §1.5 LedgerClient + dev in-memory TigerBeetle semantics + HTTP client
│   └── store/                   # embedded JSON store (STORE_FILE optional) — zero-dep dev mode
├── services/
│   ├── onboarding/    (Go, :8101)    T5
│   ├── presumptive/   (Go, :8102)    T12
│   ├── education/     (FastAPI,:8103) T14
│   └── ussd-gateway/  (Go, :8104)
├── apps/
│   ├── agent-pwa/     (React 18 + TS + Vite + Tailwind PWA, :5201)
│   └── operator-pwa/  (React 18 + TS + Vite + Tailwind PWA, :5202)
└── docker-compose.yml           # optional convenience runner
```

## Quick start (dev, zero external deps)

Every service runs standalone: external rails (NIMC, tin-graph, consent svc,
core ledger/TigerBeetle, PSSPs, reg-watch, Redpanda, Temporal) sit behind
interfaces with **working simulators/fallbacks** selected by env vars.

```bash
# Go services (Go 1.22+)
export PATH=$HOME/sdk/go/bin:$PATH
go build ./... && go vet ./... && go test ./...

go run ./services/onboarding      # :8101
go run ./services/presumptive     # :8102  (collections gated until gate flipped, see below)
go run ./services/ussd-gateway    # :8104

# Education (Python 3.12)
cd services/education
python3 -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt
python -m app.main                # :8103
pytest tests/ -q                  # 18 tests

# PWAs (Node 20)
cd apps/agent-pwa    && npm install && npm run build
cd apps/operator-pwa && npm install && npm run build
```

Or `docker compose up --build` (builds all four services + serves both PWAs).

### 60-second smoke tour

```bash
# open the presumptive collections gate (dev, local gate file fallback)
curl -X POST localhost:8102/v1/gates/G8.presumptive_reg/flip -H 'X-Dev-Role: admin' -d '{"open":true}'

# onboarding: register -> NIN verify -> TIN provision
curl -X POST localhost:8101/v1/operators -H 'X-Dev-Role: operator' -H 'Content-Type: application/json' \
  -d '{"nin":"12345678901","full_name":"Adaeze Okafor","agent_id":"agent-1"}'     # -> op_...
curl -X POST localhost:8101/v1/tin/provision -H 'X-Dev-Role: operator' -H 'Content-Type: application/json' \
  -d '{"operator_id":"<op_id>","nin":"12345678901"}'                              # wf-onb-tin-provision

# offline capture batch (idempotent)
curl -X POST localhost:8101/v1/capture/batch -H 'X-Dev-Role: operator' \
  -H 'Idempotency-Key: batch-1' -H 'Content-Type: application/json' \
  -d '{"agent_id":"agent-1","items":[{"client_ref":"r1","nin":"23456789012","full_name":"Musa Garba","captured_at":"2026-01-01T08:00:00Z"}]}'

# presumptive: band -> pay (simulated PSSP) -> certificate -> public verify
curl -X POST localhost:8102/v1/bands/evaluate -H 'X-Dev-Role: operator' -H 'Content-Type: application/json' \
  -d '{"state":"Lagos","trade_category":"retail","annual_turnover_kobo":300000000}'
curl -X POST localhost:8102/v1/workflows/wf-psm-payment/trigger -H 'X-Dev-Role: operator' \
  -H 'Content-Type: application/json' -d '{"tin_hash":"abc123","state":"Lagos","trade_category":"retail","annual_turnover_kobo":300000000,"provider":"remita"}'
curl localhost:8102/v1/certificates/verify/<serial>          # public, rate-limited

# ussd: full session via the built-in simulator
curl -X POST localhost:8104/v1/simulate -H 'Content-Type: application/json' \
  -d '{"phone":"+2348011111111","inputs":["1","12345678901","Adaeze Okafor","1"]}'

# education: calculator + grounded chat
curl -X POST localhost:8103/v1/calc/pit -H 'Content-Type: application/json' -d '{"annual_gross_income_kobo":500000000}'
curl -X POST localhost:8103/v1/chat -H 'Content-Type: application/json' -d '{"message":"What is the tax-free threshold?"}'
```

## services/onboarding (T5) — Go :8101

- **Operator registry CRUD**: `POST/GET /v1/operators`, `GET/PATCH /v1/operators/{id}`
- **NIMC verification adapter** (`POST /v1/verify/nin`): real HTTP client
  (`POST {NIMC_API_URL}/verify`, HMAC-SHA256 signed with `NIMC_API_KEY`,
  3-retry backoff + circuit breaker) when `NIMC_API_URL` set, else
  deterministic simulator (11-digit NIN; `...0000` simulates a miss).
  Legacy `NIMC_URL` is honoured as an alias. Raw NINs are never logged —
  `nin_hash` pseudonyms only.
- **TIN provisioning** (`POST /v1/tin/provision`): core tin-graph API when
  `TIN_GRAPH_URL` set with **automatic local fallback** (deterministic NIN=TIN
  fusion approximation); `POST /v1/verify/tin`.
- **Pseudonymisation**: `nin_hash`/`tin_hash` = HMAC-SHA256(value,
  `NIN_HMAC_KEY`/`TIN_HMAC_KEY`) per §1.3; events carry hashes only.
- **Consent capture** (`POST /v1/consents`, `GET /v1/consents/{subject}`,
  `POST /v1/consents/{id}/revoke`): core consent svc when `CONSENT_URL` set,
  else local NDPA receipts.
- **Offline capture ingest** (`POST /v1/capture/batch`): requires
  `Idempotency-Key` (replays return `status:"duplicate"`); per-item conflict
  resolution — `duplicate_client_ref`, NIN-identity conflicts resolved
  last-writer-wins by `captured_at`; **≥72h offline tolerance** (older records
  accepted but flagged; design note in `capture.go`).
- **wf-onb-\*** (`GET /v1/workflows`, `POST /v1/workflows/{name}/trigger`,
  `GET /v1/workflows/runs`): `wf-onb-tin-provision`, `wf-onb-capture-ingest`,
  `wf-onb-ledger-rollup`, `wf-onb-commission-settlement` (ledger **700** via
  core ledger API when `LEDGER_URL` set, dev client fallback),
  `wf-onb-filing-reminders`, `wf-onb-mbs-graduate`. Events: `nrs.onb.*`.

## services/presumptive (T12) — Go :8102

- **Band engine** (`POST /v1/bands/evaluate`, `GET /v1/packs`): evaluates
  embedded fallback copies of `rp-presumptive-federal`, `rp-presumptive-lagos`,
  `rp-presumptive-kano` + `rp-turnover-bands` + `rp-exemption-nta`
  (`services/presumptive/packs/*.json`, mirrors of the §1.4 YAML packs in
  meridian-rule-packs). Exemption below ₦800k; graduation above ₦25m; full trace.
- **Payments**: `POST /v1/payments/intent` → pending transfer (ledger **200**,
  code 1 authorise) → `POST /v1/payments/{id}/authorise` (PSSP adapter) →
  `POST /v1/payments/{id}/capture` (post pending, code 2) / `{id}/void` (code 3).
- **PSSP adapters** (Remita / eTranzact / Flutterwave-class interface) with
  deterministic simulators; **webhook callbacks** at
  `POST /v1/pssp/webhook/{provider}` (HMAC-signed, `PSSP_WEBHOOK_SECRET`).
- **Certificates**: serial `PSM-YYYY-XXXXXXXXXX`, **HMAC-signed payload**
  (`CERT_HMAC_KEY`); public rate-limited `GET /v1/certificates/verify/{serial}`
  (20/min per client, 429 beyond).
- **Agent float** (ledger **100**, `DEBITS_MUST_NOT_EXCEED_CREDITS` enforced by
  the ledger client): `POST /v1/float/accounts|topup|debit`,
  `GET /v1/float/{agent}/balance|movements`. Overdraft → 409.
- **Gate enforcement**: collections **blocked** until the presumptive gate
  opens — reg-watch API when `REG_WATCH_URL` set, else local gate file
  (`GATE_FILE`, default closed). `GET /v1/gates`, `POST /v1/gates/{id}/flip`.
- **wf-psm-\***: `wf-psm-payment` (full saga), `wf-psm-float-monitor`,
  `wf-psm-settlement` (captured vs ledger recon), `wf-psm-simulation` (band
  scenarios over operator cohorts, persisted at `GET /v1/simulations`),
  `wf-psm-pack-rollout`, `wf-psm-gate-flip`. Events: `nrs.psm.*`.

## services/education (T14) — FastAPI :8103

- Embedded **`rp-education-ng`** pack (`app/packs/rp-education-ng/1.0.0.yaml`,
  §1.4 format): PIT 0% ≤₦800k … 25% >₦50m; rent relief 20% capped ₦500k;
  CIT 30%; small-co ₦100m/₦250m; VAT 7.5%; WHT 2024 quick table.
- **Calculators** `POST /v1/calc/{pit,cit,vat,rent-relief,wht}` — each returns
  `amount_kobo` + `provision_citation` + `disclaimer` + full `trace`.
- **FAQ corpus**: 35 real Nigerian tax FAQs, local TF-IDF search
  (`GET /v1/faq`, `/v1/faq/search?q=`, `/v1/faq/{id}`).
- **Chat** `POST /v1/chat`: retrieval-grounded answers; numeric intents routed
  to the calculators; refuses ungrounded questions. **No external LLM key.**
- **Embed**: `GET /embed/sdk.js` (JS snippet) + `GET /embed/chat` (iframe page).

## services/ussd-gateway — Go :8104

- **Menu graph DSL** (`menus.json`): options/input/action/end nodes, regex
  validation, `{{var}}` templating, option-level `set` variables.
- **Session engine**: in-mem store, **180s sliding TTL** (interface ready for Redis).
- **Onboarding tree**: register → NIN capture → TIN provision status.
- **Presumptive tree**: band lookup → pay → certificate serial via SMS (SMS
  simulated in the transcript).
- **Africa's-Talking-style webhook** `POST /webhook/ussd` (form:
  sessionId/serviceCode/phoneNumber/text → `CON `/`END `), plus **simulator**
  `POST /v1/simulate` for full-session curl testing. Events: `nrs.onb.ussd.v1`,
  `nrs.psm.ussd.v1`.

## apps/agent-pwa (:5201)

Mobile-first offline-first PWA for field agents: operator capture forms →
**IndexedDB outbox** (`idb`), background sync (SW `sync` event + `online`
listener) to `capture-ingest` with idempotency keys; commission dashboard
(reads onboarding registry + presumptive float); **cash_receipt_offline** with
device-signed (SHA-256) receipts. Manual manifest + service worker (no
workbox). Warm-neutral low-saturation design (sand palette, no gradients).

## apps/operator-pwa (:5202)

Operator self-service: profile (on-device, pseudonymised on the wire),
presumptive band view with calc trace, payment flow (gate-aware) → certificate
display + public verify link, education widgets (PIT calculator, FAQ search,
T14 chat iframe embed).

## Honesty tags (what is simulated)

| External rail | Behaviour |
|---|---|
| NIMC | simulator when `NIMC_API_URL` unset (deterministic personas; `...0000` = miss); real signed HTTP adapter + breaker when set |
| Core tin-graph | local deterministic NIN=TIN fusion fallback when `TIN_GRAPH_URL` unset/unreachable |
| Core consent svc | local NDPA receipt store fallback (`CONSENT_URL`) |
| TigerBeetle / core ledger | dev in-memory client with TB semantics (`LEDGER_URL` switches to core svc; `TIGERBEETLE_ADDRESSES` handled in core) |
| PSSPs (Remita/eTranzact/Flutterwave) | deterministic simulators; webhook HMAC dev secret |
| reg-watch | local gate file fallback (`GATE_FILE`), gate default **closed** |
| Redpanda | in-process bus (`internal/platform/events`), same envelope |
| Temporal | dev in-process workflow runner (recorded runs) |
| SMS (USSD cert delivery) | simulated in USSD transcript / event log |
| Education chat | retrieval + templates over local FAQ corpus (no generative LLM) |

Rule-pack figures are `subject_to_regazette: true` (NTA 2025 as passed; may
change on gazetting).

## Conventions implemented (SPEC §1)

- `GET /healthz` / `GET /readyz` on every HTTP service; env config; `PORT`.
- Auth: `X-Dev-Role` or HS256 dev JWT (`MERIDIAN_DEV_JWT_SECRET`) in dev;
  public paths: health, PSSP webhooks, certificate verify, USSD webhook/sim.
- RFC7807 `application/problem+json` errors.
- Money: **integer kobo only** end-to-end.
- Events: §1.1 envelope (ULID id, RFC3339 time, trace_id, rule_pack_version).
- Ledger: §1.5 ids 100/200/700 used here; codes 1–7; 128-bit account ids
  (namespace high-64 / entity serial low-64); no PII on ledger accounts.

## Production profile (HARDENING H1 env contract)

Every real integration is env-selected; with zero config the suite keeps
running in dev profile. Startup **never fails** because a prod var is
missing — each component logs `profile=dev|prod component=<name>` at boot.

| Var | Purpose | Default (dev) |
|---|---|---|
| `AUTH_MODE` | `dev` (HS256 + `X-Dev-Role`) or `keycloak` | dev |
| `KEYCLOAK_ISSUER` | e.g. `https://keycloak:8443/realms/meridian` | unset |
| `KEYCLOAK_AUDIENCE` | expected `aud` (e.g. `meridian-services`) | unset |
| `KEYCLOAK_JWKS_URL` | defaults to `{issuer}/protocol/openid-connect/certs` | derived |
| `MERIDIAN_DEV_JWT_SECRET` | dev-mode HMAC secret | `meridian-dev-secret` |
| `DATABASE_URL` | `postgres://user:pass@host:5432/dbname` (pgx/v5, auto-migrated jsonb doc table) | unset → embedded JSON store (`STORE_FILE`) |
| `KAFKA_BROKERS` | comma list (Redpanda; franz-go producer/consumer, group = service name) | unset → embedded inproc bus |
| `NIMC_API_URL` / `NIMC_API_KEY` | NIMC identity adapter (`POST {url}/verify`, HMAC-signed, retry+breaker) | unset → simulator |
| `PSSP_API_URL` / `PSSP_API_KEY` | PSSP payment adapter (init/capture/verify, HMAC-signed, retry+breaker); also the webhook HMAC secret | unset → simulators |
| `USSD_AGGREGATOR_URL` / `USSD_AGGREGATOR_KEY` | aggregator outbound notify + inbound webhook HMAC verification | unset → simulator/unsigned dev |
| `STORE_FILE` | embedded store persistence path (dev) | in-memory |
| `TIN_GRAPH_URL` / `CONSENT_URL` / `LEDGER_URL` / `REG_WATCH_URL` / `NIN_HMAC_KEY` / `TIN_HMAC_KEY` / `CERT_HMAC_KEY` / `GATE_FILE` | suite-local rails (see service sections) | dev fallbacks |
| `PORT` | listen port | 8101/8102/8103/8104 |

### Running the prod profile

```bash
export AUTH_MODE=keycloak KEYCLOAK_ISSUER=https://keycloak:8443/realms/meridian KEYCLOAK_AUDIENCE=meridian-services
export DATABASE_URL=postgres://meridian:secret@postgres:5432/inclusion
export KAFKA_BROKERS=redpanda:9092
export NIMC_API_URL=https://nimc.example/api NIMC_API_KEY=...
export PSSP_API_URL=https://pssp.example/api PSSP_API_KEY=...
export USSD_AGGREGATOR_URL=https://agg.example/api USSD_AGGREGATOR_KEY=...
go run ./services/onboarding   # auth=RS256/JWKS, store=postgres, bus=kafka, nimc=real
```

- **Auth (H2)**: `AUTH_MODE=keycloak` switches every Go service to RS256
  Bearer validation against the Keycloak JWKS (5-min cache, refresh on
  unknown kid; `iss`/`exp`/`aud` validated; Keycloak `realm_access.roles`
  mapped to §1.3 roles). Dev mode is unchanged. The PWAs use oidc-client-ts
  authorization-code+PKCE when `VITE_AUTH_MODE=keycloak`
  (`VITE_KEYCLOAK_ISSUER`, `VITE_KEYCLOAK_CLIENT_ID`), tokens in memory with
  silent renew; the dev-token (`X-Dev-Role`) path stays the default.
- **Adapters (H4)**: NIMC / PSSP / USSD-aggregator real clients all use
  HMAC-SHA256 request signing, 3-retry exponential backoff and a circuit
  breaker (5 failures → open 30s). Raw NIN/TIN/MSISDN are never logged —
  hash pseudonyms only.

## CI

`.github/workflows/ci.yml` (Go build/vet/test -race, education pytest, both
PWA `npm ci` + build). A verbatim copy lives at `ci/workflows/ci.yml` (see
`ci/README.md`) for tokens without GitHub `workflow` scope.
