# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Tests for the research fallback pass (auto mode's final rung)."""

import csv
import io
import os

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

from config import config  # noqa: E402
from conftest import make_orchestrator as _make_orchestrator  # noqa: E402
from domain_processing import DomainProcessor  # noqa: E402
from progress_tracker import ProgressTracker  # noqa: E402
from shared_types import DomainWorkItem  # noqa: E402


class _FakeOpenRouter:
    def __init__(
        self,
        research_content="Reuters is a global news agency.",
        research_success=True,
        classify_success=True,
    ):
        self.research_content = research_content
        self.research_success = research_success
        self.classify_success = classify_success
        self.research_calls = []
        self.classify_calls = []

    async def research_website(self, domain, **kwargs):
        self.research_calls.append(domain)
        if not self.research_success:
            return {"success": False, "error": "boom", "research_content": ""}
        return {"success": True, "research_content": self.research_content}

    async def classify_site(
        self, domain, content="", title="", meta_description="", content_source="scrape"
    ):
        self.classify_calls.append((domain, content_source))
        if not self.classify_success:
            return {"success": False, "error": "classifier down"}
        return {
            "success": True,
            "quality": "Premium",
            "justification": "Major news agency",
            "vertical_tier_1": "News and Politics",
            "vertical_tier_2": "",
            "vertical_tier_3": "",
            "description": "Global news agency",
            "language": "English",
            "political_leaning": "Center",
            "audience_size": "XL",
            "source": "openrouter",
        }


def _make_processor(tmp_path, openrouter):
    tracker = ProgressTracker(str(tmp_path / "progress.json"))
    out = io.StringIO()
    writer = csv.DictWriter(out, fieldnames=config.CSV_FIELDNAMES)
    processor = DomainProcessor(
        progress_tracker=tracker,
        scraper_client=None,
        openrouter_client=openrouter,
        reporter=None,
        results_writer=writer,
        results_file=out,
    )
    return processor, tracker, out


async def test_research_success_records_research_scrape_mode(tmp_path):
    openrouter = _FakeOpenRouter()
    processor, tracker, out = _make_processor(tmp_path, openrouter)

    await processor.process_domain_research(DomainWorkItem(domain="reuters.com"))

    assert openrouter.classify_calls == [("reuters.com", "research")]
    rows = list(csv.DictReader(io.StringIO(out.getvalue()), fieldnames=config.CSV_FIELDNAMES))
    assert rows[0]["Quality"] == "Premium"
    assert rows[0]["Scrape_Mode"] == "research"
    assert tracker.is_domain_processed("reuters.com")
    assert "reuters.com" not in tracker.get_failed_domains()


async def test_research_insufficient_keeps_domain_failed(tmp_path):
    openrouter = _FakeOpenRouter(research_content="")
    processor, tracker, out = _make_processor(tmp_path, openrouter)
    await tracker.mark_domain_processed(
        "unknownsite.example",
        status="retry_pending",
        error_message="Received HTTP status 403",
    )

    await processor.process_domain_research(DomainWorkItem(domain="unknownsite.example"))

    assert openrouter.classify_calls == []  # never guessed at
    rows = list(csv.DictReader(io.StringIO(out.getvalue()), fieldnames=config.CSV_FIELDNAMES))
    assert rows[0]["Quality"] == "Failed"
    # The original scrape error stays visible in the failure justification.
    assert "403" in rows[0]["Justification"]
    assert "unknownsite.example" in tracker.get_failed_domains()


async def test_research_error_keeps_domain_failed(tmp_path):
    openrouter = _FakeOpenRouter(research_success=False)
    processor, tracker, out = _make_processor(tmp_path, openrouter)

    await processor.process_domain_research(DomainWorkItem(domain="example.com"))

    assert "example.com" in tracker.get_failed_domains()


async def test_orchestrator_research_pass_processes_failed_websites(monkeypatch, tmp_path):
    orchestrator = _make_orchestrator(monkeypatch, tmp_path, "pageURL\nblocked.com\nfine.com\n")
    items = orchestrator._load_input_data()
    await orchestrator.progress_tracker.mark_domain_processed(
        "blocked.com", status="error", error_message="Received HTTP status 401"
    )
    await orchestrator.progress_tracker.mark_domain_processed("fine.com", status="success")

    researched = []

    async def fake_research(self, item):
        researched.append(item.domain)

    monkeypatch.setattr(DomainProcessor, "process_domain_research", fake_research)
    await orchestrator._research_failed_websites(items)

    assert researched == ["blocked.com"]


async def test_orchestrator_research_pass_respects_disable_flag(monkeypatch, tmp_path):
    orchestrator = _make_orchestrator(monkeypatch, tmp_path, "pageURL\nblocked.com\n")
    items = orchestrator._load_input_data()
    await orchestrator.progress_tracker.mark_domain_processed(
        "blocked.com", status="error", error_message="Received HTTP status 401"
    )
    monkeypatch.setattr(config, "RESEARCH_FALLBACK_ENABLED", False)

    researched = []

    async def fake_research(self, item):
        researched.append(item.domain)

    monkeypatch.setattr(DomainProcessor, "process_domain_research", fake_research)
    await orchestrator._research_failed_websites(items)

    assert researched == []


async def test_research_website_detects_insufficient_information():
    from openrouter_client import OpenRouterClient

    client = OpenRouterClient(api_key="test-key")

    class _Msg:
        def model_dump(self):
            return {
                "choices": [{"message": {"content": "INSUFFICIENT INFORMATION"}}],
                "usage": {"total_tokens": 10},
            }

    async def fake_call(func, **kwargs):
        return _Msg()

    client._call_api_with_retry = fake_call
    client._client = object()  # sentinel; the fake call never touches it

    result = await client.research_website("nobody-knows-this.example")
    assert result["success"] is True
    assert result["research_content"] == ""
