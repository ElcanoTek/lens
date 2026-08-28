# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

import json
import os
import sys
from pathlib import Path
from typing import Any

import pytest

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

PROJECT_ROOT = Path(__file__).resolve().parents[1]
if str(PROJECT_ROOT) not in sys.path:
    sys.path.insert(0, str(PROJECT_ROOT))

from openrouter_client import OpenRouterClient
import scraper_client
from scraper_client import ScraperClient
from main import SiteAnalysisOrchestrator
from config import config


@pytest.mark.asyncio
async def test_scraper_session_pool():
    async with ScraperClient(request_timeout=5, max_retries=1, retry_delay=0.1) as client:
        await client.create_session_pool(2)
        session_one = await client.get_available_session()
        session_two = await client.get_available_session()

        assert session_one.id == "local-0"
        assert session_two.id == "local-1"

        client.release_session(session_one)
        client.release_session(session_two)

        stats = client.get_session_stats()
        assert stats["total_sessions"] == 2
        assert stats["in_use"] == 0


@pytest.mark.asyncio
async def test_scraper_fail_fast_failure_recorded():
    class ImmediateFailScraper(ScraperClient):
        async def _fetch_html_with_retries(self, url: str, timeout: int) -> Any:  # type: ignore[override]
            raise RuntimeError("boom")

    async with ImmediateFailScraper(request_timeout=5) as client:
        await client.create_session_pool(1)
        session = await client.get_available_session()

        try:
            result = await client.scrape_site(session, "example.invalid")
        finally:
            client.release_session(session)

        assert result["success"] is False
        assert result["error"] == "boom"


@pytest.mark.asyncio
async def test_openrouter_live_connection():
    api_key = os.getenv("OPENROUTER_API_KEY")

    if not api_key or api_key == "test-key":
        pytest.skip("OpenRouter API key is not configured in the environment")

    async with OpenRouterClient(
        api_key=api_key,
        model="google/gemini-2.5-flash-lite",
        max_tokens=16,
        temperature=0.0,
    ) as client:
        if not await client.test_connection():
            pytest.skip("OpenRouter API is unreachable in this environment")
        assert client.get_mode() == "openrouter"


from domain_processing import DomainProcessor
from progress_tracker import ProgressTracker
from reporting import TerminalReporter


def test_orchestrator_rejects_short_scrape_content():
    processor = DomainProcessor(
        progress_tracker=ProgressTracker("progress.json"),
        scraper_client=ScraperClient(),
        openrouter_client=OpenRouterClient(api_key="test-key"),
        reporter=TerminalReporter(
            total=1,
            completed=0,
            successes=0,
            failures=0,
            quiet=True,
            verbose=False,
            jsonl_path=None,
        ),
        results_writer=None,
        results_file=None,
    )
    short_length = config.MIN_CONTENT_LENGTH - 1
    scrape_result = {
        "success": True,
        "status_code": 200,
        "content": "a" * short_length,
        "content_length": short_length,
    }

    validation_error = processor._validate_scrape_result(scrape_result)

    assert validation_error is not None
    assert "too short" in validation_error.lower()


def test_chrome_container_wait_retries_connection_reset(monkeypatch):
    client = ScraperClient(chrome_startup_timeout=5)
    client._chrome_endpoint = "http://localhost:4444"

    response_payload = json.dumps({"value": {"ready": True}}).encode("utf-8")

    class DummyResponse:
        status = 200

        def __enter__(self):
            return self

        def __exit__(self, exc_type, exc, tb):
            return False

        def read(self):
            return response_payload

    attempts = {"count": 0}

    def fake_urlopen(url, timeout=2):
        assert url.endswith("/wd/hub/status")
        attempts["count"] += 1
        if attempts["count"] < 3:
            raise ConnectionResetError("Connection reset by peer")
        return DummyResponse()

    def fake_time():
        base = 1000
        return base + attempts["count"] * 0.1

    monkeypatch.setattr(scraper_client.urllib.request, "urlopen", fake_urlopen)
    monkeypatch.setattr(scraper_client.time, "sleep", lambda _: None)
    monkeypatch.setattr(scraper_client.time, "time", fake_time)

    client._wait_for_container_ready()

    assert attempts["count"] == 3


@pytest.mark.asyncio
async def test_deep_scrape_recovers_after_invalid_session(monkeypatch):
    monkeypatch.setattr(scraper_client, "SELENIUM_AVAILABLE", True)

    class DummyDriver:
        def __init__(self, owner):
            self.owner = owner
            self.page_source = "<html><title>Recovered</title><body>ok</body></html>"
            self.current_url = ""

        def set_page_load_timeout(self, timeout):
            self.timeout = timeout

        def get(self, url):
            if self.owner.fail_next_navigation:
                self.owner.fail_next_navigation = False
                raise scraper_client.WebDriverException(
                    "Cannot find session with id: broken"
                )
            self.visited_url = url
            self.current_url = url

        def quit(self):
            self.owner.quit_calls += 1

    class RecoveringScraper(ScraperClient):
        def __init__(self):
            super().__init__(
                mode="deep",
                request_timeout=5,
                chrome_wait_after_load=0,
                keep_container=True,
            )
            self.container_running = False
            self.restart_attempts = 0
            self.fail_next_navigation = True
            self.quit_calls = 0

        def _ensure_chrome_container(self):
            self.container_running = True
            self._container_started = True

        def _stop_chrome_container(self):
            self.restart_attempts += 1
            self.container_running = False
            self._container_started = False

        def _is_container_running(self):
            return self.container_running

        def _wait_for_container_ready(self):
            return None

        def _create_webdriver(self):
            return DummyDriver(self)

    async with RecoveringScraper() as client:
        await client.create_session_pool(1)
        session = await client.get_available_session()

        try:
            result = await client.scrape_site(session, "example.com")
        finally:
            client.release_session(session)

    assert result["success"] is True
    assert result["title"] == "Recovered"
    assert client.restart_attempts == 1
    assert client.quit_calls >= 1


def _tool_call_response(function_name: str) -> dict:
    arguments = {
        "quality": "Premium",
        "justification": "test",
        "vertical": "Games",
        "description": "test",
        "confidence": "High",
        "language": "English",
        "political_leaning": "Non-Political",
        "audience_size": "M",
    }
    return {
        "choices": [
            {
                "message": {
                    "tool_calls": [
                        {
                            "function": {
                                "name": function_name,
                                "arguments": json.dumps(arguments),
                            }
                        }
                    ]
                }
            }
        ],
        "usage": {"total_tokens": 10},
    }


def test_parse_classification_accepts_matching_tool_name(caplog):
    client = OpenRouterClient(api_key="test-key")
    with caplog.at_level("WARNING", logger="openrouter_client"):
        result = client._parse_classification_response(
            _tool_call_response("classify_app"), expected_name="classify_app"
        )
    assert result["quality"] == "Premium"
    assert "did not match expected" not in caplog.text


def test_parse_classification_warns_on_mismatched_tool_name(caplog):
    client = OpenRouterClient(api_key="test-key")
    with caplog.at_level("WARNING", logger="openrouter_client"):
        result = client._parse_classification_response(
            _tool_call_response("classify_app")
        )
    assert result["quality"] == "Premium"
    assert "did not match expected" in caplog.text


def test_parse_classification_raises_on_unparseable_message():
    """No tool_calls and no inline JSON must raise, never return a silent
    Standard default (the old behavior wrote fake 'Failed to parse response'
    rows into the output CSV)."""
    client = OpenRouterClient(api_key="test-key")
    truncated = {
        "choices": [
            {
                "finish_reason": "length",
                "message": {"content": "Quality", "tool_calls": None},
            }
        ],
        "usage": {"total_tokens": 1500},
    }
    with pytest.raises(ValueError):
        client._parse_classification_response(truncated)


class _FakeResponse:
    """Duck-types the AsyncOpenAI response objects _request_and_parse uses."""

    def __init__(self, payload: dict):
        self._payload = payload
        usage = payload.get("usage") or {}

        class _Usage:
            total_tokens = usage.get("total_tokens", 0)

        self.usage = _Usage() if usage else None

    def model_dump(self) -> dict:
        return self._payload


def _install_fake_completions(client: OpenRouterClient, responses: list):
    """Point client._client at a stub whose create() pops from *responses*
    (repeating the last one) and records each call's kwargs."""
    calls: list = []

    class _Completions:
        async def create(self, **kwargs):
            calls.append(kwargs)
            payload = responses[min(len(calls) - 1, len(responses) - 1)]
            return _FakeResponse(payload)

    class _Chat:
        completions = _Completions()

    class _Client:
        chat = _Chat()

    client._client = _Client()
    return calls


_TRUNCATED_RESPONSE = {
    "choices": [
        {
            "finish_reason": "length",
            "message": {"content": None, "tool_calls": None},
        }
    ],
    "usage": {"total_tokens": 1500},
}


@pytest.mark.asyncio
async def test_classify_site_retries_truncated_response_with_larger_budget():
    client = OpenRouterClient(api_key="test-key", max_tokens=1500)
    calls = _install_fake_completions(
        client, [_TRUNCATED_RESPONSE, _tool_call_response("classify_website")]
    )

    result = await client.classify_site("example.com", content="some content")

    assert result["success"] is True
    assert result["quality"] == "Premium"
    assert len(calls) == 2
    assert calls[0]["max_tokens"] == 1500
    assert calls[1]["max_tokens"] == 3000


@pytest.mark.asyncio
async def test_classify_site_fails_when_every_attempt_truncates():
    client = OpenRouterClient(api_key="test-key", max_tokens=1500)
    calls = _install_fake_completions(client, [_TRUNCATED_RESPONSE])

    result = await client.classify_site("example.com", content="some content")

    assert result["success"] is False
    assert result["quality"] == "Error"
    assert "Classification failed" in result["justification"]
    # initial attempt + two truncation retries, each with a doubled budget
    assert [c["max_tokens"] for c in calls] == [1500, 3000, 6000]
