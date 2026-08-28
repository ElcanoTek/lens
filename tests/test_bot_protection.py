# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Tests for the Bot_Protection output column and its derivation."""

import csv
import os
import sys
from pathlib import Path

import pytest

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

PROJECT_ROOT = Path(__file__).resolve().parents[1]
if str(PROJECT_ROOT) not in sys.path:
    sys.path.insert(0, str(PROJECT_ROOT))

from config import config
from domain_processing import DomainProcessor, derive_bot_protection
from progress_tracker import ProgressTracker
from shared_types import DomainWorkItem

from conftest import make_orchestrator as _make_orchestrator


# --- derivation matrix -------------------------------------------------------

def test_direct_success_means_none_detected():
    assert derive_bot_protection("direct") == "None Detected"


def test_browser_rescue_after_403_means_moderate():
    # Auto ladder: direct pass got a 403, Firecrawl rendered the page fine.
    assert derive_bot_protection(
        "firecrawl",
        prior_error="Scrape validation failed: Received HTTP status 403",
        prior_error_mode="direct",
    ) == "Moderate"


def test_browser_rescue_after_nonblock_failure_means_none():
    # JS-heavy site: direct pass content was too short, not blocked.
    assert derive_bot_protection(
        "firecrawl",
        prior_error="Scraped content too short (101 chars < 500)",
        prior_error_mode="direct",
    ) == "None Detected"


def test_research_rescue_after_browser_block_means_aggressive():
    assert derive_bot_protection(
        "research",
        prior_error="Potential block page detected ('just a moment')",
        prior_error_mode="firecrawl",
    ) == "Aggressive"


def test_research_rescue_after_plain_http_block_means_moderate():
    # Ladder without Firecrawl/Chrome available: only the plain client was
    # proven blocked, so don't overstate.
    assert derive_bot_protection(
        "research",
        prior_error="Received HTTP status 403",
        prior_error_mode="direct",
    ) == "Moderate"


def test_research_rescue_without_block_evidence_means_unknown():
    assert derive_bot_protection(
        "research",
        prior_error="Chrome scraping failed: timeout",
        prior_error_mode="deep",
    ) == "Unknown"


def test_direct_failure_with_403_means_moderate():
    assert derive_bot_protection(
        "direct",
        current_error="Scrape validation failed: Received HTTP status 403",
    ) == "Moderate"


def test_browser_failure_with_block_means_aggressive():
    assert derive_bot_protection(
        "deep",
        current_error="Potential block page detected ('captcha')",
        prior_error="Received HTTP status 403",
        prior_error_mode="direct",
    ) == "Aggressive"


def test_research_failure_echoing_prior_block_uses_prior_rung():
    # The research pass concatenates the scrape error into its own message;
    # the block markers belong to the rung that actually hit them.
    combined = (
        "Received HTTP status 403; research fallback: Research found no "
        "meaningful public information about this domain"
    )
    assert derive_bot_protection(
        "research",
        current_error=combined,
        prior_error="Received HTTP status 403",
        prior_error_mode="firecrawl",
    ) == "Aggressive"
    assert derive_bot_protection(
        "research",
        current_error=combined,
        prior_error="Received HTTP status 403",
        prior_error_mode="direct",
    ) == "Moderate"


def test_failure_without_block_evidence_means_unknown():
    assert derive_bot_protection(
        "direct", current_error="Chrome scraping failed: timeout"
    ) == "Unknown"


def test_prior_mode_missing_reads_conservatively_as_moderate():
    # State files from before modes were tracked.
    assert derive_bot_protection(
        "research",
        prior_error="Received HTTP status 429",
        prior_error_mode=None,
    ) == "Moderate"


# --- end-to-end through the record writers -----------------------------------

def _make_processor(tmp_path):
    tracker = ProgressTracker(str(tmp_path / "progress.json"))
    out = open(tmp_path / "out.csv", "w", newline="")
    writer = csv.DictWriter(out, fieldnames=config.CSV_FIELDNAMES)
    writer.writeheader()
    processor = DomainProcessor(
        progress_tracker=tracker,
        scraper_client=None,
        openrouter_client=None,
        reporter=None,
        results_writer=writer,
        results_file=out,
    )
    return processor, tracker, out


def _read_rows(tmp_path):
    with open(tmp_path / "out.csv", newline="") as f:
        return list(csv.DictReader(f))


@pytest.mark.asyncio
async def test_success_row_carries_bot_protection_from_prior_pass(tmp_path):
    processor, tracker, out = _make_processor(tmp_path)
    try:
        await tracker.mark_domain_processed(
            "blocked.com",
            status="retry_pending",
            error_message="Scrape validation failed: Received HTTP status 403",
            scrape_mode="direct",
        )
        await processor._record_success(
            DomainWorkItem(domain="blocked.com"),
            {
                "quality": "Standard",
                "justification": "j",
                "description": "d",
            },
            {"content_length": 1000, "scraped_at": "2026-01-01T00:00:00"},
            "firecrawl",
            "openrouter",
            1.0,
        )
    finally:
        out.close()

    rows = _read_rows(tmp_path)
    assert rows[0]["Bot_Protection"] == "Moderate"
    assert rows[0]["Quality"] == "Standard"


@pytest.mark.asyncio
async def test_failure_row_carries_bot_protection_and_mode_is_tracked(tmp_path):
    processor, tracker, out = _make_processor(tmp_path)
    try:
        await processor._record_failure(
            DomainWorkItem(domain="fortress.com"),
            "Scrape validation failed: Received HTTP status 403",
            "direct",
            "openrouter",
            1.0,
            "2026-01-01T00:00:00",
        )
    finally:
        out.close()

    rows = _read_rows(tmp_path)
    assert rows[0]["Bot_Protection"] == "Moderate"
    error, mode = tracker.get_domain_failure_details("fortress.com")
    assert "403" in error
    assert mode == "direct"


# --- output schema migration --------------------------------------------------

def test_setup_output_file_migrates_old_header(monkeypatch, tmp_path):
    orchestrator = _make_orchestrator(monkeypatch, tmp_path, "pageURL\nexample.com\n")

    old_fields = [f for f in config.CSV_FIELDNAMES if f != "Bot_Protection"]
    out_path = tmp_path / "output.csv"
    with open(out_path, "w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=old_fields)
        writer.writeheader()
        writer.writerow({field: "x" for field in old_fields})

    orchestrator._setup_output_file()
    orchestrator._teardown_output_file()

    with open(out_path, newline="") as f:
        reader = csv.DictReader(f)
        assert reader.fieldnames == config.CSV_FIELDNAMES
        rows = list(reader)
    assert rows[0]["Bot_Protection"] == ""
    assert rows[0]["Domain"] == "x"
