# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Scraper plumbing and the auto-mode retry ladder (direct -> deep -> research)."""

import json
import os

import pytest

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

from conftest import make_orchestrator
from scraper_client import ScraperClient, _browser_headers

_make_orchestrator = make_orchestrator


class FakeScraper:
    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return None

    async def create_session_pool(self, num_sessions):
        return None


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


def test_firecrawl_mode_is_gone():
    with pytest.raises(ValueError):
        import asyncio

        asyncio.run(ScraperClient(mode="firecrawl").__aenter__())


# ---------------------------------------------------------------------------
# Redirect policy
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "start,final",
    [
        ("https://example.com", "https://www.example.com/"),
        ("https://myfantasyleague.com", "https://home.myfantasyleague.com/"),
        ("https://m.example.com", "https://example.com/"),
        ("https://bbc.co.uk", "https://www.news.bbc.co.uk/"),
    ],
)
def test_same_site_redirect_is_allowed(start, final):
    ScraperClient(reject_redirects=True)._ensure_same_host(start, final)


@pytest.mark.parametrize(
    "start,final",
    [
        ("https://example.com", "https://parked-lander.com/x"),
        ("https://paper.io", "https://voodoo.io/"),
        ("https://foo.co.uk", "https://bar.co.uk/"),
        ("https://example.com", "https://notexample.com/"),
        ("https://shop.example.com", "https://evil.com/"),
    ],
)
def test_unrelated_domain_redirect_is_rejected(start, final):
    with pytest.raises(RuntimeError, match="redirected to different host"):
        ScraperClient(reject_redirects=True)._ensure_same_host(start, final)


def test_redirects_allowed_when_policy_disabled():
    ScraperClient(reject_redirects=False)._ensure_same_host(
        "https://example.com", "https://parked-lander.com/x"
    )


# ---------------------------------------------------------------------------
# Deep retry pass
# ---------------------------------------------------------------------------


async def test_deep_retry_skips_antibot_blocked_sites(monkeypatch, tmp_path):
    from contextlib import AsyncExitStack

    orchestrator = _make_orchestrator(
        monkeypatch, tmp_path, "pageURL\nblocked.com\nrenderfail.com\n"
    )
    items = orchestrator._load_input_data()

    # The direct pass proved blocked.com is blocking this IP; renderfail.com
    # failed for a reason a rendering browser might fix.
    await orchestrator.progress_tracker.mark_domain_processed(
        "blocked.com", status="error", error_message="Received HTTP status 403"
    )
    await orchestrator.progress_tracker.mark_domain_processed(
        "renderfail.com", status="error", error_message="Scraper returned empty content"
    )

    monkeypatch.setattr(orchestrator, "_deep_crawl_available", lambda: True)
    built = []

    def fake_build(self, mode, **kwargs):
        built.append((mode, kwargs))
        return FakeScraper()

    monkeypatch.setattr(type(orchestrator), "_build_scraper_client", fake_build)
    processed = []

    async def fake_process(retry_items):
        processed.extend(retry_items)

    monkeypatch.setattr(orchestrator, "_process_items_concurrently", fake_process)

    async with AsyncExitStack() as stack:
        await orchestrator._retry_failed_websites_deep(stack, items)

    assert [item.identifier for item in processed] == ["renderfail.com"]
    # The deep pass follows cross-host redirects by default.
    assert built == [("deep", {"reject_redirects": False})]


async def test_deep_retry_respects_strict_redirect_policy(monkeypatch, tmp_path):
    from contextlib import AsyncExitStack

    import config as config_module

    orchestrator = _make_orchestrator(monkeypatch, tmp_path, "pageURL\nrenderfail.com\n")
    items = orchestrator._load_input_data()
    await orchestrator.progress_tracker.mark_domain_processed(
        "renderfail.com", status="error", error_message="Scraper returned empty content"
    )
    monkeypatch.setattr(config_module.config, "RETRY_FOLLOW_REDIRECTS", False)
    monkeypatch.setattr(orchestrator, "_deep_crawl_available", lambda: True)
    built = []
    monkeypatch.setattr(
        type(orchestrator),
        "_build_scraper_client",
        lambda self, mode, **kwargs: built.append(kwargs) or FakeScraper(),
    )

    async def fake_process(retry_items):
        return None

    monkeypatch.setattr(orchestrator, "_process_items_concurrently", fake_process)
    async with AsyncExitStack() as stack:
        await orchestrator._retry_failed_websites_deep(stack, items)
    assert built == [{"reject_redirects": True}]


# ---------------------------------------------------------------------------
# Old config.json files from the Firecrawl era keep loading
# ---------------------------------------------------------------------------


def _load_config(tmp_path, monkeypatch, data):
    import config as config_module

    path = tmp_path / "config.json"
    path.write_text(json.dumps(data))
    monkeypatch.chdir(tmp_path)
    return config_module.Config(config_file=str(path))


def test_legacy_firecrawl_keys_are_dropped(tmp_path, monkeypatch, capsys):
    cfg = _load_config(
        tmp_path,
        monkeypatch,
        {"firecrawl_url": "http://x", "firecrawl_max_concurrent": 9, "scrape_mode": "firecrawl"},
    )
    assert not any(attr.startswith("FIRECRAWL_") for attr in vars(cfg))
    assert cfg.SCRAPE_MODE == "auto"
    assert "firecrawl" in capsys.readouterr().out


def test_legacy_redirect_setting_is_honoured(tmp_path, monkeypatch):
    cfg = _load_config(tmp_path, monkeypatch, {"firecrawl_follow_redirects": False})
    assert cfg.RETRY_FOLLOW_REDIRECTS is False


def test_new_redirect_setting_wins_over_legacy(tmp_path, monkeypatch):
    cfg = _load_config(
        tmp_path,
        monkeypatch,
        {"firecrawl_follow_redirects": False, "retry_follow_redirects": True},
    )
    assert cfg.RETRY_FOLLOW_REDIRECTS is True
