from __future__ import annotations

import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))


@pytest.fixture()
def env(tmp_path, monkeypatch):
    """Isolated SQLite DB + FS storage per test."""
    monkeypatch.setenv("DATABASE_URL", f"sqlite:///{tmp_path}/t.db")
    monkeypatch.setenv("STORAGE_FS_ROOT", str(tmp_path / "store"))
    monkeypatch.setenv("AUTH_MODE", "dev")
    monkeypatch.setenv("ALLOW_SIM_APPROVE", "false")
    from kyc_engine.models import db
    db.init_engine(f"sqlite:///{tmp_path}/t.db")
    db.create_all()
    import kyc_engine.adapters.storage as storage
    storage._storage = storage.FsStorage(str(tmp_path / "store"))
    yield tmp_path
    storage._storage = None
    db._engine = None
    db._SessionLocal = None


@pytest.fixture()
def client(env):
    from fastapi.testclient import TestClient
    from kyc_engine.main import app
    with TestClient(app) as c:
        yield c
