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

    # The deep retry pass rescues the domain.
    await _attempt(reporter, "a.com", ok=True)
    assert reporter.completed == 1
    assert (reporter.successes, reporter.failures) == (1, 0)


async def test_plain_output_on_any_terminal(capsys, tmp_path):
    # rich was removed: the same plain lines go to a TTY and to a log file.
    reporter = TerminalReporter(
        total=2,
        completed=1,
        successes=1,
        failures=0,
        quiet=False,
        verbose=True,
        jsonl_path=str(tmp_path / "run.jsonl"),
    )
    reporter.start()
    await _attempt(reporter, "a.com", ok=False)
    reporter.log_retry(domain="b.com", attempt=1, total=3, delay=2.0, reason="timeout")
    reporter.stop()
    reporter.render_summary({"total_domains": 2, "successful": 1, "errors": 1}, 61)
    out = capsys.readouterr().out.splitlines()

    assert out[0] == "Progress: 1/2 | Succ: 1 | Fail: 0 | Failure Rate: 0.0%"
    assert out[1].endswith("❌ Failed a.com (boom) after 0.10s | Size: 1.0KB | Retries: 0")
    assert out[2] == "Progress: 2/2 | Succ: 1 | Fail: 1 | Failure Rate: 50.0%"
    assert out[3].endswith("⚠️ Retry 1/3 for b.com in 2.0s (timeout)")
    assert out[4:] == [
        "",
        "--- Lens run complete ---",
        "Total: 2",
        "✅ Success: 1",
        "❌ Failures: 1",
        "Failure Rate: 50.0%",
        "Duration: 1m1s",
    ]
    assert '"domain": "a.com"' in (tmp_path / "run.jsonl").read_text()
