# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Tests for the single smart "auto" mode: detection, routing, and retry."""

import base64
import csv
import json
import os
import time

import pytest

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

# Self-contained unified-auth harness: mint elcano_auth cookies with a local
# Ed25519 key and point the verifier at it per-test (auth_cookie re-reads
# AUTH_SIGNING_PUBKEY on every request, so monkeypatch.setenv is enough).
from cryptography.hazmat.primitives import serialization as _ser
from cryptography.hazmat.primitives.asymmetric.ed25519 import (
    Ed25519PrivateKey as _Ed,
)

_AUTH_PRIV = _Ed.generate()
_AUTH_PUB_B64 = base64.b64encode(
    _AUTH_PRIV.public_key().public_bytes(_ser.Encoding.Raw, _ser.PublicFormat.Raw)
).decode()


def _auth_cookie(email: str = "tester@elcanotek.com") -> str:
    payload = {
        "email": email,
        "tenant": "elcanotek.com",
        "iat": int(time.time()),
        "exp": int(time.time()) + 3600,
    }
    body = base64.urlsafe_b64encode(json.dumps(payload).encode()).decode().rstrip("=")
    sig = _AUTH_PRIV.sign(body.encode())
    return f"{body}.{base64.urlsafe_b64encode(sig).decode().rstrip('=')}"


os.environ["AUTH_SIGNING_PUBKEY"] = _AUTH_PUB_B64

from fastapi.testclient import TestClient  # noqa: E402

import web_service  # noqa: E402
from conftest import make_orchestrator as _make_orchestrator  # noqa: E402


def test_auto_mode_resolves_to_fast_first_pass(monkeypatch, tmp_path):
    orchestrator = _make_orchestrator(monkeypatch, tmp_path, "pageURL\nexample.com\n")
    assert orchestrator.auto_mode is True
    assert orchestrator.scrape_mode == "direct"


def test_explicit_modes_do_not_enable_auto(monkeypatch, tmp_path):
    orchestrator = _make_orchestrator(
        monkeypatch, tmp_path, "pageURL\nexample.com\n", scrape_mode="deep"
    )
    assert orchestrator.auto_mode is False
    assert orchestrator.scrape_mode == "deep"


def test_invalid_mode_rejected(monkeypatch, tmp_path):
    from orchestration import SiteAnalysisOrchestrator

    with pytest.raises(ValueError, match="Unsupported scrape mode"):
        SiteAnalysisOrchestrator(quiet=True, scrape_mode="bogus")


def test_auto_mode_routes_ctv_files_to_ctv_workflow(monkeypatch, tmp_path):
    ctv_csv = (
        'SSP,Publisher,"App, Account or Network Name",Bundle ID\n'
        "ExampleSSP,Northwind Broadcasting,Example News Network,512001\n"
    )
    orchestrator = _make_orchestrator(monkeypatch, tmp_path, ctv_csv)

    work_items = orchestrator._load_input_data()

    assert orchestrator.is_ctv_input is True
    assert work_items == []


def test_non_auto_mode_only_warns_on_ctv_files(monkeypatch, tmp_path):
    ctv_csv = (
        'SSP,Publisher,"App, Account or Network Name",Bundle ID\n'
        "ExampleSSP,Northwind Broadcasting,Example News Network,512001\n"
    )
    orchestrator = _make_orchestrator(monkeypatch, tmp_path, ctv_csv, scrape_mode="direct")

    orchestrator._load_input_data()
    assert orchestrator.is_ctv_input is False


def test_auto_mode_detects_mixed_content(monkeypatch, tmp_path):
    mixed = "pageURL\nexample.com\n333903271\ncom.spotify.music\n"
    orchestrator = _make_orchestrator(monkeypatch, tmp_path, mixed)

    work_items = orchestrator._load_input_data()

    assert orchestrator.is_ctv_input is False
    assert orchestrator.has_websites is True
    assert orchestrator.has_ios_apps is True
    assert orchestrator.has_android_apps is True
    assert len(work_items) == 3


@pytest.mark.asyncio
async def test_deep_retry_noop_without_failures(monkeypatch, tmp_path):
    from contextlib import AsyncExitStack

    orchestrator = _make_orchestrator(monkeypatch, tmp_path, "pageURL\nexample.com\n")
    items = orchestrator._load_input_data()

    # No failures recorded -> the retry pass must not even probe for podman.
    def _boom(*args, **kwargs):
        raise AssertionError("availability probe should not run")

    monkeypatch.setattr(orchestrator, "_deep_crawl_available", _boom)

    async with AsyncExitStack() as stack:
        await orchestrator._retry_failed_websites_deep(stack, items)


@pytest.mark.asyncio
async def test_deep_retry_skipped_when_deep_crawl_unavailable(monkeypatch, tmp_path):
    from contextlib import AsyncExitStack

    orchestrator = _make_orchestrator(monkeypatch, tmp_path, "pageURL\nexample.com\n")
    items = orchestrator._load_input_data()
    await orchestrator.progress_tracker.mark_domain_processed(
        "example.com", status="error", error_message="HTTP 403"
    )

    monkeypatch.setattr(orchestrator, "_deep_crawl_available", lambda: False)

    processed = []

    async def fake_process(retry_items):
        processed.extend(retry_items)

    monkeypatch.setattr(orchestrator, "_process_items_concurrently", fake_process)

    async with AsyncExitStack() as stack:
        await orchestrator._retry_failed_websites_deep(stack, items)

    assert processed == []


@pytest.mark.asyncio
async def test_deep_retry_targets_only_failed_websites(monkeypatch, tmp_path):
    from contextlib import AsyncExitStack

    mixed = "pageURL\nexample.com\ngithub.com\n333903271\n"
    orchestrator = _make_orchestrator(monkeypatch, tmp_path, mixed)
    items = orchestrator._load_input_data()

    await orchestrator.progress_tracker.mark_domain_processed(
        "example.com", status="error", error_message="HTTP 403"
    )
    await orchestrator.progress_tracker.mark_domain_processed("github.com", status="success")
    await orchestrator.progress_tracker.mark_domain_processed(
        "333903271", status="error", error_message="rate limited"
    )

    monkeypatch.setattr(orchestrator, "_deep_crawl_available", lambda: True)

    class FakeDeepScraper:
        async def __aenter__(self):
            return self

        async def __aexit__(self, *exc):
            return None

        async def create_session_pool(self, num_sessions):
            assert num_sessions == 1

    import orchestration

    monkeypatch.setattr(orchestration, "ScraperClient", lambda **kwargs: FakeDeepScraper())

    processed = []

    async def fake_process(retry_items):
        processed.extend(retry_items)

    monkeypatch.setattr(orchestrator, "_process_items_concurrently", fake_process)

    async with AsyncExitStack() as stack:
        await orchestrator._retry_failed_websites_deep(stack, items)

    # Only the failed WEBSITE retries; the failed iOS app does not.
    assert [item.identifier for item in processed] == ["example.com"]
    assert orchestrator.scrape_mode == "deep"


@pytest.mark.asyncio
async def test_retry_pending_not_counted_as_processed(tmp_path):
    from progress_tracker import ProgressTracker

    tracker = ProgressTracker(str(tmp_path / "progress.json"))
    tracker.set_total_domains(3)
    await tracker.mark_domain_processed("a.com", status="success")
    await tracker.mark_domain_processed("b.com", status="retry_pending", error_message="HTTP 403")
    await tracker.mark_domain_processed("c.com", status="error", error_message="DNS failure")

    summary = tracker.get_summary()
    assert summary["processed"] == 2
    assert summary["retrying"] == 1
    assert summary["successful"] == 1
    assert summary["errors"] == 1
    assert summary["completion_percentage"] == pytest.approx(66.7, abs=0.1)
    assert tracker.get_retry_pending_domains() == ["b.com"]


@pytest.mark.asyncio
async def test_retry_pending_resolves_to_success(tmp_path):
    from progress_tracker import ProgressTracker

    tracker = ProgressTracker(str(tmp_path / "progress.json"))
    tracker.set_total_domains(1)
    await tracker.mark_domain_processed("a.com", status="retry_pending", error_message="HTTP 403")
    await tracker.mark_domain_processed("a.com", status="success")

    summary = tracker.get_summary()
    assert summary["processed"] == 1
    assert summary["retrying"] == 0
    assert summary["successful"] == 1
    assert summary["errors"] == 0
    assert summary["completion_percentage"] == 100.0


@pytest.mark.asyncio
async def test_finalize_pending_retries_converts_to_errors(tmp_path):
    from progress_tracker import ProgressTracker

    tracker = ProgressTracker(str(tmp_path / "progress.json"))
    tracker.set_total_domains(2)
    await tracker.mark_domain_processed("a.com", status="retry_pending", error_message="HTTP 403")
    await tracker.mark_domain_processed("b.com", status="success")

    converted = await tracker.finalize_pending_retries()

    assert converted == 1
    summary = tracker.get_summary()
    assert summary["processed"] == 2
    assert summary["retrying"] == 0
    assert summary["errors"] == 1
    assert summary["completion_percentage"] == 100.0


@pytest.mark.asyncio
async def test_domain_processor_defers_failures(monkeypatch, tmp_path):
    from domain_processing import DomainProcessor
    from progress_tracker import ProgressTracker
    from shared_types import DomainWorkItem

    tracker = ProgressTracker(str(tmp_path / "progress.json"))
    tracker.set_total_domains(1)

    class FakeWriter:
        rows = []

        def writerow(self, record):
            self.rows.append(record)

    class FakeFile:
        def flush(self):
            pass

    class FakeScraper:
        def get_mode(self):
            return "direct"

        async def get_available_session(self):
            return object()

        def release_session(self, session):
            pass

        async def scrape_site(self, session, domain):
            return {"success": False, "error": "HTTP 403"}

    processor = DomainProcessor(
        progress_tracker=tracker,
        scraper_client=FakeScraper(),
        openrouter_client=None,
        reporter=None,
        results_writer=FakeWriter(),
        results_file=FakeFile(),
        defer_failures=True,
    )

    await processor.process_domain(DomainWorkItem(domain="blocked.com"))

    assert tracker.get_retry_pending_domains() == ["blocked.com"]
    summary = tracker.get_summary()
    assert summary["processed"] == 0
    assert summary["retrying"] == 1
    # The Failed row is still written; a successful retry replaces it via
    # the dedupe pass.
    assert FakeWriter.rows[0]["Quality"] == "Failed"


def test_read_progress_excludes_retry_pending(monkeypatch, tmp_path):
    progress_path = tmp_path / "progress.json"
    progress_path.write_text(
        json.dumps(
            {
                "total_domains": 4,
                "successful_count": 1,
                "error_count": 1,
                "processed_domains": {
                    "a.com": {"status": "success"},
                    "b.com": {"status": "error"},
                    "c.com": {"status": "retry_pending"},
                    "d.com": {"status": "retry_pending"},
                },
            }
        ),
        encoding="utf-8",
    )

    progress = web_service._read_progress(progress_path)

    assert progress["processed"] == 2
    assert progress["retrying"] == 2
    assert progress["remaining"] == 2
    assert progress["completion_percentage"] == 50.0


@pytest.mark.asyncio
async def test_deep_retry_includes_retry_pending_items(monkeypatch, tmp_path):
    from contextlib import AsyncExitStack

    orchestrator = _make_orchestrator(monkeypatch, tmp_path, "pageURL\nexample.com\ngithub.com\n")
    items = orchestrator._load_input_data()
    await orchestrator.progress_tracker.mark_domain_processed(
        "example.com", status="retry_pending", error_message="HTTP 403"
    )
    await orchestrator.progress_tracker.mark_domain_processed("github.com", status="success")

    monkeypatch.setattr(orchestrator, "_deep_crawl_available", lambda: True)

    class FakeDeepScraper:
        async def __aenter__(self):
            return self

        async def __aexit__(self, *exc):
            return None

        async def create_session_pool(self, num_sessions):
            pass

    import orchestration

    monkeypatch.setattr(orchestration, "ScraperClient", lambda **kwargs: FakeDeepScraper())

    processed = []

    async def fake_process(retry_items):
        processed.extend(retry_items)

    monkeypatch.setattr(orchestrator, "_process_items_concurrently", fake_process)

    async with AsyncExitStack() as stack:
        await orchestrator._retry_failed_websites_deep(stack, items)

    assert [item.identifier for item in processed] == ["example.com"]
    # The research fallback runs after the deep pass by default, so deep
    # failures stay retryable; with the fallback disabled they are terminal.
    assert orchestrator._defer_website_failures is True

    monkeypatch.setattr(orchestrator, "_research_fallback_available", lambda: False)
    processed.clear()
    await orchestrator.progress_tracker.mark_domain_processed(
        "example.com", status="retry_pending", error_message="HTTP 403"
    )
    async with AsyncExitStack() as stack:
        await orchestrator._retry_failed_websites_deep(stack, items)

    assert [item.identifier for item in processed] == ["example.com"]
    # With no later pass available, the deep pass records failures terminally.
    assert orchestrator._defer_website_failures is False


def test_dedupe_output_csv_prefers_success_rows(monkeypatch, tmp_path):
    from config import config

    output_path = tmp_path / "output.csv"
    fieldnames = ["Domain", "Quality", "Scrape_Mode"]
    rows = [
        {"Domain": "a.com", "Quality": "Failed", "Scrape_Mode": "direct"},
        {"Domain": "b.com", "Quality": "Premium", "Scrape_Mode": "direct"},
        {"Domain": "a.com", "Quality": "Standard", "Scrape_Mode": "deep"},
        {"Domain": "c.com", "Quality": "Failed", "Scrape_Mode": "direct"},
        {"Domain": "c.com", "Quality": "Failed", "Scrape_Mode": "deep"},
    ]
    with open(output_path, "w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=fieldnames)
        writer.writeheader()
        writer.writerows(rows)

    monkeypatch.setattr(config, "OUTPUT_CSV_PATH", str(output_path))
    orchestrator = _make_orchestrator(monkeypatch, tmp_path, "pageURL\nexample.com\n")
    monkeypatch.setattr(config, "OUTPUT_CSV_PATH", str(output_path))

    orchestrator._dedupe_output_csv()

    with open(output_path, newline="") as f:
        result = list(csv.DictReader(f))

    by_domain = {row["Domain"]: row for row in result}
    assert len(result) == 3
    # The deep-crawl rescue replaces the Failed row.
    assert by_domain["a.com"]["Quality"] == "Standard"
    assert by_domain["a.com"]["Scrape_Mode"] == "deep"
    # Repeated failures collapse to the most recent one.
    assert by_domain["c.com"]["Quality"] == "Failed"
    assert by_domain["c.com"]["Scrape_Mode"] == "deep"
    # Order of first appearance is preserved.
    assert [row["Domain"] for row in result] == ["a.com", "b.com", "c.com"]


def test_enqueue_defaults_to_auto_mode(monkeypatch, tmp_path):
    monkeypatch.setenv("AUTH_SIGNING_PUBKEY", _AUTH_PUB_B64)
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)
    (input_dir / "list.csv").write_text("pageURL\nexample.com\n", encoding="utf-8")

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())

        response = client.post(
            "/jobs",
            data={"input_file": "list.csv"},
            follow_redirects=False,
        )
        assert response.status_code == 303
        assert "message=" in response.headers["location"]

    state = json.loads((output_dir / web_service.JOBS_STATE_FILENAME).read_text(encoding="utf-8"))
    assert state["jobs"][0]["mode"] == "auto"


@pytest.mark.asyncio
async def test_run_job_passes_auto_scrape_mode(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)
    (input_dir / "list.csv").write_text("pageURL\nexample.com\n", encoding="utf-8")

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    captured = {}

    class DummyProcess:
        pid = 4242

        async def wait(self):
            return 0

    async def fake_spawn(*args, **kwargs):
        captured["cmd"] = list(args)
        return DummyProcess()

    monkeypatch.setattr(web_service.asyncio, "create_subprocess_exec", fake_spawn)

    manager = web_service.JobManager()
    job = await manager.create_job("list.csv", "auto")
    await manager._run_job(job.id)

    assert "--scrape-mode" in captured["cmd"]
    flag_index = captured["cmd"].index("--scrape-mode")
    assert captured["cmd"][flag_index + 1] == "auto"


def test_index_renders_unified_runs(monkeypatch, tmp_path):
    monkeypatch.setenv("AUTH_SIGNING_PUBKEY", _AUTH_PUB_B64)
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)
    (input_dir / "list.csv").write_text("pageURL\nexample.com\n", encoding="utf-8")

    # An orphaned artifact group (no job in state) must surface as archived.
    (output_dir / "20260101000000-abc123_output.csv").write_text("Domain\n", encoding="utf-8")
    (output_dir / "20260101000000-abc123_progress.json").write_text(
        json.dumps({"summary": {"total_domains": 5, "successful": 4, "errors": 1}}),
        encoding="utf-8",
    )

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())
        response = client.get("/")

    assert response.status_code == 200
    body = response.text
    assert "Ready to analyze" in body
    assert "20260101000000-abc123" in body
    assert "archived" in body
    # The mode picker is gone for good.
    assert 'name="mode"' not in body
