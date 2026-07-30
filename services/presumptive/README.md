# services/presumptive (T12) — Go :8102

Presumptive taxation: turnover-band engine (rp-presumptive-* +
rp-turnover-bands), payment intent → PSSP authorise → capture/void,
HMAC-signed certificates with public verification, agent float management
(ledger 100, `DEBITS_MUST_NOT_EXCEED_CREDITS`), gate enforcement, and the
`wf-psm-*` workflows. See the suite root README for the full API tour.

## Run

```bash
go run ./services/presumptive   # dev profile, zero external deps
# collections are gated until the presumptive gate is opened:
curl -X POST localhost:8102/v1/gates/G8.presumptive_reg/flip -H 'X-Dev-Role: admin' -d '{"open":true}'
```

## Environment

| Var | Purpose | Default (dev) |
|---|---|---|
| `PORT` | listen port | 8102 |
| `AUTH_MODE` / `KEYCLOAK_ISSUER` / `KEYCLOAK_AUDIENCE` / `KEYCLOAK_JWKS_URL` | §1.3 auth; `keycloak` = RS256 JWKS | dev (HS256 + `X-Dev-Role`) |
| `DATABASE_URL` | Postgres (pgx/v5) backing store | embedded JSON store (`STORE_FILE`) |
| `KAFKA_BROKERS` | Redpanda brokers for `nrs.psm.*` | embedded inproc bus |
| `PSSP_API_URL` / `PSSP_API_KEY` | real PSSP adapter (provider `pssp`): `/payments/init|capture|verify|void`, HMAC-SHA256 signed, 3-retry backoff, breaker (5 failures → open 30s); `PSSP_API_KEY` also signs/verifies webhooks | provider simulators (remita/etranzact/flutterwave) |
| `PSSP_WEBHOOK_SECRET` | webhook HMAC secret when `PSSP_API_KEY` unset | dev secret |
| `REG_WATCH_URL` / `GATE_FILE` | gate enforcement source | local gate file (default closed) |
| `LEDGER_URL` | core ledger svc | dev ledger client |
| `CERT_HMAC_KEY` / `TIN_HMAC_KEY` | certificate signing / pseudonymisation keys | dev keys |

## Prod profile

Set `AUTH_MODE=keycloak`, `DATABASE_URL`, `KAFKA_BROKERS`, `PSSP_API_URL` +
`PSSP_API_KEY`, `REG_WATCH_URL`. The real PSSP adapter registers as provider
`pssp`; simulators stay available for side-by-side testing. Startup logs
`profile=prod component=...` per component and never fails on a missing
prod var.

## Tests

`go test ./services/presumptive/` — bands, payments saga, certificates,
floats, gates.
