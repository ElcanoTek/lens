# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""
Progress tracking module for Lens.
"""

import asyncio
import json
import logging
import os
from datetime import datetime, timedelta
from typing import Any, Dict, List, Optional

logger = logging.getLogger(__name__)


class ProgressTracker:
    """Manages progress tracking and persistence for the site analysis script."""

    def __init__(self, progress_file: str):
        self.progress_file = progress_file
        self.progress_data = {
            "processed_domains": {},
            "last_processed_index": -1,
            "total_domains": 0,
            "successful_count": 0,
            "error_count": 0,
            "start_time": None,
            "last_update": None,
            "session_info": {},
        }
        self._lock = asyncio.Lock()
        self.load_progress()

    def load_progress(self) -> None:
        """Load progress from the progress file."""
        if os.path.exists(self.progress_file):
            try:
                with open(self.progress_file, "r") as f:
                    loaded_data = json.load(f)
                    self.progress_data.update(loaded_data)
                logger.info(f"Loaded progress: {self.get_summary()}")
            except (json.JSONDecodeError, IOError) as e:
                logger.warning(f"Could not load progress file {self.progress_file}: {e}")
                logger.info("Starting with fresh progress tracking")

    async def save_progress(self) -> None:
        """Save current progress to the progress file."""
        async with self._lock:
            self.progress_data["last_update"] = datetime.now().isoformat()
            try:
                # Write to temporary file first, then rename for atomic operation
                temp_file = f"{self.progress_file}.tmp"
                with open(temp_file, "w") as f:
                    json.dump(self.progress_data, f, indent=2)
                os.rename(temp_file, self.progress_file)
            except IOError as e:
                logger.error(f"Could not save progress file {self.progress_file}: {e}")

    async def mark_domain_processed(
        self,
        domain: str,
        status: str,
        result: Optional[Dict[str, Any]] = None,
        error_message: Optional[str] = None,
        processing_time: Optional[float] = None,
        scrape_mode: Optional[str] = None,
    ) -> None:
        """Mark a domain as processed with the given status and result.

        Status "retry_pending" records a fast-crawl failure that a deep-crawl
        pass will retry: it is not counted as processed, so progress bars
        don't read 100% while a retry queue is outstanding.
        """
        async with self._lock:
            previous_data = self.progress_data["processed_domains"].get(domain)
            if previous_data:
                previous_status = previous_data.get("status")
                if previous_status == "success" and self.progress_data["successful_count"] > 0:
                    self.progress_data["successful_count"] -= 1
                elif previous_status == "error" and self.progress_data["error_count"] > 0:
                    self.progress_data["error_count"] -= 1

            domain_data = {
                "status": status,
                "timestamp": datetime.now().isoformat(),
                "processing_time": processing_time,
            }

            if result:
                domain_data["result"] = result

            if error_message:
                domain_data["error_message"] = error_message

            if scrape_mode:
                domain_data["scrape_mode"] = scrape_mode

            self.progress_data["processed_domains"][domain] = domain_data

            if status == "success":
                self.progress_data["successful_count"] += 1
            elif status == "error":
                self.progress_data["error_count"] += 1

        # Save progress after each domain (for crash recovery)
        await self.save_progress()

    def is_domain_processed(self, domain: str) -> bool:
        """Check if a domain has already been processed successfully."""
        domain_data = self.progress_data["processed_domains"].get(domain)
        return domain_data is not None and domain_data.get("status") == "success"

    def get_processed_domains(self) -> List[str]:
        """Get list of all processed domains."""
        return list(self.progress_data["processed_domains"].keys())

    def get_successful_domains(self) -> List[str]:
        """Get list of successfully processed domains."""
        return [
            domain
            for domain, data in self.progress_data["processed_domains"].items()
            if data.get("status") == "success"
        ]

    def get_failed_domains(self) -> List[str]:
        """Get list of domains that failed processing."""
        return [
            domain
            for domain, data in self.progress_data["processed_domains"].items()
            if data.get("status") == "error"
        ]

    def get_retry_pending_domains(self) -> List[str]:
        """Get list of domains awaiting the deep-crawl retry pass."""
        return [
            domain
            for domain, data in self.progress_data["processed_domains"].items()
            if data.get("status") == "retry_pending"
        ]

    def get_domain_error(self, domain: str) -> Optional[str]:
        """Return the last recorded error message for a domain, if any."""
        data = self.progress_data["processed_domains"].get(domain)
        if not data:
            return None
        return data.get("error_message")

    def get_domain_failure_details(self, domain: str) -> tuple:
        """Return (error_message, scrape_mode) of the last recorded failure.

        The mode identifies which crawler rung produced the error (direct,
        firecrawl, deep, research); it is None for entries written before
        modes were tracked.
        """
        data = self.progress_data["processed_domains"].get(domain)
        if not data:
            return None, None
        return data.get("error_message"), data.get("scrape_mode")

    async def finalize_pending_retries(self) -> int:
        """Convert any leftover retry_pending entries to errors.

        Called when the retry pass is skipped or interrupted so progress can
        reach 100% and the entries read as real failures.
        """
        async with self._lock:
            pending = self.get_retry_pending_domains()
            for domain in pending:
                self.progress_data["processed_domains"][domain]["status"] = "error"
                self.progress_data["error_count"] += 1

        if pending:
            await self.save_progress()
            logger.info("Finalized %d unretried failures as errors", len(pending))
        return len(pending)

    def set_total_domains(self, total: int) -> None:
        """Set the total number of domains to process."""
        self.progress_data["total_domains"] = total
        if self.progress_data["start_time"] is None:
            self.progress_data["start_time"] = datetime.now().isoformat()

    def get_summary(self) -> Dict[str, Any]:
        """Get a summary of the current progress."""
        total = self.progress_data["total_domains"]
        retrying = sum(
            1
            for data in self.progress_data["processed_domains"].values()
            if data.get("status") == "retry_pending"
        )
        # retry_pending items are still in flight: a queued deep-crawl pass
        # will reprocess them, so they don't count toward completion.
        processed = len(self.progress_data["processed_domains"]) - retrying
        successful = self.progress_data["successful_count"]
        errors = self.progress_data["error_count"]
        remaining = max(0, total - processed)

        summary = {
            "total_domains": total,
            "processed": processed,
            "successful": successful,
            "errors": errors,
            "retrying": retrying,
            "remaining": remaining,
            "completion_percentage": (processed / total * 100) if total > 0 else 0,
        }

        if self.progress_data["start_time"]:
            start_time = datetime.fromisoformat(self.progress_data["start_time"])
            elapsed = datetime.now() - start_time
            summary["elapsed_time"] = str(elapsed)

            if processed > 0:
                avg_time_per_domain = elapsed.total_seconds() / processed
                estimated_remaining = avg_time_per_domain * remaining
                summary["estimated_time_remaining"] = str(
                    timedelta(seconds=int(estimated_remaining))
                )

        return summary

    def get_retry_domains(self) -> List[str]:
        """Get list of domains that should be retried (failed domains)."""
        return self.get_failed_domains()

    async def reset_failed_domains(self) -> None:
        """Reset failed domains so they can be retried."""
        async with self._lock:
            failed_domains = self.get_failed_domains()
            for domain in failed_domains:
                del self.progress_data["processed_domains"][domain]
                self.progress_data["error_count"] -= 1

        await self.save_progress()
        logger.info(f"Reset {len(failed_domains)} failed domains for retry")

    async def cleanup_old_progress(self, days: int = 7) -> None:
        """Remove progress entries older than specified days."""
        if not self.progress_data["processed_domains"]:
            return

        cutoff_date = datetime.now().timestamp() - (days * 24 * 60 * 60)
        domains_to_remove = []

        async with self._lock:
            for domain, data in self.progress_data["processed_domains"].items():
                try:
                    domain_timestamp = datetime.fromisoformat(data["timestamp"]).timestamp()
                    if domain_timestamp < cutoff_date:
                        domains_to_remove.append(domain)
                except (ValueError, KeyError):
                    # Invalid timestamp, remove it
                    domains_to_remove.append(domain)

            for domain in domains_to_remove:
                del self.progress_data["processed_domains"][domain]

        if domains_to_remove:
            await self.save_progress()
            logger.info(f"Cleaned up {len(domains_to_remove)} old progress entries")

    def print_summary(self) -> None:
        """Print a formatted summary of the current progress."""
        summary = self.get_summary()
        print("\n📊 Progress Summary:")
        print(f"   Total domains: {summary['total_domains']}")
        print(f"   Processed: {summary['processed']}")
        print(f"   Successful: {summary['successful']}")
        print(f"   Errors: {summary['errors']}")
        print(f"   Remaining: {summary['remaining']}")
        print(f"   Completion: {summary['completion_percentage']:.1f}%")

        if "elapsed_time" in summary:
            print(f"   Elapsed time: {summary['elapsed_time']}")

        if "estimated_time_remaining" in summary:
            print(f"   Estimated remaining: {summary['estimated_time_remaining']}")
        print()
