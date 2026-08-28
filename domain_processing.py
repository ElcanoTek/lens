# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

import logging
import time
from datetime import datetime
from typing import Any, Dict, Optional, TextIO

from config import config
from openrouter_client import OpenRouterClient
from progress_tracker import ProgressTracker
from reporting import TerminalReporter, shorten_error
from scraper_client import ScraperClient
from shared_types import DomainWorkItem, is_antibot_error

logger = logging.getLogger(__name__)

# Crawler rungs that drive a real rendering browser. A block at one of these
# means the site rejects even browser-like automation, not just plain HTTP
# clients.
_BROWSER_SCRAPE_MODES = ("firecrawl", "deep")


def derive_bot_protection(
    scrape_mode: str,
    *,
    current_error: Optional[str] = None,
    prior_error: Optional[str] = None,
    prior_error_mode: Optional[str] = None,
) -> str:
    """Classify a site's observed bot protection from how scraping went.

    Sites with bot mitigation tend to carry less invalid ad traffic, so this
    is a useful buying signal. Levels are strictly evidence-based — derived
    from which crawler rungs the site blocked during this run, never guessed
    by the LLM:

    - ``None Detected`` — no crawler rung was ever blocked.
    - ``Moderate`` — the plain-HTTP crawler was blocked (403/401/429 or a
      challenge page), but no rendering browser was proven blocked: the site
      filters basic bots.
    - ``Aggressive`` — a rendering-browser rung (Firecrawl/headless Chrome)
      was also blocked: the site rejects automated traffic from this host
      outright.
    - ``Unknown`` — the site could not be scraped but for reasons that are
      not block signatures (timeouts, DNS failures, dead site).

    Args:
        scrape_mode: The rung that produced the row being recorded.
        current_error: The failure message for Failed rows; None on success.
        prior_error: The last failure recorded by an earlier pass, if any.
        prior_error_mode: Which rung produced *prior_error* (None on state
            files from before modes were tracked; treated as plain-HTTP,
            the conservative reading).
    """
    if current_error and is_antibot_error(current_error) and scrape_mode != "research":
        blocked_rung = scrape_mode
    elif is_antibot_error(prior_error) or (current_error and is_antibot_error(current_error)):
        # Research-rung records only echo earlier scrape errors, so block
        # markers there are attributed to the rung that actually hit them.
        blocked_rung = prior_error_mode or "direct"
    else:
        if current_error is not None:
            return "Unknown"
        # Scraped successfully without ever being blocked; a research-rung
        # success means the site was never fetched, so nothing was observed.
        return "Unknown" if scrape_mode == "research" else "None Detected"

    return "Aggressive" if blocked_rung in _BROWSER_SCRAPE_MODES else "Moderate"


class DomainProcessor:
    def __init__(
        self,
        *,
        progress_tracker: ProgressTracker,
        scraper_client: ScraperClient,
        openrouter_client: OpenRouterClient,
        reporter: TerminalReporter,
        results_writer,
        results_file: Optional[TextIO],
        defer_failures: bool = False,
    ):
        self.progress_tracker = progress_tracker
        self.scraper_client = scraper_client
        self.openrouter_client = openrouter_client
        self.reporter = reporter
        self.results_writer = results_writer
        self.results_file = results_file
        # Auto mode's fast pass: failures will be retried by the deep-crawl
        # pass, so they are tracked as retry_pending (not yet processed)
        # rather than terminal errors.
        self.defer_failures = defer_failures

    async def process_domain(self, item: DomainWorkItem) -> None:
        """Process a single domain through the complete workflow."""
        start_time = time.perf_counter()
        scrape_result: Dict[str, Any] = {}
        classification_result: Dict[str, Any] = {}

        try:
            logger.debug("Processing %s", item.domain)

            session = await self.scraper_client.get_available_session()

            try:
                scrape_result = await self.scraper_client.scrape_site(session, item.domain)
                scrape_mode = scrape_result.get("mode", self.scraper_client.get_mode())

                validation_error = self._validate_scrape_result(scrape_result)
                if validation_error:
                    raise RuntimeError(f"Scrape validation failed: {validation_error}")

                classification_result = await self.openrouter_client.classify_site(
                    domain=item.domain,
                    content=scrape_result.get("content", ""),
                    title=scrape_result.get("title", ""),
                    meta_description=scrape_result.get("meta_description", ""),
                )
                classifier_mode = classification_result.get("source", "openrouter")

                if not classification_result.get("success", False):
                    raise RuntimeError(
                        f"Classification failed: {classification_result.get('error', 'Unknown error')}"
                    )

                processing_time = time.perf_counter() - start_time
                await self._record_success(
                    item,
                    classification_result,
                    scrape_result,
                    scrape_mode,
                    classifier_mode,
                    processing_time,
                )

                if self.reporter:
                    size_kb = (scrape_result.get("content_length", 0) or 0) / 1024
                    await self.reporter.log_attempt(
                        domain=item.domain,
                        ok=True,
                        status_code=scrape_result.get("status_code"),
                        error=None,
                        elapsed=processing_time,
                        size_kb=size_kb,
                        retries_used=scrape_result.get("retries_used", 0),
                        cache_status=scrape_result.get("cache_status", "miss"),
                    )

            finally:
                self.scraper_client.release_session(session)

        except Exception as e:
            processing_time = time.perf_counter() - start_time
            error_message = str(e)

            logger.error("❌ Failed to process %s: %s", item.domain, error_message)

            scrape_mode = scrape_result.get("mode", self.scraper_client.get_mode())
            classifier_mode = classification_result.get("source", "openrouter")
            scraped_at = scrape_result.get("scraped_at", datetime.now().isoformat())

            await self._record_failure(
                item,
                error_message,
                scrape_mode,
                classifier_mode,
                processing_time,
                scraped_at,
                scrape_result.get("content_length", 0) if scrape_result else 0,
            )

            if self.reporter:
                short_error = shorten_error(error_message)
                size_kb = (
                    (scrape_result.get("content_length", 0) or 0) / 1024 if scrape_result else None
                )
                await self.reporter.log_attempt(
                    domain=item.domain,
                    ok=False,
                    status_code=scrape_result.get("status_code"),
                    error=short_error,
                    elapsed=processing_time,
                    size_kb=size_kb,
                    retries_used=(scrape_result.get("retries_used", 0) if scrape_result else 0),
                    cache_status=(scrape_result.get("cache_status") if scrape_result else None),
                )

    async def process_domain_research(self, item: DomainWorkItem) -> None:
        """Classify a domain from external research when every scrape pass failed.

        Auto mode's final rung: a web-search-augmented model researches the
        domain (the target site is never contacted from this host), then the
        regular classifier runs on that research summary. Domains the research
        model knows nothing about stay Failed rather than being guessed at.
        """
        start_time = time.perf_counter()
        classification_result: Dict[str, Any] = {}
        research_content = ""

        try:
            logger.debug("Researching %s", item.domain)

            research = await self.openrouter_client.research_website(
                item.domain,
                research_model=getattr(config, "RESEARCH_MODEL", "perplexity/sonar-pro"),
                temperature=getattr(config, "RESEARCH_TEMPERATURE", 0.2),
                max_tokens=getattr(config, "RESEARCH_MAX_TOKENS", 1500),
            )
            if not research.get("success", False):
                raise RuntimeError(f"Research failed: {research.get('error', 'unknown error')}")
            research_content = research.get("research_content", "")
            if not research_content:
                raise RuntimeError(
                    "Research found no meaningful public information about this domain"
                )

            classification_result = await self.openrouter_client.classify_site(
                domain=item.domain,
                content=research_content,
                content_source="research",
            )
            if not classification_result.get("success", False):
                raise RuntimeError(
                    f"Classification failed: {classification_result.get('error', 'Unknown error')}"
                )

            processing_time = time.perf_counter() - start_time
            scrape_result = {
                "content_length": len(research_content),
                "scraped_at": datetime.now().isoformat(),
            }
            await self._record_success(
                item,
                classification_result,
                scrape_result,
                "research",
                classification_result.get("source", "openrouter"),
                processing_time,
            )

            if self.reporter:
                await self.reporter.log_attempt(
                    domain=item.domain,
                    ok=True,
                    status_code=None,
                    error=None,
                    elapsed=processing_time,
                    size_kb=len(research_content) / 1024,
                    retries_used=0,
                    cache_status="research",
                )

        except Exception as e:
            processing_time = time.perf_counter() - start_time
            # Keep the (more specific) scrape error visible alongside why the
            # research rescue could not finish the job either.
            prior_error = self.progress_tracker.get_domain_error(item.domain)
            error_message = str(e)
            if prior_error:
                error_message = f"{prior_error}; research fallback: {e}"

            logger.error("❌ Failed to process %s: %s", item.domain, error_message)

            await self._record_failure(
                item,
                error_message,
                "research",
                classification_result.get("source", "openrouter"),
                processing_time,
                datetime.now().isoformat(),
                len(research_content),
            )

            if self.reporter:
                await self.reporter.log_attempt(
                    domain=item.domain,
                    ok=False,
                    status_code=None,
                    error=shorten_error(error_message),
                    elapsed=processing_time,
                    size_kb=None,
                    retries_used=0,
                    cache_status="research",
                )

    def _validate_scrape_result(self, scrape_result: Dict[str, Any]) -> Optional[str]:
        """Validate that the scraper returned meaningful page content."""

        if not scrape_result:
            return "Scraper returned no data"

        if not scrape_result.get("success", False):
            return scrape_result.get("error") or "Scraper reported failure"

        status_code = scrape_result.get("status_code")
        status_code_int: Optional[int] = None
        if status_code is not None:
            try:
                status_code_int = int(status_code)
            except (TypeError, ValueError):
                status_code_int = None

        if status_code_int is not None and not (200 <= status_code_int < 400):
            return f"Received HTTP status {status_code_int}"

        content = scrape_result.get("content", "")
        if not content or not content.strip():
            return "Scraper returned empty content"

        content_length_raw = scrape_result.get("content_length")
        if content_length_raw is None:
            content_length_raw = len(content)

        try:
            content_length = int(content_length_raw)
        except (TypeError, ValueError):
            content_length = len(content)

        if content_length < config.MIN_CONTENT_LENGTH:
            return (
                f"Scraped content too short ({content_length} chars < {config.MIN_CONTENT_LENGTH})"
            )

        lower_snippet = content.lower()[:512]
        block_phrases = (
            "access denied",
            "attention required",
            "captcha",
            "forbidden",
            "just a moment",
            "service unavailable",
            "temporarily unavailable",
        )

        for phrase in block_phrases:
            if phrase in lower_snippet:
                return f"Potential block page detected ('{phrase}')"

        return None

    async def _record_success(
        self,
        item: DomainWorkItem,
        classification_result: Dict[str, Any],
        scrape_result: Dict[str, Any],
        scrape_mode: str,
        classifier_mode: str,
        processing_time: float,
    ) -> None:
        # An earlier pass's block (e.g. direct got a 403 before Firecrawl got
        # through) is the evidence for the bot-protection level; read it
        # before mark_domain_processed overwrites it below.
        prior_error, prior_error_mode = self.progress_tracker.get_domain_failure_details(
            item.domain
        )
        record = {
            "Domain": item.domain,
            "Type": "WEBSITE",
            "App_Name": "",
            "Developer": "",
            "Store_Category": "",
            "Rating": "",
            "Rating_Count": "",
            "Downloads": "",
            "Quality": classification_result["quality"],
            "Justification": classification_result["justification"],
            "IAB Tier 1": classification_result.get("vertical_tier_1", ""),
            "IAB Tier 2": classification_result.get("vertical_tier_2", ""),
            "IAB Tier 3": classification_result.get("vertical_tier_3", ""),
            "Description": classification_result["description"],
            "Language": classification_result.get("language", "Unknown"),
            "Political_Leaning": classification_result.get("political_leaning", "Non-Political"),
            "Audience_Size": classification_result.get("audience_size", "Unknown"),
            "Bot_Protection": derive_bot_protection(
                scrape_mode,
                prior_error=prior_error,
                prior_error_mode=prior_error_mode,
            ),
            "Content_Length": scrape_result.get("content_length", 0),
            "Processing_Time": round(processing_time, 2),
            "Scrape_Mode": scrape_mode,
            "Classifier_Mode": classifier_mode,
            "Scraped_At": scrape_result.get("scraped_at", datetime.now().isoformat()),
        }

        self._write_result(record)

        await self.progress_tracker.mark_domain_processed(
            domain=item.domain,
            status="success",
            result=record,
            processing_time=processing_time,
        )

        logger.debug("✅ %s → %s", item.domain, classification_result["quality"])

    async def _record_failure(
        self,
        item: DomainWorkItem,
        error_message: str,
        scrape_mode: str,
        classifier_mode: str,
        processing_time: float,
        scraped_at: str,
        content_length: int = 0,
    ) -> None:
        prior_error, prior_error_mode = self.progress_tracker.get_domain_failure_details(
            item.domain
        )
        record = {
            "Domain": item.domain,
            "Type": "WEBSITE",
            "App_Name": "",
            "Developer": "",
            "Store_Category": "",
            "Rating": "",
            "Rating_Count": "",
            "Downloads": "",
            "Quality": "Failed",
            "Justification": f"Processing failed: {error_message}",
            "IAB Tier 1": "N/A",
            "IAB Tier 2": "N/A",
            "IAB Tier 3": "N/A",
            "Description": "N/A",
            "Language": "Unknown",
            "Political_Leaning": "Non-Political",
            "Audience_Size": "Unknown",
            "Bot_Protection": derive_bot_protection(
                scrape_mode,
                current_error=error_message,
                prior_error=prior_error,
                prior_error_mode=prior_error_mode,
            ),
            "Content_Length": content_length,
            "Processing_Time": round(processing_time, 2),
            "Scrape_Mode": scrape_mode,
            "Classifier_Mode": classifier_mode,
            "Scraped_At": scraped_at,
        }

        self._write_result(record)

        await self.progress_tracker.mark_domain_processed(
            domain=item.domain,
            status="retry_pending" if self.defer_failures else "error",
            error_message=error_message,
            processing_time=processing_time,
            scrape_mode=scrape_mode,
        )

    def _write_result(self, record: Dict[str, Any]) -> None:
        """Persist a result row to the output CSV."""
        if not self.results_writer or not self.results_file:
            raise RuntimeError("Results writer has not been initialised")

        self.results_writer.writerow(record)
        self.results_file.flush()
