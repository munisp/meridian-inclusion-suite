# kyc-engine (SPEC A) — KYC/KYB document verification service

FastAPI, Python 3.12, CPU-only. Covers doc parsing, OCR field extraction,
VLM+classical forensics, face match, liveness, KYB/UBO, risk decisioning and
hash-chained audit evidence. Anything not backed by a real integration is
tagged `sim: true` in checks and API responses, and the decision engine
refuses auto-approve when a required check is sim
(`ALLOW_SIM_APPROVE=false` default).

## API (all under /v1, Keycloak roles kyc.agent|kyc.reviewer|kyc.admin)

| Endpoint | Purpose |
|---|---|
| `POST /v1/cases` | create case `{subject_type, channel}` -> `{case_id, upload_urls[]}` |
| `PUT /v1/cases/{id}/documents` | multipart upload, 202; idempotent on case+sha256 |
| `POST /v1/cases/{id}/process` | run pipeline (in-proc; Temporal when `TEMPORAL_URL` set) |
| `GET /v1/cases/{id}` | status, per-check results, decision, reasons[] |
| `POST /v1/cases/{id}/liveness/session` | `{ws_url, challenge[]}` |
| `WS /liveness/{session_id}` | challenge-response frames in, verdicts out |
| `POST /v1/cases/{id}/review` | reviewer approve/reject for step_up |
| `GET /v1/cases/{id}/evidence` | ordered hash-chained evidence |
| `GET /healthz /readyz /metrics` | ops (stage latency, decision mix, OCR conf histogram) |

## Pipeline (KycCaseWorkflow, 120s activity timeout, 3x retry)

ingest -> docling parse -> PaddleOCR (PP-OCRv4, ch+en, mkldnn) -> per-doctype
field extraction (nin_slip, cac_cert, passport, drivers_license) -> forensics
(Ollama qwen2-vl:7b VLM + ELA z-score>2.5 / noise variance / EXIF) -> face
match (insightface buffalo_l, cosine) -> liveness (MiniFASNetv2 passive +
blink/yaw challenges) -> KYB (CAC parse -> directors/UBOs >25% -> registry
cross-check) -> weighted decision 0-100 (>=70 approve, 40-69 step_up, <40
reject; any hard-fail forces reject).

## Failure modes (SPEC A §6)

- Ollama down -> classical-only forensics, check `degraded=true`, decision
  capped at step_up.
- Low-light selfie -> face cosine penalized -> step_up band.
- Unknown doctype -> generic extraction, never auto-approve.
- >20MB scans -> downscaled to 2000px long edge for CPU stages (original kept
  in WORM bucket `kyc-raw`).
- Stage retries exhausted -> DLQ event `kyc.dlq.v1` via the audit outbox.
- Evidence events drain from the outbox table to Kafka topic
  `kyc.evidence.v1`; in dev (no `KAFKA_BOOTSTRAP`) the drain is in-proc.

## REAL vs [SIM]

REAL (no heavy dep needed): ELA/noise/EXIF forensics (numpy), field
extractors (regex + layout anchors, NIN/RC/MRZ validators), decision engine,
evidence hash chain + outbox, liveness challenge state machine, KYB UBO
extraction + CAC text parsing, WORM FS storage, idempotent ingest.

REAL behind import-guards (active in the Docker image): docling parse,
PaddleOCR, insightface buffalo_l, MiniFASNetv2 (onnxruntime), Ollama VLM,
Temporal worker, MinIO (boto3), Kafka relay, Postgres. When a dependency is
absent the stage runs a deterministic fallback honestly tagged `sim: true`.

[SIM] adapters (interface mirrors the real API): CAC registry
(`CAC_REGISTRY_URL` unset), NIMC NIN verify (`NIN_VERIFY_URL` unset).

## Integrations

- tin-graph (`TIN_GRAPH_URL`): on business approval the service calls
  `POST /v1/tin/provision` `{cac_rc, legal_name}` (fail-closed: tin-graph
  down -> case returns to step_up, never silently approved).
- Keycloak RS256 (`AUTH_MODE=keycloak`, `KEYCLOAK_ISSUER/_JWKS_URL/_AUDIENCE`)
  — fail-closed; `AUTH_MODE=dev` uses `X-Dev-Role` (suite dev pattern).
- Temporal (`TEMPORAL_URL`): durable `KycCaseWorkflow`; otherwise the in-proc
  runner applies identical retry/timeout semantics.

## Dev quickstart (light deps only)

```bash
cd services/kyc-engine
pip install -r requirements-test.txt
python -m pytest tests/ -q          # 48 tests
python tests/make_fixtures.py       # regenerate synthetic fixtures
uvicorn kyc_engine.main:app --port 8105   # AUTH_MODE=dev, SQLite, FS storage
```

## Docker

```bash
docker build -t meridian/kyc-engine .
docker run -p 8105:8105 -e AUTH_MODE=dev meridian/kyc-engine
```
