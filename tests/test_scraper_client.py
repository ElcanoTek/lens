# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

import asyncio
import os

import pytest

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

from scraper_client import ScraperClient


async def test_session_pool_acquire_and_release():
    client = ScraperClient(mode="direct")
    await client.create_session_pool(2)

    first = await client.get_available_session()
    second = await client.get_available_session()
    assert first.in_use and second.in_use
    assert first is not second

    client.release_session(first)
    third = await client.get_available_session()
    assert third is first


async def test_read_body_drains_the_full_stream():
    """StreamReader.read(n) returns one buffered chunk, not the body; the
    scraper must keep reading to EOF or real pages parse as near-empty."""

    class FakeContent:
        def __init__(self, chunks):
            self.chunks = list(chunks)

        async def read(self, n):
            return self.chunks.pop(0) if self.chunks else b""

    class FakeResponse:
        content = FakeContent([b"a" * 1000, b"b" * 1000, b"c" * 1000])

    body = await ScraperClient._read_body(FakeResponse())
    assert body == b"a" * 1000 + b"b" * 1000 + b"c" * 1000


async def test_read_body_respects_size_cap(monkeypatch):
    import scraper_client as sc

    monkeypatch.setattr(sc, "MAX_RESPONSE_BYTES", 1500)

    class FakeContent:
        async def read(self, n):
            return b"x" * min(n, 1000)

    class FakeResponse:
        content = FakeContent()

    body = await ScraperClient._read_body(FakeResponse())
    assert len(body) == 1500


async def test_get_available_session_releases_permit_on_failure():
    client = ScraperClient(mode="direct")
    await client.create_session_pool(1)

    # Force the "no free session" failure path while the semaphore still has
    # a permit. The permit must be returned, or the next acquire deadlocks.
    client.sessions.clear()

    with pytest.raises(RuntimeError, match="No available sessions"):
        await client.get_available_session()

    from scraper_client import ScraperSession

    client.sessions = [ScraperSession("local-0")]
    session = await asyncio.wait_for(client.get_available_session(), timeout=1.0)
    assert session.in_use
