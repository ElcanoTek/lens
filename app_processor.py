# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""App processor module for elcano-lens.

This module handles the processing of iOS and Android apps, including:
- Fetching app metadata (iOS via API, Android via scraping)
- Sending app data to LLM for classification
- Recording results
"""

import logging
import time
from datetime import datetime
from typing import Any, Dict, Optional, TextIO

from ios_api_client import iOSAPIClient
from android_scraper import AndroidScraper
from openrouter_client import OpenRouterClient
from progress_tracker import ProgressTracker
from reporting import TerminalReporter, shorten_error
from shared_types import WorkItem, ContentType

logger = logging.getLogger(__name__)


class AppProcessor:
    """Processor for iOS and Android apps."""

    def __init__(
        self,
        *,
        progress_tracker: ProgressTracker,
        ios_client: Optional[iOSAPIClient],
        android_scraper: Optional[AndroidScraper],
        openrouter_client: OpenRouterClient,
        reporter: Optional[TerminalReporter],
        results_writer,
        results_file: Optional[TextIO],
    ):
        """
        Initialize the app processor.

        Args:
            progress_tracker: Progress tracking instance
            ios_client: iOS API client instance
            android_scraper: Android scraper instance
            openrouter_client: OpenRouter client for LLM classification
            reporter: Terminal reporter instance
            results_writer: CSV writer for results
            results_file: File handle for results
        """
        self.progress_tracker = progress_tracker
        self.ios_client = ios_client
        self.android_scraper = android_scraper
        self.openrouter_client = openrouter_client
        self.reporter = reporter
        self.results_writer = results_writer
        self.results_file = results_file

    async def process_ios_app(self, item: "WorkItem") -> None:
        """
        Process a single iOS app through the complete workflow.

        Args:
            item: Work item containing the iOS app ID
        """
        start_time = time.perf_counter()
        app_data: Dict[str, Any] = {}
        classification_result: Dict[str, Any] = {}

        try:
            logger.debug("Processing iOS app: %s", item.identifier)

            if not self.ios_client:
                raise RuntimeError("iOS client not initialized")

            # Fetch app metadata from iTunes API
            app_data = await self.ios_client.fetch_app_metadata(item.identifier)

            if not app_data.get("success"):
                raise RuntimeError(
                    f"Failed to fetch iOS app data: {app_data.get('error', 'Unknown error')}"
                )

            # Classify the app using LLM
            classification_result = await self.openrouter_client.classify_app(
                app_id=item.identifier,
                app_name=app_data.get("app_name", ""),
                developer=app_data.get("developer", ""),
                store_category=app_data.get("primary_genre", ""),
                description=app_data.get("description", ""),
                rating=app_data.get("rating", 0.0),
                rating_count=app_data.get("rating_count", 0),
                content_for_llm=app_data.get("content_for_llm", ""),
                platform="ios",
            )

            if not classification_result.get("success"):
                raise RuntimeError(
                    f"Classification failed: {classification_result.get('error', 'Unknown error')}"
                )

            processing_time = time.perf_counter() - start_time
            await self._record_app_success(
                item=item,
                app_data=app_data,
                classification_result=classification_result,
                processing_time=processing_time,
                platform="ios",
            )

            if self.reporter:
                await self.reporter.log_attempt(
                    domain=f"iOS:{item.identifier}",
                    ok=True,
                    status_code=200,
                    error=None,
                    elapsed=processing_time,
                    size_kb=len(app_data.get("content_for_llm", "")) / 1024,
                    retries_used=0,
                    cache_status="miss",
                )

        except Exception as e:
            processing_time = time.perf_counter() - start_time
            error_message = str(e)

            logger.error("Failed to process iOS app %s: %s", item.identifier, error_message)

            await self._record_app_failure(
                item=item,
                error_message=error_message,
                processing_time=processing_time,
                platform="ios",
                fetched_at=app_data.get("fetched_at", datetime.now().isoformat()),
            )

            if self.reporter:
                short_error = shorten_error(error_message)
                await self.reporter.log_attempt(
                    domain=f"iOS:{item.identifier}",
                    ok=False,
                    status_code=None,
                    error=short_error,
                    elapsed=processing_time,
                    size_kb=None,
                    retries_used=0,
                    cache_status=None,
                )

    async def process_android_app(self, item: "WorkItem") -> None:
        """
        Process a single Android app through the complete workflow.

        Args:
            item: Work item containing the Android package name
        """
        start_time = time.perf_counter()
        app_data: Dict[str, Any] = {}
        classification_result: Dict[str, Any] = {}

        try:
            logger.debug("Processing Android app: %s", item.identifier)

            if not self.android_scraper:
                raise RuntimeError("Android scraper not initialized")

            # Fetch app metadata from Play Store
            app_data = await self.android_scraper.fetch_app_metadata(item.identifier)

            if not app_data.get("success"):
                raise RuntimeError(
                    f"Failed to fetch Android app data: {app_data.get('error', 'Unknown error')}"
                )

            # Classify the app using LLM
            classification_result = await self.openrouter_client.classify_app(
                app_id=item.identifier,
                app_name=app_data.get("app_name", ""),
                developer=app_data.get("developer", ""),
                store_category=app_data.get("category", ""),
                description=app_data.get("description", ""),
                rating=app_data.get("rating", 0.0),
                rating_count=app_data.get("rating_count", 0),
                downloads=app_data.get("downloads", ""),
                content_for_llm=app_data.get("content_for_llm", ""),
                platform="android",
            )

            if not classification_result.get("success"):
                raise RuntimeError(
                    f"Classification failed: {classification_result.get('error', 'Unknown error')}"
                )

            processing_time = time.perf_counter() - start_time
            await self._record_app_success(
                item=item,
                app_data=app_data,
                classification_result=classification_result,
                processing_time=processing_time,
                platform="android",
            )

            if self.reporter:
                await self.reporter.log_attempt(
                    domain=f"Android:{item.identifier}",
                    ok=True,
                    status_code=200,
                    error=None,
                    elapsed=processing_time,
                    size_kb=len(app_data.get("content_for_llm", "")) / 1024,
                    retries_used=0,
                    cache_status="miss",
                )

        except Exception as e:
            processing_time = time.perf_counter() - start_time
            error_message = str(e)

            logger.error("Failed to process Android app %s: %s", item.identifier, error_message)

            await self._record_app_failure(
                item=item,
                error_message=error_message,
                processing_time=processing_time,
                platform="android",
                fetched_at=app_data.get("fetched_at", datetime.now().isoformat()),
            )

            if self.reporter:
                short_error = shorten_error(error_message)
                await self.reporter.log_attempt(
                    domain=f"Android:{item.identifier}",
                    ok=False,
                    status_code=None,
                    error=short_error,
                    elapsed=processing_time,
                    size_kb=None,
                    retries_used=0,
                    cache_status=None,
                )

    async def process_app(self, item: "WorkItem") -> None:
        """
        Process an app based on its content type.

        Args:
            item: Work item containing the app identifier and type
        """
        if item.content_type == ContentType.IOS_APP:
            await self.process_ios_app(item)
        elif item.content_type == ContentType.ANDROID_APP:
            await self.process_android_app(item)
        else:
            raise ValueError(f"Invalid content type for app processing: {item.content_type}")

    async def _record_app_success(
        self,
        item: "WorkItem",
        app_data: Dict[str, Any],
        classification_result: Dict[str, Any],
        processing_time: float,
        platform: str,
    ) -> None:
        """Record a successful app processing result."""
        # Determine the identifier column based on platform
        identifier = item.identifier

        # Get app name for display
        app_name = app_data.get("app_name", identifier)

        record = {
            "Domain": identifier,  # Using Domain column for compatibility
            "Type": platform.upper(),
            "App_Name": app_name,
            "Developer": app_data.get("developer", ""),
            "Store_Category": app_data.get("primary_genre") or app_data.get("category", ""),
            "Rating": app_data.get("rating", ""),
            "Rating_Count": app_data.get("rating_count", ""),
            "Downloads": app_data.get("downloads", "") if platform == "android" else "",
            "Quality": classification_result["quality"],
            "Justification": classification_result["justification"],
            "IAB Tier 1": classification_result.get("vertical_tier_1", ""),
            "IAB Tier 2": classification_result.get("vertical_tier_2", ""),
            "IAB Tier 3": classification_result.get("vertical_tier_3", ""),
            "Description": classification_result.get("description", ""),
            "Language": classification_result.get("language", "Unknown"),
            "Political_Leaning": classification_result.get("political_leaning", "Non-Political"),
            "Audience_Size": classification_result.get("audience_size", "Unknown"),
            "Bot_Protection": "",  # site-level signal; not applicable to store apps
            "Content_Length": len(app_data.get("content_for_llm", "")),
            "Processing_Time": round(processing_time, 2),
            "Scrape_Mode": f"{platform}_api" if platform == "ios" else f"{platform}_scrape",
            "Classifier_Mode": "openrouter",
            "Scraped_At": app_data.get("fetched_at", datetime.now().isoformat()),
        }

        self._write_result(record)

        await self.progress_tracker.mark_domain_processed(
            domain=identifier,
            status="success",
            result=record,
            processing_time=processing_time,
        )

        logger.debug("Processed %s app: %s → %s", platform.upper(), app_name, classification_result["quality"])

    async def _record_app_failure(
        self,
        item: "WorkItem",
        error_message: str,
        processing_time: float,
        platform: str,
        fetched_at: str,
    ) -> None:
        """Record a failed app processing result."""
        record = {
            "Domain": item.identifier,
            "Type": platform.upper(),
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
            "Bot_Protection": "",
            "Content_Length": 0,
            "Processing_Time": round(processing_time, 2),
            "Scrape_Mode": f"{platform}_api" if platform == "ios" else f"{platform}_scrape",
            "Classifier_Mode": "openrouter",
            "Scraped_At": fetched_at,
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
