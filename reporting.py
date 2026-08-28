# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

import asyncio
import json
import sys
import time
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, Optional

try:  # Prefer rich for terminal output enhancements
    from rich.console import Console
    from rich.progress import (
        BarColumn,
        Progress,
        SpinnerColumn,
        TextColumn,
        TimeElapsedColumn,
        TimeRemainingColumn,
    )
    from rich.table import Table

    RICH_AVAILABLE = True
except ImportError:  # pragma: no cover - fallback when rich isn't installed
    Console = None  # type: ignore[assignment]
    Progress = None  # type: ignore[assignment]
    Table = None  # type: ignore[assignment]
    RICH_AVAILABLE = False


def shorten_error(message: str, *, max_length: int = 80) -> str:
    """Return the first sentence of an error message, truncated for readability."""

    first_line = message.splitlines()[0].strip()
    if not first_line:
        return "Unknown error"

    sentence_end = first_line.find(". ")
    if sentence_end != -1:
        first_line = first_line[: sentence_end + 1]

    if len(first_line) <= max_length:
        return first_line

    truncated = first_line[: max_length - 1].rstrip()
    return f"{truncated}…"


def format_duration(seconds: float) -> str:
    """Format a duration in seconds as HhMmSs with compact styling."""

    total_seconds = max(0, int(round(seconds)))
    minutes, sec = divmod(total_seconds, 60)
    hours, minutes = divmod(minutes, 60)

    parts = []
    if hours:
        parts.append(f"{hours}h")
    parts.append(f"{minutes}m")
    parts.append(f"{sec}s")
    return "".join(parts)


class TerminalReporter:
    """Render progress, per-domain status, and summaries to the terminal."""

    def __init__(
        self,
        *,
        total: Optional[int],
        completed: int,
        successes: int,
        failures: int,
        quiet: bool,
        verbose: bool,
        jsonl_path: Optional[str],
    ) -> None:
        self.quiet = quiet
        self.verbose = verbose
        self.total = total if total and total > 0 else None
        self.total_display = total if total is not None else "?"
        self.completed = completed
        self.successes = successes
        self.failures = failures
        self.jsonl_path = Path(jsonl_path) if jsonl_path else None
        self.jsonl_file: Optional[Any] = None
        self.lock = asyncio.Lock()
        self.is_tty = sys.stdout.isatty()
        self.use_rich = RICH_AVAILABLE and self.is_tty
        self.console: Optional[Console] = Console(highlight=False) if self.use_rich else None
        self.progress: Optional[Progress] = None
        self.progress_task: Optional[int] = None
        self.start_time = time.perf_counter()
        self._last_metrics_line: Optional[str] = None
        # Latest outcome per domain. Auto mode re-attempts failed domains in
        # later passes; counters must track domains, not attempts, or the
        # progress bar overshoots its total.
        self._domain_outcomes: Dict[str, bool] = {}

    def start(self) -> None:
        """Initialise progress display and JSON logging."""

        if self.jsonl_path:
            self.jsonl_path.parent.mkdir(parents=True, exist_ok=True)
            self.jsonl_file = open(self.jsonl_path, "a", encoding="utf-8")

        if self.use_rich:
            columns = [
                TextColumn("{task.fields[metrics]}", justify="left"),
            ]

            if self.total is not None:
                columns.insert(1, BarColumn(bar_width=None))
                columns.append(TimeRemainingColumn())
            else:
                columns.insert(1, SpinnerColumn(style="cyan"))

            columns.append(TimeElapsedColumn())

            self.progress = Progress(
                *columns,
                console=self.console,
                transient=False,
                refresh_per_second=5,
            )

            metrics = self._metrics_text()
            self.progress_task = self.progress.add_task(
                "Progress",
                total=self.total,
                completed=self.completed,
                metrics=metrics,
            )
            self.progress.start()
        else:
            # Print initial metrics for non-rich environments when there is existing progress.
            initial_metrics = self._metrics_text()
            if initial_metrics:
                self._emit_metrics(initial_metrics)

    async def log_attempt(
        self,
        *,
        domain: str,
        ok: bool,
        status_code: Optional[Any],
        error: Optional[str],
        elapsed: float,
        size_kb: Optional[float],
        retries_used: int,
        cache_status: Optional[str],
    ) -> None:
        """Record the outcome of a domain attempt and refresh the UI."""

        async with self.lock:
            previous = self._domain_outcomes.get(domain)
            self._domain_outcomes[domain] = ok
            advance = 0
            if previous is None:
                self.completed += 1
                advance = 1
                if ok:
                    self.successes += 1
                else:
                    self.failures += 1
            elif previous != ok:
                # A retry pass changed this domain's outcome.
                if ok:
                    self.successes += 1
                    self.failures -= 1
                else:
                    self.failures += 1
                    self.successes -= 1

            metrics = self._metrics_text()

            if not self.quiet:
                self._print_attempt_line(
                    domain=domain,
                    ok=ok,
                    status_code=status_code,
                    error=error,
                    elapsed=elapsed,
                    size_kb=size_kb,
                    retries_used=retries_used,
                    cache_status=cache_status,
                )

            if self.progress and self.progress_task is not None:
                self.progress.update(
                    self.progress_task,
                    advance=advance,
                    metrics=metrics,
                )
            else:
                self._emit_metrics(metrics)

            if self.jsonl_file:
                json_entry = {
                    "timestamp": datetime.now().isoformat(),
                    "domain": domain,
                    "ok": ok,
                    "status_code": status_code,
                    "error": error,
                    "elapsed_sec": round(elapsed, 2),
                    "retries_used": retries_used,
                }
                self.jsonl_file.write(json.dumps(json_entry) + "\n")
                self.jsonl_file.flush()

    def log_retry(
        self,
        *,
        domain: str,
        attempt: int,
        total: int,
        delay: float,
        reason: str,
    ) -> None:
        """Render retry information in yellow."""

        timestamp = datetime.now().strftime("%H:%M:%S")
        message = f"[{timestamp}] ⚠️ Retry {attempt}/{total} for {domain} in {delay:.1f}s ({reason})"

        if self.use_rich and self.console:
            target_console = self.progress.console if self.progress else self.console
            target_console.print(f"[yellow]{message}[/yellow]")
        else:
            print(message, flush=True)

    def stop(self) -> None:
        """Tear down progress output and close resources."""

        if self.progress:
            self.progress.stop()
            self.progress = None
        if self.jsonl_file:
            self.jsonl_file.close()
            self.jsonl_file = None

    def render_summary(self, summary: Dict[str, Any], duration: float) -> None:
        """Display the final summary block."""

        total = summary.get("total_domains", self.total or 0)
        successes = summary.get("successful", self.successes)
        failures = summary.get("errors", self.failures)
        attempts = successes + failures
        failure_rate = (failures / attempts * 100) if attempts else 0.0
        duration_text = format_duration(duration)

        if self.use_rich and self.console:
            table = Table(show_header=False, box=None)
            table.add_column("Metric", justify="right", style="bold")
            table.add_column("Value", justify="left")
            table.add_row("Total", str(total))
            table.add_row("✅ Success", str(successes))
            table.add_row("❌ Failures", str(failures))
            table.add_row("Failure Rate", f"{failure_rate:.1f}%")
            table.add_row("Duration", duration_text)

            self.console.print("\n--- Lens run complete ---", style="bold")
            self.console.print(table)
        else:
            print("\n--- Lens run complete ---")
            print(f"Total: {total}")
            print(f"✅ Success: {successes}")
            print(f"❌ Failures: {failures}")
            print(f"Failure Rate: {failure_rate:.1f}%")
            print(f"Duration: {duration_text}")

    def _metrics_text(self) -> str:
        attempts = self.successes + self.failures
        failure_rate = (self.failures / attempts * 100) if attempts else 0.0
        total_display = self.total_display
        return (
            f"Progress: {self.completed}/{total_display} | "
            f"Succ: {self.successes} | Fail: {self.failures} | "
            f"Failure Rate: {failure_rate:.1f}%"
        )

    def _emit_metrics(self, metrics: str) -> None:
        if metrics == self._last_metrics_line:
            return
        print(metrics, flush=True)
        self._last_metrics_line = metrics

    def _print_attempt_line(
        self,
        *,
        domain: str,
        ok: bool,
        status_code: Optional[Any],
        error: Optional[str],
        elapsed: float,
        size_kb: Optional[float],
        retries_used: int,
        cache_status: Optional[str],
    ) -> None:
        timestamp = datetime.now().strftime("%H:%M:%S")
        extra_parts = []
        if self.verbose:
            if size_kb is not None:
                extra_parts.append(f"Size: {size_kb:.1f}KB")
            extra_parts.append(f"Retries: {retries_used}")
            if cache_status:
                extra_parts.append(f"Cache: {cache_status}")

        extra = " | ".join(extra_parts)

        if ok:
            status_text = status_code if status_code is not None else "OK"
            base_message = f"[{timestamp}] ✅ Success {domain} ({status_text}) {elapsed:.2f}s"
            if self.use_rich and self.console:
                target_console = self.progress.console if self.progress else self.console
                rich_message = (
                    f"[{timestamp}] [green]✅ Success[/green] {domain} "
                    f"({status_text}) {elapsed:.2f}s"
                )
                if extra:
                    rich_message += f" | {extra}"
                target_console.print(rich_message)
            else:
                if extra:
                    base_message += f" | {extra}"
                print(base_message, flush=True)
        else:
            error_text = error or "Unknown error"
            base_message = f"[{timestamp}] ❌ Failed {domain} ({error_text}) after {elapsed:.2f}s"
            if self.use_rich and self.console:
                target_console = self.progress.console if self.progress else self.console
                rich_message = (
                    f"[{timestamp}] [red]❌ Failed[/red] {domain} "
                    f"({error_text}) after {elapsed:.2f}s"
                )
                if extra:
                    rich_message += f" | {extra}"
                target_console.print(rich_message)
            else:
                if extra:
                    base_message += f" | {extra}"
                print(base_message, flush=True)
