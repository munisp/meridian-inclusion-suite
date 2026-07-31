"""Pydantic API schemas (SPEC A §2)."""
from __future__ import annotations

from datetime import datetime
from typing import Any, Literal, Optional

from pydantic import BaseModel, Field


class CreateCaseRequest(BaseModel):
    subject_type: Literal["individual", "business"]
    channel: str = "api"
    subject_ref: Optional[str] = None
    declared_value: Optional[float] = None  # feeds the HIGH_VALUE EDD trigger


class CreateCaseResponse(BaseModel):
    case_id: str
    upload_urls: list[str] = Field(default_factory=list)
    status: str = "created"


class CheckOut(BaseModel):
    kind: str
    score: float
    passed: bool
    sim: bool
    degraded: bool = False
    detail: dict[str, Any] = Field(default_factory=dict)


class CaseOut(BaseModel):
    case_id: str
    subject_type: str
    status: str
    decision: Optional[str] = None
    risk_score: Optional[int] = None
    reasons: list[str] = Field(default_factory=list)
    checks: list[CheckOut] = Field(default_factory=list)
    created_at: Optional[datetime] = None


class LivenessSessionOut(BaseModel):
    session_id: str
    ws_url: str
    challenge: list[str]


class ReviewRequest(BaseModel):
    action: Literal["approve", "reject"]
    note: str = ""


class EvidenceLink(BaseModel):
    seq: int
    event_type: str
    hash: str
    prev_hash: str
    timestamp: datetime
    payload: dict[str, Any] = Field(default_factory=dict)


class EvidenceChainOut(BaseModel):
    case_id: str
    chain_valid: bool
    links: list[EvidenceLink]
