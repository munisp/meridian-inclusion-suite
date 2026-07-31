"""kyc-engine configuration (SPEC A §5): thresholds tunable via env."""
from __future__ import annotations

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="", env_file=".env", extra="ignore")

    service_name: str = "kyc-engine"
    version: str = "1.0.0"

    # --- persistence ---
    # sqlite for dev; postgres://... in prod (schema `kyc`, monthly partitions
    # are applied by migrations in prod deployments).
    database_url: str = "sqlite:///./kyc_engine.db"

    # --- object storage (WORM evidence + raw docs) ---
    minio_url: str = ""            # empty -> local FS dev fallback
    minio_access_key: str = ""
    minio_secret_key: str = ""
    minio_bucket_raw: str = "kyc-raw"
    minio_bucket_evidence: str = "kyc-evidence"
    storage_fs_root: str = "./.kyc_store"   # dev fallback root

    # --- events ---
    kafka_bootstrap: str = ""      # empty -> in-proc outbox drain (dev)
    kafka_topic_evidence: str = "kyc.evidence.v1"
    kafka_topic_dlq: str = "kyc.dlq.v1"

    # --- orchestration ---
    # When set, the orchestrator connects to a real Temporal server; otherwise
    # an in-proc runner with identical stage semantics is used.
    temporal_url: str = ""
    temporal_task_queue: str = "kyc-case"
    stage_timeout_seconds: int = 120
    stage_max_retries: int = 3

    # --- PII at rest ---
    pii_hmac_key: str = ""         # REQUIRED in prod (fail-closed); dev uses inert key

    # --- auth (shared pattern: AUTH_MODE=dev|keycloak) ---
    auth_mode: str = "dev"
    keycloak_issuer: str = ""
    keycloak_audience: str = ""
    keycloak_jwks_url: str = ""

    # --- OCR / models ---
    paddle_use_mkldnn: bool = True
    ocr_lang: str = "ch+en"
    max_scan_bytes: int = 20 * 1024 * 1024   # >20MB -> downscale (SPEC §6)
    downscale_long_edge: int = 2000

    # --- VLM forensics ---
    ollama_url: str = "http://localhost:11434"
    ollama_model: str = "qwen2-vl:7b"
    ollama_fallback_model: str = "llava:13b"
    ollama_timeout_seconds: float = 30.0

    # --- thresholds (SPEC A §5) ---
    ocr_conf_step_up: float = 0.75
    forensics_reject: float = 0.7
    forensics_step_up: float = 0.4
    ela_z_flag: float = 2.5
    face_pass: float = 0.45
    face_step_up: float = 0.32
    liveness_pass: float = 0.8
    liveness_challenges_required: int = 2
    liveness_challenge_window_seconds: float = 10.0
    decision_approve: int = 70
    decision_step_up: int = 40
    # UBO/PSC ownership threshold (strict `>`). Default 5%: Nigeria CAMA 2020
    # PSC register norm; FATF R.10 "more than 25%" deployments set 25.0 via env.
    ubo_ownership_pct: float = 5.0
    # SPEC §6: refuse auto-approve when any required check is sim
    allow_sim_approve: bool = False

    # --- integrations ---
    tin_graph_url: str = ""        # empty -> KYB CAC provision disabled (fail-closed)
    cac_registry_url: str = ""     # empty -> [SIM] deterministic fixtures
    nin_verify_url: str = ""       # empty -> [SIM] NIMC adapter


def get_settings() -> Settings:
    return Settings()
