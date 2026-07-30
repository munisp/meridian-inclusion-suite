# services/onboarding (T5) — Go :8101

Informal-sector onboarding: operator registry, NIMC identity verification,
TIN provisioning, NDPA consent capture, offline-first capture ingest, and the
`wf-onb-*` workflows. See the suite root README for the full API tour.

## Run

```bash
go run ./services/onboarding    # dev profile, zero external deps
```

## Environment

| Var | Purpose | Default (dev) |
|---|---|---|
| `PORT` | listen port | 8101 |
| `AUTH_MODE` / `KEYCLOAK_ISSUER` / `KEYCLOAK_AUDIENCE` / `KEYCLOAK_JWKS_URL` | §1.3 auth; `keycloak` = RS256 JWKS | dev (HS256 + `X-Dev-Role`) |
| `DATABASE_URL` | Postgres (pgx/v5) backing store | embedded JSON store (`STORE_FILE`) |
| `KAFKA_BROKERS` | Redpanda brokers for `nrs.onb.*` | embedded inproc bus |
| `NIMC_API_URL` / `NIMC_API_KEY` | real NIMC adapter: `POST {url}/verify`, HMAC-SHA256 signed, 3-retry backoff, breaker (5 failures → open 30s) | simulator (`NIMC_URL` legacy alias honoured) |
| `TIN_GRAPH_URL` | core tin-graph API | deterministic local fusion fallback |
| `CONSENT_URL` | core consent svc | local NDPA receipts |
| `LEDGER_URL` | core ledger svc (commission settlement, ledger 700) | dev ledger client |
| `NIN_HMAC_KEY` / `TIN_HMAC_KEY` | §1.3 pseudonymisation keys | dev keys |

## Prod profile

Set `AUTH_MODE=keycloak`, `DATABASE_URL`, `KAFKA_BROKERS`, `NIMC_API_URL` +
`NIMC_API_KEY` (and the core URLs). Startup logs `profile=prod
component=...` per component and never fails when a prod var is missing —
it falls back to the dev component with a log line. Raw NIN/TIN are never
logged; only `nin_hash`/`tin_hash`.

## Tests

`go test ./services/onboarding/` — registry/capture/consent/workflows plus
the NIMC HTTP adapter (HMAC signing, retry, circuit breaker).
