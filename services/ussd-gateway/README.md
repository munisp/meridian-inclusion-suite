# services/ussd-gateway — Go :8104

USSD session engine (JSON menu-graph DSL, 180s sliding-TTL sessions) with
onboarding (`nrs.onb.ussd.v1`) and presumptive-payment (`nrs.psm.ussd.v1`)
trees, an Africa's-Talking-style aggregator webhook and a built-in
full-session simulator. See the suite root README for the API tour.

## Run

```bash
go run ./services/ussd-gateway  # dev profile, zero external deps
```

## Environment

| Var | Purpose | Default (dev) |
|---|---|---|
| `PORT` | listen port | 8104 |
| `AUTH_MODE` / `KEYCLOAK_ISSUER` / `KEYCLOAK_AUDIENCE` / `KEYCLOAK_JWKS_URL` | §1.3 auth; `keycloak` = RS256 JWKS | dev (HS256 + `X-Dev-Role`) |
| `KAFKA_BROKERS` | Redpanda brokers for `nrs.*.ussd.v1` | embedded inproc bus |
| `USSD_AGGREGATOR_KEY` | HMAC-SHA256 key for inbound `POST /webhook/ussd` signature verification (`X-Aggregator-Signature`) and outbound notify signing | unset → unsigned webhooks accepted (dev) |
| `USSD_AGGREGATOR_URL` | outbound notify client `POST {url}/notify` on session end; HMAC-signed, 3-retry backoff, breaker (5 failures → open 30s) | unset → notify dropped (simulator covers sessions) |

## Prod profile

Set `AUTH_MODE=keycloak`, `KAFKA_BROKERS`, `USSD_AGGREGATOR_URL` +
`USSD_AGGREGATOR_KEY`. With a key configured, unsigned or wrongly signed
webhooks are rejected with 401. Raw MSISDNs are never logged — only an
`msisdn_hash` prefix.

## Tests

`go test ./services/ussd-gateway/` — menu engine, sessions, webhook replay
logic, aggregator signature verification and notify client.
