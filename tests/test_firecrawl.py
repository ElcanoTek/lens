# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Tests for the Firecrawl scrape mode and its auto-mode retry pass."""

import os

import pytest

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

from conftest import make_orchestrator

from scraper_client import ScraperClient, _browser_headers

_make_orchestrator = make_orchestrator


class FakeFirecrawlScraper:
    """Stand-in scraper for retry-pass orchestration tests."""

    def __init__(self, expected_pool=None):
        self.expected_pool = expected_pool

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return None

    async def create_session_pool(self, num_sessions):
        if self.expected_pool is not None:
            assert num_sessions == self.expected_pool


# ---------------------------------------------------------------------------
# ScraperClient: mode plumbing
# ---------------------------------------------------------------------------


def test_firecrawl_mode_reported():
    client = ScraperClient(mode="firecrawl")
    assert client.get_mode() == "firecrawl"


def test_orchestrator_accepts_firecrawl_mode(monkeypatch, tmp_path):
    orchestrator = _make_orchestrator(
        monkeypatch, tmp_path, "pageURL\nexample.com\n", scrape_mode="firecrawl"
    )
    assert orchestrator.auto_mode is False
    assert orchestrator.scrape_mode == "firecrawl"


async def test_firecrawl_unreachable_service_raises_helpful_error():
    # Port 9 (discard) is never serving HTTP locally.
    client = ScraperClient(
        mode="firecrawl", firecrawl_url="http://127.0.0.1:9", firecrawl_timeout=1
    )
    with pytest.raises(RuntimeError, match="not reachable"):
        async with client:
            pass


# ---------------------------------------------------------------------------
# ScraperClient: document parsing
# ---------------------------------------------------------------------------


def _document(markdown="# Hello\n\nWorld", **metadata):
    meta = {
        "title": "Hello",
        "description": "A greeting",
        "statusCode": 200,
        "url": "https://example.com",
        "sourceURL": "https://example.com",
    }
    meta.update(metadata)
    return {"markdown": markdown, "metadata": meta}


def test_parse_firecrawl_document_extracts_fields():
    client = ScraperClient(mode="firecrawl")
    parsed = client._parse_firecrawl_document("https://example.com", _document())
    assert parsed["title"] == "Hello"
    assert parsed["meta_description"] == "A greeting"
    assert parsed["content"] == "# Hello\n\nWorld"
    assert parsed["status_code"] == 200


def test_parse_firecrawl_document_truncates_long_markdown():
    client = ScraperClient(mode="firecrawl")
    parsed = client._parse_firecrawl_document("https://example.com", _document(markdown="x" * 5000))
    assert len(parsed["content"]) == 4003  # 4000 chars + "..."
    assert parsed["content"].endswith("...")


def test_parse_firecrawl_document_defaults_missing_metadata():
    client = ScraperClient(mode="firecrawl")
    parsed = client._parse_firecrawl_document("https://example.com", {"markdown": "content"})
    assert parsed["title"] == ""
    assert parsed["meta_description"] == ""
    assert parsed["status_code"] == 200


def test_parse_firecrawl_document_rejects_cross_host_redirect():
    client = ScraperClient(mode="firecrawl", reject_redirects=True)
    with pytest.raises(RuntimeError, match="redirected to different host"):
        client._parse_firecrawl_document(
            "https://example.com", _document(url="https://parked-lander.com/x")
        )


def test_parse_firecrawl_document_allows_www_redirect():
    client = ScraperClient(mode="firecrawl", reject_redirects=True)
    parsed = client._parse_firecrawl_document(
        "https://example.com", _document(url="https://www.example.com/")
    )
    assert parsed["content"]


def test_parse_firecrawl_document_allows_redirects_when_disabled():
    client = ScraperClient(mode="firecrawl", reject_redirects=False)
    parsed = client._parse_firecrawl_document(
        "https://example.com", _document(url="https://parked-lander.com/x")
    )
    assert parsed["content"]


# ---------------------------------------------------------------------------
# ScraperClient: /v2/scrape calls (stubbed HTTP session)
# ---------------------------------------------------------------------------


class _FakeResponse:
    def __init__(self, status, body):
        self.status = status
        self._body = body

    async def json(self, content_type=None):
        return self._body

    async def text(self):
        if isinstance(self._body, str):
            return self._body
        import json as _json

        return _json.dumps(self._body)

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return None


class _FakeSession:
    """Stub aiohttp session returning queued responses."""

    def __init__(self, responses):
        self.responses = list(responses)
        self.requests = []

    def post(self, url, json=None):
        self.requests.append((url, json))
        return self.responses.pop(0)

    def get(self, url, timeout=None):
        self.requests.append((url, None))
        return self.responses.pop(0)


def _firecrawl_client(**kwargs):
    client = ScraperClient(mode="firecrawl", max_retries=3, retry_delay=0.01, **kwargs)
    return client


async def test_scrape_with_firecrawl_success():
    client = _firecrawl_client()
    client._web_session = _FakeSession([_FakeResponse(200, {"success": True, "data": _document()})])
    parsed = await client._scrape_with_firecrawl("https://example.com")
    assert parsed["title"] == "Hello"

    url, payload = client._web_session.requests[0]
    assert url.endswith("/v2/scrape")
    assert payload["url"] == "https://example.com"
    assert payload["formats"] == ["markdown"]
    assert payload["onlyMainContent"] is True
    # "basic" proxy tier => one render attempt, no stealth-proxy escalation.
    assert payload["proxy"] == "basic"


async def test_scrape_with_firecrawl_antibot_is_not_retried():
    # A 5xx carrying the service's anti-bot verdict is deterministic: retrying
    # just re-hammers a site that is blocking us, so it must fail on the first
    # response, not exhaust the retry budget.
    client = _firecrawl_client()
    client._web_session = _FakeSession(
        [
            _FakeResponse(
                500,
                {
                    "success": False,
                    "code": "SCRAPE_RETRY_LIMIT",
                    "error": "Scrape aborted after exceeding retry limit (document_antibot).",
                },
            )
        ]
        * 3
    )
    with pytest.raises(RuntimeError, match="anti-bot"):
        await client._scrape_with_firecrawl("https://example.com")
    assert len(client._web_session.requests) == 1


async def test_scrape_with_firecrawl_failure_is_not_retried():
    client = _firecrawl_client()
    client._web_session = _FakeSession(
        [_FakeResponse(200, {"success": False, "error": "site blocked"})]
    )
    with pytest.raises(RuntimeError, match="site blocked"):
        await client._scrape_with_firecrawl("https://example.com")
    # The service already retried internally; exactly one request must go out.
    assert len(client._web_session.requests) == 1


async def test_scrape_with_firecrawl_retries_server_errors():
    client = _firecrawl_client()
    client._web_session = _FakeSession(
        [
            _FakeResponse(500, "<html>Internal Server Error</html>"),
            _FakeResponse(502, {"success": False, "error": "bad gateway"}),
            _FakeResponse(200, {"success": True, "data": _document()}),
        ]
    )
    parsed = await client._scrape_with_firecrawl("https://example.com")
    assert parsed["title"] == "Hello"
    assert len(client._web_session.requests) == 3


async def test_scrape_with_firecrawl_retries_rate_limits():
    client = _firecrawl_client()
    client._web_session = _FakeSession(
        [
            _FakeResponse(429, {"error": "rate limited"}),
            _FakeResponse(200, {"success": True, "data": _document()}),
        ]
    )
    parsed = await client._scrape_with_firecrawl("https://example.com")
    assert parsed["title"] == "Hello"
    assert len(client._web_session.requests) == 2


async def test_scrape_with_firecrawl_exhausted_retries_raise():
    client = _firecrawl_client()
    client._web_session = _FakeSession([_FakeResponse(500, {"error": "boom"})] * 3)
    with pytest.raises(RuntimeError, match="Failed to scrape"):
        await client._scrape_with_firecrawl("https://example.com")


async def test_service_ready_requires_firecrawl_identity():
    from scraper_client import firecrawl_service_ready

    # A different service squatting on the port must not count as available.
    squatter = _FakeSession([_FakeResponse(200, "<html>Some other app</html>")])
    assert await firecrawl_service_ready("http://x", session=squatter) is False

    real = _FakeSession(
        [_FakeResponse(200, {"message": "Firecrawl API", "documentation_url": "x"})]
    )
    assert await firecrawl_service_ready("http://x", session=real) is True


async def test_scrape_site_dispatches_to_firecrawl():
    client = _firecrawl_client()
    client._web_session = _FakeSession([_FakeResponse(200, {"success": True, "data": _document()})])
    await client.create_session_pool(1)
    session = await client.get_available_session()
    result = await client.scrape_site(session, "example.com")
    client.release_session(session)

    assert result["success"] is True
    assert result["mode"] == "firecrawl"
    assert result["title"] == "Hello"
    # Scheme-less input is normalised before hitting the service.
    assert client._web_session.requests[0][1]["url"] == "https://example.com"


async def test_scrape_with_firecrawl_sends_optional_knobs():
    client = _firecrawl_client(firecrawl_wait_for=1500, firecrawl_max_age_ms=0)
    client._web_session = _FakeSession([_FakeResponse(200, {"success": True, "data": _document()})])
    await client._scrape_with_firecrawl("https://example.com")
    payload = client._web_session.requests[0][1]
    assert payload["waitFor"] == 1500
    assert payload["maxAge"] == 0


# ---------------------------------------------------------------------------
# Direct-mode hardening
# ---------------------------------------------------------------------------


def test_browser_headers_look_like_a_browser():
    headers = _browser_headers("test-agent/1.0")
    assert headers["User-Agent"] == "test-agent/1.0"
    assert "text/html" in headers["Accept"]
    assert headers["Accept-Language"].startswith("en-US")
    assert headers["Sec-Fetch-Mode"] == "navigate"


def test_www_fallback_url_for_bare_domain():
    assert ScraperClient._www_fallback_url("https://example.com") == "https://www.example.com"
    assert (
        ScraperClient._www_fallback_url("https://example.com/path?q=1")
        == "https://www.example.com/path?q=1"
    )


def test_www_fallback_url_skips_non_bare_hosts():
    assert ScraperClient._www_fallback_url("https://www.example.com") is None
    assert ScraperClient._www_fallback_url("https://sub.example.com") is None
    assert ScraperClient._www_fallback_url("https://example.com:8080") is None
    assert ScraperClient._www_fallback_url("https://example.co.uk") is None


# ---------------------------------------------------------------------------
# Orchestration: auto-mode Firecrawl retry pass
# ---------------------------------------------------------------------------


async def test_firecrawl_available_respects_kill_switch(monkeypatch, tmp_path):
    from config import config

    orchestrator = _make_orchestrator(monkeypatch, tmp_path, "pageURL\nexample.com\n")
    monkeypatch.setattr(config, "FIRECRAWL_ENABLED", False, raising=False)
    assert await orchestrator._firecrawl_available() is False


async def test_firecrawl_available_caches_probe(monkeypatch, tmp_path):
    orchestrator = _make_orchestrator(monkeypatch, tmp_path, "pageURL\nexample.com\n")
    orchestrator._firecrawl_service_available = True
    assert await orchestrator._firecrawl_available() is True


async def test_firecrawl_retry_noop_without_failures(monkeypatch, tmp_path):
    from contextlib import AsyncExitStack

    orchestrator = _make_orchestrator(monkeypatch, tmp_path, "pageURL\nexample.com\n")
    items = orchestrator._load_input_data()

    async def _boom(*args, **kwargs):
        raise AssertionError("availability probe should not run")

    monkeypatch.setattr(orchestrator, "_firecrawl_available", _boom)

    async with AsyncExitStack() as stack:
        await orchestrator._retry_failed_websites_firecrawl(stack, items)


async def test_firecrawl_retry_skipped_when_service_down(monkeypatch, tmp_path):
    from contextlib import AsyncExitStack

    orchestrator = _make_orchestrator(monkeypatch, tmp_path, "pageURL\nexample.com\n")
    items = orchestrator._load_input_data()
    await orchestrator.progress_tracker.mark_domain_processed(
        "example.com", status="error", error_message="HTTP 403"
    )

    async def _unavailable():
        return False

    monkeypatch.setattr(orchestrator, "_firecrawl_available", _unavailable)

    processed = []

    async def fake_process(retry_items):
        processed.extend(retry_items)

    monkeypatch.setattr(orchestrator, "_process_items_concurrently", fake_process)

    async with AsyncExitStack() as stack:
        await orchestrator._retry_failed_websites_firecrawl(stack, items)

    assert processed == []


async def test_firecrawl_retry_targets_only_failed_websites(monkeypatch, tmp_path):
    from contextlib import AsyncExitStack

    mixed = "pageURL\nexample.com\ngithub.com\n333903271\n"
    orchestrator = _make_orchestrator(monkeypatch, tmp_path, mixed)
    items = orchestrator._load_input_data()

    await orchestrator.progress_tracker.mark_domain_processed(
        "example.com", status="retry_pending", error_message="HTTP 403"
    )
    await orchestrator.progress_tracker.mark_domain_processed("github.com", status="success")
    await orchestrator.progress_tracker.mark_domain_processed(
        "333903271", status="error", error_message="rate limited"
    )

    async def _available():
        return True

    monkeypatch.setattr(orchestrator, "_firecrawl_available", _available)
    monkeypatch.setattr(orchestrator, "_deep_crawl_available", lambda: True)

    from config import config

    expected_pool = min(config.CONCURRENT_SESSIONS, config.FIRECRAWL_MAX_CONCURRENT)

    import orchestration

    monkeypatch.setattr(
        orchestration.SiteAnalysisOrchestrator,
        "_build_scraper_client",
        lambda self, mode, **kwargs: FakeFirecrawlScraper(expected_pool),
    )

    processed = []

    async def fake_process(retry_items):
        processed.extend(retry_items)

    monkeypatch.setattr(orchestrator, "_process_items_concurrently", fake_process)

    async with AsyncExitStack() as stack:
        await orchestrator._retry_failed_websites_firecrawl(stack, items)

    assert [item.identifier for item in processed] == ["example.com"]
    assert orchestrator.scrape_mode == "firecrawl"
    # Deep crawl is still available, so Firecrawl failures stay retryable.
    assert orchestrator._defer_website_failures is True


async def test_firecrawl_retry_failures_terminal_without_deep(monkeypatch, tmp_path):
    from contextlib import AsyncExitStack

    orchestrator = _make_orchestrator(monkeypatch, tmp_path, "pageURL\nexample.com\n")
    items = orchestrator._load_input_data()
    await orchestrator.progress_tracker.mark_domain_processed(
        "example.com", status="retry_pending", error_message="HTTP 403"
    )

    async def _available():
        return True

    monkeypatch.setattr(orchestrator, "_firecrawl_available", _available)
    monkeypatch.setattr(orchestrator, "_deep_crawl_available", lambda: False)
    monkeypatch.setattr(orchestrator, "_research_fallback_available", lambda: False)

    monkeypatch.setattr(
        type(orchestrator),
        "_build_scraper_client",
        lambda self, mode, **kwargs: FakeFirecrawlScraper(),
    )

    async def fake_process(retry_items):
        pass

    monkeypatch.setattr(orchestrator, "_process_items_concurrently", fake_process)

    async with AsyncExitStack() as stack:
        await orchestrator._retry_failed_websites_firecrawl(stack, items)

    assert orchestrator._defer_website_failures is False


async def test_deep_retry_skips_antibot_blocked_sites(monkeypatch, tmp_path):
    from contextlib import AsyncExitStack

    mixed = "pageURL\nblocked.com\nrenderfail.com\n"
    orchestrator = _make_orchestrator(monkeypatch, tmp_path, mixed)
    items = orchestrator._load_input_data()

    # blocked.com hit an anti-bot wall (Firecrawl proved this IP is blocked);
    # renderfail.com failed for a non-block reason the deep pass might fix.
    await orchestrator.progress_tracker.mark_domain_processed(
        "blocked.com", status="error", error_message="Received HTTP status 403"
    )
    await orchestrator.progress_tracker.mark_domain_processed(
        "renderfail.com", status="error", error_message="Scraper returned empty content"
    )

    monkeypatch.setattr(orchestrator, "_deep_crawl_available", lambda: True)
    monkeypatch.setattr(
        type(orchestrator),
        "_build_scraper_client",
        lambda self, mode, **kwargs: FakeFirecrawlScraper(),
    )

    processed = []

    async def fake_process(retry_items):
        processed.extend(retry_items)

    monkeypatch.setattr(orchestrator, "_process_items_concurrently", fake_process)

    async with AsyncExitStack() as stack:
        await orchestrator._retry_failed_websites_deep(stack, items)

    # The anti-bot-blocked site is skipped; the other still gets a deep attempt.
    assert [item.identifier for item in processed] == ["renderfail.com"]
