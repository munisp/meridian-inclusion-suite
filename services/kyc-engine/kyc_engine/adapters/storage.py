"""Object storage adapter: MinIO (WORM buckets) with local FS dev fallback.

WORM semantics: put is write-once — overwriting an existing key raises.
"""
from __future__ import annotations

import hashlib
import os
from pathlib import Path

from ..config import get_settings


class StorageError(RuntimeError):
    pass


class ObjectExists(StorageError):
    pass


class FsStorage:
    """Dev fallback: content-addressed files under storage_fs_root/<bucket>/."""

    def __init__(self, root: str | None = None):
        self.root = Path(root or get_settings().storage_fs_root)

    def _path(self, bucket: str, key: str) -> Path:
        return self.root / bucket / key

    def put(self, bucket: str, key: str, data: bytes) -> str:
        p = self._path(bucket, key)
        if p.exists():
            raise ObjectExists(f"WORM violation: {bucket}/{key} exists")
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(data)
        return key

    def get(self, bucket: str, key: str) -> bytes:
        return self._path(bucket, key).read_bytes()

    def exists(self, bucket: str, key: str) -> bool:
        return self._path(bucket, key).exists()


class MinioStorage:
    """Prod: MinIO/S3 with object-lock (WORM) buckets."""

    def __init__(self):
        s = get_settings()
        try:
            import boto3  # type: ignore
        except ImportError as e:
            raise StorageError("boto3 required for MinIO storage") from e
        self.bucket_raw = s.minio_bucket_raw
        self._s3 = boto3.client(
            "s3",
            endpoint_url=s.minio_url,
            aws_access_key_id=s.minio_access_key,
            aws_secret_access_key=s.minio_secret_key,
        )

    def put(self, bucket: str, key: str, data: bytes) -> str:
        try:
            self._s3.head_object(Bucket=bucket, Key=key)
            raise ObjectExists(f"WORM violation: {bucket}/{key} exists")
        except self._s3.exceptions.ClientError:
            pass
        self._s3.put_object(Bucket=bucket, Key=key, Body=data)
        return key

    def get(self, bucket: str, key: str) -> bytes:
        return self._s3.get_object(Bucket=bucket, Key=key)["Body"].read()

    def exists(self, bucket: str, key: str) -> bool:
        try:
            self._s3.head_object(Bucket=bucket, Key=key)
            return True
        except self._s3.exceptions.ClientError:
            return False


_storage = None


def get_storage(fs_root: str | None = None):
    """MinIO when minio_url set, else FS dev fallback."""
    global _storage
    if fs_root is not None:
        return FsStorage(fs_root)
    if _storage is None:
        s = get_settings()
        _storage = MinioStorage() if s.minio_url else FsStorage()
    return _storage


def sha256_hex(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()
