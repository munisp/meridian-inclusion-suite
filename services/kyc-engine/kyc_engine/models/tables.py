"""SQLAlchemy models (SPEC A §3).

Prod: Postgres schema `kyc`, partitioned by month on created_at (migration
concern); dev/tests: SQLite with the same logical shape. JSON columns map to
JSONB on Postgres and JSON text on SQLite.
"""
from __future__ import annotations

import uuid
from datetime import datetime, timezone
from decimal import Decimal

from sqlalchemy import JSON, Boolean, DateTime, Float, ForeignKey, Index, Integer, Numeric, String, Text
from sqlalchemy.orm import Mapped, mapped_column, relationship

from .db import Base


def _uuid() -> str:
    return str(uuid.uuid4())


def _now() -> datetime:
    return datetime.now(timezone.utc)


class KycCase(Base):
    __tablename__ = "kyc_case"
    id: Mapped[str] = mapped_column(String(36), primary_key=True, default=_uuid)
    subject_type: Mapped[str] = mapped_column(String(16))  # individual|business
    channel: Mapped[str] = mapped_column(String(32), default="api")
    subject_ref: Mapped[str | None] = mapped_column(String(128), nullable=True)
    # owning agent (token sub / X-Dev-Subject) — object-level authz anchor
    agent_ref: Mapped[str | None] = mapped_column(String(128), nullable=True)
    status: Mapped[str] = mapped_column(String(32), default="created")
    # created|documents_received|processing|liveness_pending|step_up|
    # approved|rejected|failed
    risk_score: Mapped[int | None] = mapped_column(Integer, nullable=True)
    # optional declared transaction/relationship value feeding the HIGH_VALUE
    # EDD trigger (edd_high_value_threshold); None = not declared.
    # Numeric, not Float: money as binary float loses kobo precision (audit P1).
    declared_value: Mapped[Decimal | None] = mapped_column(Numeric(20, 2), nullable=True)
    decision: Mapped[str | None] = mapped_column(String(16), nullable=True)
    reason_codes: Mapped[list] = mapped_column(JSON, default=list)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=_now)

    documents: Mapped[list["KycDocument"]] = relationship(back_populates="case")
    checks: Mapped[list["KycCheck"]] = relationship(back_populates="case")

    __table_args__ = (
        # work-queue query: cases by status ordered by creation
        Index("ix_kyc_case_status_created", "status", "created_at"),
    )


class KycDocument(Base):
    __tablename__ = "kyc_document"
    id: Mapped[str] = mapped_column(String(36), primary_key=True, default=_uuid)
    case_id: Mapped[str] = mapped_column(ForeignKey("kyc_case.id"), index=True)
    doc_type: Mapped[str] = mapped_column(String(32), default="unknown")
    sha256: Mapped[str] = mapped_column(String(64), index=True)
    minio_key: Mapped[str] = mapped_column(String(256))
    mime: Mapped[str] = mapped_column(String(64))
    pages: Mapped[int] = mapped_column(Integer, default=1)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=_now)

    case: Mapped[KycCase] = relationship(back_populates="documents")
    extraction: Mapped["KycExtraction | None"] = relationship(back_populates="document", uselist=False)


class KycExtraction(Base):
    __tablename__ = "kyc_extraction"
    id: Mapped[str] = mapped_column(String(36), primary_key=True, default=_uuid)
    document_id: Mapped[str] = mapped_column(ForeignKey("kyc_document.id"), index=True)
    # fields holds HMAC-pseudonymised + masked values only (see adapters/pii).
    fields: Mapped[dict] = mapped_column(JSON, default=dict)
    # RESTRICTED (tokenisation vault): raw PII for reversible lookup by
    # legitimate processing only (e.g. periodic re-screening). Never
    # serialised by the API, never logged.
    pii_vault: Mapped[dict] = mapped_column(JSON, default=dict)
    ocr_conf_avg: Mapped[float] = mapped_column(Float, default=0.0)
    extractor_version: Mapped[str] = mapped_column(String(32), default="1.0.0")

    document: Mapped[KycDocument] = relationship(back_populates="extraction")


class KycCheck(Base):
    __tablename__ = "kyc_check"
    id: Mapped[str] = mapped_column(String(36), primary_key=True, default=_uuid)
    case_id: Mapped[str] = mapped_column(ForeignKey("kyc_case.id"), index=True)
    kind: Mapped[str] = mapped_column(String(24))  # ocr|forensics|face_match|liveness|registry|kyb
    score: Mapped[float] = mapped_column(Float, default=0.0)
    passed: Mapped[bool] = mapped_column(Boolean, default=False)
    detail: Mapped[dict] = mapped_column(JSON, default=dict)
    sim: Mapped[bool] = mapped_column(Boolean, default=False)
    degraded: Mapped[bool] = mapped_column(Boolean, default=False)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=_now)

    case: Mapped[KycCase] = relationship(back_populates="checks")


class KycDecision(Base):
    """Hash-chained decision record (SPEC A §3): prev_hash -> hash."""
    __tablename__ = "kyc_decision"
    id: Mapped[str] = mapped_column(String(36), primary_key=True, default=_uuid)
    case_id: Mapped[str] = mapped_column(ForeignKey("kyc_case.id"), index=True)
    verdict: Mapped[str] = mapped_column(String(16))  # approve|step_up|reject
    score: Mapped[int] = mapped_column(Integer)
    reasons: Mapped[list] = mapped_column(JSON, default=list)
    actor: Mapped[str] = mapped_column(String(16), default="system")  # system|user
    prev_hash: Mapped[str] = mapped_column(String(64), default="")
    hash: Mapped[str] = mapped_column(String(64), default="")
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=_now)


class AuditEvent(Base):
    """Outbox table (SPEC A §3): drained to Kafka topic kyc.evidence.v1."""
    __tablename__ = "audit_event"
    id: Mapped[str] = mapped_column(String(36), primary_key=True, default=_uuid)
    case_id: Mapped[str] = mapped_column(String(36), index=True)
    event_type: Mapped[str] = mapped_column(String(64))
    payload: Mapped[dict] = mapped_column(JSON, default=dict)
    prev_hash: Mapped[str] = mapped_column(String(64), default="")
    hash: Mapped[str] = mapped_column(String(64), default="")
    published: Mapped[bool] = mapped_column(Boolean, default=False)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=_now)

    __table_args__ = (
        # outbox drain query: unpublished events in creation order
        Index("ix_kyc_audit_unpub", "published", "created_at"),
    )


class LivenessSession(Base):
    __tablename__ = "liveness_session"
    id: Mapped[str] = mapped_column(String(36), primary_key=True, default=_uuid)
    case_id: Mapped[str] = mapped_column(ForeignKey("kyc_case.id"), index=True)
    challenges: Mapped[list] = mapped_column(JSON, default=list)
    state: Mapped[str] = mapped_column(String(16), default="open")  # open|passed|failed|expired
    attempts: Mapped[int] = mapped_column(Integer, default=0)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=_now)
