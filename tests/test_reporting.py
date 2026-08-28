# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Tests for TerminalReporter counters across auto-mode retry passes."""

import os

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

from reporting import TerminalReporter


def _reporter(total=2):
    return TerminalReporter(
        total=total,
        completed=0,
        successes=0,
        failures=0,
        quiet=True,
        verbose=False,
        jsonl_path=None,
    )


async def _attempt(reporter, domain, ok):
    await reporter.log_attempt(
        domain=domain,
        ok=ok,
        status_code=200 if ok else None,
        error=None if ok else "boom",
        elapsed=0.1,
        size_kb=1.0,
        retries_used=0,
        cache_status=None,
    )


async def test_counters_track_domains_not_attempts():
    reporter = _reporter(total=2)
    await _attempt(reporter, "a.com", ok=False)
    await _attempt(reporter, "b.com", ok=True)
    # Retry passes re-attempt a.com; completed must not exceed the total.
    await _attempt(reporter, "a.com", ok=False)
    await _attempt(reporter, "a.com", ok=False)

    assert reporter.completed == 2
    assert reporter.successes == 1
    assert reporter.failures == 1


async def test_retry_rescue_flips_failure_to_success():
    reporter = _reporter(total=1)
    await _attempt(reporter, "a.com", ok=False)
    assert (reporter.successes, reporter.failures) == (0, 1)

    # The Firecrawl/deep retry pass rescues the domain.
    await _attempt(reporter, "a.com", ok=True)
    assert reporter.completed == 1
    assert (reporter.successes, reporter.failures) == (1, 0)
