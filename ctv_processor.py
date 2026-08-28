# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""CTV (Connected TV) processor module for elcano-lens.

This module handles the processing of CTV applications using a two-step AI pipeline:
1. Research Step: Uses Perplexity Sonar Pro to gather detailed information about the CTV app
2. Classification Step: Uses a fast classification model to structure the research into
   a consistent format for advertising categorization

The two-step approach is cost-effective: Perplexity for powerful web search and synthesis,
and a smaller model for data structuring.
"""

import asyncio
import logging
import time
from datetime import datetime
from typing import Any, Dict, List, Optional, TextIO

from config import config
from openrouter_client import OpenRouterClient
from progress_tracker import ProgressTracker
from reporting import TerminalReporter, shorten_error
from shared_types import CTVWorkItem

logger = logging.getLogger(__name__)


class CTVProcessor:
    """Processor for CTV (Connected TV) applications using a two-step AI pipeline."""

    def __init__(
        self,
        *,
        progress_tracker: ProgressTracker,
        openrouter_client: OpenRouterClient,
        reporter: Optional[TerminalReporter],
        results_writer,
        results_file: Optional[TextIO],
        research_model: Optional[str] = None,
        research_temperature: float = 0.3,
        research_max_tokens: int = 2000,
        classification_model: Optional[str] = None,
        classification_temperature: float = 0.1,
        classification_max_tokens: int = 1500,
        request_delay: float = 1.0,
    ):
        """
        Initialize the CTV processor.

        Args:
            progress_tracker: Progress tracking instance
            openrouter_client: OpenRouter client for LLM calls
            reporter: Terminal reporter instance
            results_writer: CSV writer for results
            results_file: File handle for results
            research_model: Model to use for research step (default from config)
            research_temperature: Temperature for research model
            research_max_tokens: Max tokens for research response
            classification_model: Model to use for classification step (default from config)
            classification_temperature: Temperature for classification model
            classification_max_tokens: Max tokens for classification response
            request_delay: Delay between requests in seconds
        """
        self.progress_tracker = progress_tracker
        self.openrouter_client = openrouter_client
        self.reporter = reporter
        self.results_writer = results_writer
        self.results_file = results_file

        # Research step configuration
        self.research_model = research_model or getattr(
            config, "CTV_RESEARCH_MODEL", "perplexity/sonar-pro"
        )
        self.research_temperature = research_temperature
        self.research_max_tokens = research_max_tokens

        # Classification step configuration
        self.classification_model = classification_model or getattr(
            config, "CTV_CLASSIFICATION_MODEL", "~google/gemini-flash-latest"
        )
        self.classification_temperature = classification_temperature
        self.classification_max_tokens = classification_max_tokens

        # Rate limiting
        self.request_delay = request_delay
        self._last_request_time = 0.0
        self._request_lock = asyncio.Lock()

    async def _apply_rate_limit(self) -> None:
        """Apply rate limiting between requests."""
        async with self._request_lock:
            now = time.time()
            elapsed = now - self._last_request_time
            if elapsed < self.request_delay:
                await asyncio.sleep(self.request_delay - elapsed)
            self._last_request_time = time.time()

    async def process_ctv_app(self, item: CTVWorkItem) -> None:
        """
        Process a single CTV app through the complete two-step workflow.

        This method implements separate error handling for the research and
        classification steps, providing granular error messages in the output.

        Args:
            item: CTVWorkItem containing the CTV app information
        """
        start_time = time.perf_counter()
        research_result: Dict[str, Any] = {}
        classification_result: Dict[str, Any] = {}

        try:
            logger.debug("Processing CTV app: %s", item.app_name)

            # Step 1: Research the CTV app
            try:
                await self._apply_rate_limit()
                research_result = await self._research_step(item)

                if not research_result.get("success"):
                    raise RuntimeError(
                        f"Research failed: {research_result.get('error', 'Unknown error')}"
                    )

            except Exception as research_error:
                # Handle research step failure specifically
                processing_time = time.perf_counter() - start_time
                error_message = f"Research failed: {research_error}"

                logger.error("CTV research failed for %s: %s", item.app_name, research_error)

                await self._record_ctv_failure(
                    item=item,
                    error_message=error_message,
                    processing_time=processing_time,
                    research_result=research_result,
                )

                if self.reporter:
                    short_error = shorten_error(str(research_error))
                    await self.reporter.log_attempt(
                        domain=f"CTV:{item.app_name}",
                        ok=False,
                        status_code=None,
                        error=f"Research: {short_error}",
                        elapsed=processing_time,
                        size_kb=None,
                        retries_used=0,
                        cache_status=None,
                    )
                return

            # Step 2: Classify the CTV app based on research
            try:
                await self._apply_rate_limit()
                classification_result = await self._classification_step(
                    item=item,
                    research_content=research_result.get("research_content", ""),
                )

                if not classification_result.get("success"):
                    raise RuntimeError(
                        f"Classification failed: {classification_result.get('error', 'Unknown error')}"
                    )

            except Exception as classification_error:
                # Handle classification step failure specifically
                processing_time = time.perf_counter() - start_time
                error_message = f"Classification failed: {classification_error}"

                logger.error("CTV classification failed for %s: %s", item.app_name, classification_error)

                await self._record_ctv_failure(
                    item=item,
                    error_message=error_message,
                    processing_time=processing_time,
                    research_result=research_result,
                )

                if self.reporter:
                    short_error = shorten_error(str(classification_error))
                    await self.reporter.log_attempt(
                        domain=f"CTV:{item.app_name}",
                        ok=False,
                        status_code=None,
                        error=f"Classification: {short_error}",
                        elapsed=processing_time,
                        size_kb=None,
                        retries_used=0,
                        cache_status=None,
                    )
                return

            # Both steps succeeded - record success
            processing_time = time.perf_counter() - start_time
            await self._record_ctv_success(
                item=item,
                research_result=research_result,
                classification_result=classification_result,
                processing_time=processing_time,
            )

            if self.reporter:
                await self.reporter.log_attempt(
                    domain=f"CTV:{item.app_name}",
                    ok=True,
                    status_code=200,
                    error=None,
                    elapsed=processing_time,
                    size_kb=len(research_result.get("research_content", "")) / 1024,
                    retries_used=0,
                    cache_status="miss",
                )

        except Exception as e:
            # Catch any unexpected errors
            processing_time = time.perf_counter() - start_time
            error_message = str(e)

            logger.error("Unexpected error processing CTV app %s: %s", item.app_name, error_message)

            await self._record_ctv_failure(
                item=item,
                error_message=f"Unexpected error: {error_message}",
                processing_time=processing_time,
                research_result=research_result,
            )

            if self.reporter:
                short_error = shorten_error(error_message)
                await self.reporter.log_attempt(
                    domain=f"CTV:{item.app_name}",
                    ok=False,
                    status_code=None,
                    error=short_error,
                    elapsed=processing_time,
                    size_kb=None,
                    retries_used=0,
                    cache_status=None,
                )

    async def _research_step(self, item: CTVWorkItem) -> Dict[str, Any]:
        """
        Execute the research step of the CTV processing pipeline.

        Uses Perplexity Sonar Pro to gather detailed information about the CTV app.

        Args:
            item: CTVWorkItem containing the CTV app information

        Returns:
            Dictionary containing research results
        """
        logger.debug("Starting research step for CTV app: %s", item.app_name)

        return await self.openrouter_client.research_ctv_app(
            app_name=item.app_name,
            bundle_id=item.bundle_id or "",
            platform=item.platform or "",
            url=item.url or "",
            research_model=self.research_model,
            temperature=self.research_temperature,
            max_tokens=self.research_max_tokens,
        )

    async def _classification_step(
        self,
        item: CTVWorkItem,
        research_content: str,
    ) -> Dict[str, Any]:
        """
        Execute the classification step of the CTV processing pipeline.

        Uses a fast classification model to structure the research into
        a consistent format.

        Args:
            item: CTVWorkItem containing the CTV app information
            research_content: Research content from the first step

        Returns:
            Dictionary containing classification results
        """
        logger.debug("Starting classification step for CTV app: %s", item.app_name)

        return await self.openrouter_client.classify_ctv_app(
            app_name=item.app_name,
            research_content=research_content,
            bundle_id=item.bundle_id or "",
            platform=item.platform or "",
            url=item.url or "",
            classification_model=self.classification_model,
            temperature=self.classification_temperature,
            max_tokens=self.classification_max_tokens,
        )

    async def _record_ctv_success(
        self,
        item: CTVWorkItem,
        research_result: Dict[str, Any],
        classification_result: Dict[str, Any],
        processing_time: float,
    ) -> None:
        """Record a successful CTV app processing result."""
        record = {
            "App_Name": item.app_name,
            "Type": "CTV",
            "Bundle_ID": item.bundle_id or "",
            "SSP": getattr(item, 'ssp', '') or "",
            "Publisher": getattr(item, 'publisher', '') or "",
            "Platform": item.platform or self._detect_ctv_platform(item.bundle_id),
            "URL": item.url or "",
            "Quality": classification_result["quality"],
            "Justification": classification_result["justification"],
            "IAB Tier 1": classification_result.get("vertical_tier_1", ""),
            "IAB Tier 2": classification_result.get("vertical_tier_2", ""),
            "IAB Tier 3": classification_result.get("vertical_tier_3", ""),
            "Description": classification_result.get("description", ""),
            "Target_Audience": classification_result.get("target_audience", ""),
            "Content_Type": classification_result.get("content_type", ""),
            "Language": classification_result.get("language", "Unknown"),
            "Political_Leaning": classification_result.get("political_leaning", "Non-Political"),
            "Network_Affiliation": classification_result.get("network_affiliation", ""),
            "Audience_Size": classification_result.get("audience_size", "Unknown"),
            "Research_Summary": self._truncate_research(self._strip_markdown(research_result.get("research_content", ""))),
            "Processing_Time": round(processing_time, 2),
            "Research_Model": self.research_model,
            "Classification_Model": self.classification_model,
            "Processed_At": datetime.now().isoformat(),
        }

        self._write_result(record)

        await self.progress_tracker.mark_domain_processed(
            domain=item.identifier,
            status="success",
            result=record,
            processing_time=processing_time,
        )

        logger.debug(
            "Processed CTV app: %s -> %s (%s)",
            item.app_name,
            classification_result["quality"],
            classification_result.get("content_type", "Unknown"),
        )

    async def _record_ctv_failure(
        self,
        item: CTVWorkItem,
        error_message: str,
        processing_time: float,
        research_result: Optional[Dict[str, Any]] = None,
    ) -> None:
        """Record a failed CTV app processing result."""
        record = {
            "App_Name": item.app_name,
            "Type": "CTV",
            "Bundle_ID": item.bundle_id or "",
            "SSP": getattr(item, 'ssp', '') or "",
            "Publisher": getattr(item, 'publisher', '') or "",
            "Platform": item.platform or "",
            "URL": item.url or "",
            "Quality": "Failed",
            "Justification": error_message,
            "IAB Tier 1": "N/A",
            "IAB Tier 2": "N/A",
            "IAB Tier 3": "N/A",
            "Description": "N/A",
            "Target_Audience": "N/A",
            "Content_Type": "N/A",
            "Language": "Unknown",
            "Political_Leaning": "Non-Political",
            "Network_Affiliation": "N/A",
            "Audience_Size": "Unknown",
            "Research_Summary": self._truncate_research(
                self._strip_markdown(research_result.get("research_content", "") if research_result else "")
            ),
            "Processing_Time": round(processing_time, 2),
            "Research_Model": self.research_model,
            "Classification_Model": self.classification_model,
            "Processed_At": datetime.now().isoformat(),
        }

        self._write_result(record)

        await self.progress_tracker.mark_domain_processed(
            domain=item.identifier,
            status="error",
            error_message=error_message,
            processing_time=processing_time,
        )

    def _write_result(self, record: Dict[str, Any]) -> None:
        """Persist a result row to the output CSV."""
        if not self.results_writer or not self.results_file:
            raise RuntimeError("Results writer has not been initialised")

        self.results_writer.writerow(record)
        self.results_file.flush()

    @staticmethod
    def _strip_markdown(text: str) -> str:
        """
        Remove common Markdown formatting from text.

        Strips:
        - Header markers (# ## ### etc.)
        - Bold/italic markers (* ** _ __)
        - Inline code backticks
        - Link syntax [text](url) -> text
        - Citation markers like [1], [2]
        - Multiple newlines collapsed to single newlines

        Args:
            text: Raw text potentially containing Markdown formatting

        Returns:
            Cleaned text with Markdown formatting removed
        """
        import re

        if not text:
            return text

        result = text

        # Remove header markers (# ## ### etc.) at the start of lines
        result = re.sub(r'^#{1,6}\s+', '', result, flags=re.MULTILINE)

        # Remove link syntax but keep link text: [text](url) -> text
        result = re.sub(r'\[([^\]]+)\]\([^)]+\)', r'\1', result)

        # Remove citation markers like [1], [2], [3] etc.
        result = re.sub(r'\[\d+\]', '', result)

        # Remove bold markers ** and __
        result = re.sub(r'\*\*([^*]+)\*\*', r'\1', result)
        result = re.sub(r'__([^_]+)__', r'\1', result)

        # Remove italic markers * and _ (single)
        # Be careful not to match multiple asterisks
        result = re.sub(r'(?<!\*)\*([^*]+)\*(?!\*)', r'\1', result)
        result = re.sub(r'(?<!_)_([^_]+)_(?!_)', r'\1', result)

        # Remove inline code backticks
        result = re.sub(r'`([^`]+)`', r'\1', result)

        # Collapse multiple newlines into single newlines
        result = re.sub(r'\n{3,}', '\n\n', result)

        # Clean up any extra spaces/tabs at line beginnings (from removed headers)
        # Use [ \t]+ instead of \s+ to preserve newlines
        result = re.sub(r'^[ \t]+', '', result, flags=re.MULTILINE)

        return result.strip()

    @staticmethod
    def _truncate_research(research_content: str, max_length: int = 1000) -> str:
        """
        Truncate research content for storage in CSV.

        Attempts to avoid cutting off mid-sentence by finding the last
        sentence boundary (. ! ?) before the max_length limit.

        Args:
            research_content: The research content to truncate
            max_length: Maximum allowed length (default 1000)

        Returns:
            Truncated content, ending at a sentence boundary if possible
        """
        if len(research_content) <= max_length:
            return research_content

        # Look for the last sentence boundary before max_length
        # Allow some margin to find a boundary (check up to max_length chars)
        truncated = research_content[:max_length]

        # Find the last sentence-ending punctuation followed by a space or end
        import re
        # Match sentence endings: . ! ? followed by space, newline, or end
        sentence_end_pattern = r'[.!?](?=\s|$)'
        matches = list(re.finditer(sentence_end_pattern, truncated))

        if matches:
            # Use the last sentence boundary found
            last_match = matches[-1]
            # Include the punctuation mark
            cut_point = last_match.end()
            # Only use this cut point if it's reasonable (at least 50% of max_length)
            if cut_point >= max_length * 0.5:
                return research_content[:cut_point].strip()

        # If no good sentence boundary found, fall back to simple truncation
        return truncated.strip() + "..."

    @staticmethod
    def _detect_ctv_platform(bundle_id: str) -> str:
        """
        Detect the CTV platform based on bundle ID patterns.

        Args:
            bundle_id: The bundle ID string to analyze

        Returns:
            Detected platform name or "Unknown" if no pattern matches
        """
        if not bundle_id:
            return "Unknown"

        bundle_id_stripped = bundle_id.strip()
        if not bundle_id_stripped:
            return "Unknown"

        # Numeric only (e.g., "54092") = Roku
        if bundle_id_stripped.isdigit():
            return "Roku"

        # Starts with B0 or b0 (e.g., "B091RFCS5V") = Fire TV
        if bundle_id_stripped.upper().startswith("B0"):
            return "Fire TV"

        # Starts with "com." (e.g., "com.foxsports.videogo") = Android TV
        if bundle_id_stripped.lower().startswith("com."):
            return "Android TV"

        # Starts with "vizio." = Vizio
        if bundle_id_stripped.lower().startswith("vizio."):
            return "Vizio"

        # Starts with "tv." = Apple TV
        if bundle_id_stripped.lower().startswith("tv."):
            return "Apple TV"

        # Starts with G followed by numbers (e.g., "G00009197740") = Samsung TV
        if (
            bundle_id_stripped.upper().startswith("G")
            and len(bundle_id_stripped) > 1
            and bundle_id_stripped[1:].isdigit()
        ):
            return "Samsung TV"

        # Starts with 9, alphanumeric (e.g., "9wzdncrfjv7w") = Xbox
        if bundle_id_stripped.startswith("9") and bundle_id_stripped.isalnum():
            return "Xbox"

        return "Unknown"

    async def process_batch(
        self,
        items: List[CTVWorkItem],
        max_concurrent: int = 3,
    ) -> None:
        """
        Process a batch of CTV apps concurrently.

        Args:
            items: List of CTV work items to process
            max_concurrent: Maximum concurrent processing tasks
        """
        semaphore = asyncio.Semaphore(max_concurrent)

        async def process_with_semaphore(item: CTVWorkItem):
            async with semaphore:
                await self.process_ctv_app(item)

        tasks = [process_with_semaphore(item) for item in items]
        await asyncio.gather(*tasks, return_exceptions=True)
