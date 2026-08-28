# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""iOS App Store API client for elcano-lens.

This module provides functionality to fetch app metadata from the iTunes Search API.
API Documentation: https://developer.apple.com/library/archive/documentation/AudioVideo/Conceptual/iTuneSearchAPI/
"""

import asyncio
import logging
import time
from datetime import datetime
from typing import Any, Dict, List, Optional

import aiohttp

logger = logging.getLogger(__name__)


class iOSAPIClient:
    """Async client for fetching iOS app metadata from the iTunes Search API."""

    # iTunes Search API base URL
    BASE_URL = "https://itunes.apple.com/lookup"

    def __init__(
        self,
        *,
        request_timeout: int = 30,
        request_delay: float = 3.0,  # 3-second delay between requests
        max_concurrent: int = 3,  # Max 3 concurrent sessions
        max_retries: int = 3,
        retry_delay: float = 2.0,
    ) -> None:
        """
        Initialize the iOS API client.

        Args:
            request_timeout: Timeout for each request in seconds
            request_delay: Delay between requests in seconds (rate limiting)
            max_concurrent: Maximum number of concurrent requests
            max_retries: Maximum number of retry attempts
            retry_delay: Base delay between retries in seconds
        """
        self.request_timeout = request_timeout
        self.request_delay = request_delay
        self.max_concurrent = max_concurrent
        self.max_retries = max_retries
        self.retry_delay = retry_delay

        self._session: Optional[aiohttp.ClientSession] = None
        self._semaphore: Optional[asyncio.Semaphore] = None
        self._last_request_time: float = 0.0
        self._request_lock = asyncio.Lock()

        # Statistics
        self.request_count = 0
        self.success_count = 0
        self.error_count = 0

    async def __aenter__(self) -> "iOSAPIClient":
        """Async context manager entry."""
        timeout = aiohttp.ClientTimeout(total=self.request_timeout)
        self._session = aiohttp.ClientSession(timeout=timeout)
        self._semaphore = asyncio.Semaphore(self.max_concurrent)
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb) -> None:
        """Async context manager exit."""
        if self._session:
            await self._session.close()
            self._session = None

    async def _rate_limit(self) -> None:
        """Apply rate limiting between requests."""
        async with self._request_lock:
            now = time.time()
            elapsed = now - self._last_request_time
            if elapsed < self.request_delay:
                wait_time = self.request_delay - elapsed
                logger.debug("Rate limiting: waiting %.2f seconds", wait_time)
                await asyncio.sleep(wait_time)
            self._last_request_time = time.time()

    async def fetch_app_metadata(self, app_id: str) -> Dict[str, Any]:
        """
        Fetch metadata for a single iOS app.

        Args:
            app_id: The numeric App Store ID

        Returns:
            Dictionary containing app metadata or error information
        """
        if not self._session:
            raise RuntimeError("Client not initialized. Use async context manager.")

        if not self._semaphore:
            self._semaphore = asyncio.Semaphore(self.max_concurrent)

        async with self._semaphore:
            return await self._fetch_with_retry(app_id)

    async def _fetch_with_retry(self, app_id: str) -> Dict[str, Any]:
        """Fetch app metadata with retry logic."""
        start_time = time.time()
        last_error: Optional[str] = None

        for attempt in range(self.max_retries):
            try:
                # Apply rate limiting
                await self._rate_limit()

                self.request_count += 1
                result = await self._do_fetch(app_id)

                if result.get("success"):
                    self.success_count += 1
                    result["processing_time"] = time.time() - start_time
                    return result
                else:
                    last_error = result.get("error", "Unknown error")
                    logger.warning(
                        "Attempt %d/%d for app %s failed: %s",
                        attempt + 1,
                        self.max_retries,
                        app_id,
                        last_error,
                    )

            except Exception as exc:
                last_error = str(exc)
                logger.warning(
                    "Attempt %d/%d for app %s raised exception: %s",
                    attempt + 1,
                    self.max_retries,
                    app_id,
                    exc,
                )

            # Wait before retry (exponential backoff)
            if attempt < self.max_retries - 1:
                wait_time = self.retry_delay * (2**attempt)
                await asyncio.sleep(wait_time)

        self.error_count += 1
        return {
            "success": False,
            "app_id": app_id,
            "error": last_error or "Max retries exceeded",
            "processing_time": time.time() - start_time,
            "fetched_at": datetime.now().isoformat(),
        }

    async def _do_fetch(self, app_id: str) -> Dict[str, Any]:
        """Perform the actual API request."""
        url = f"{self.BASE_URL}?id={app_id}"
        fetched_at = datetime.now().isoformat()

        try:
            async with self._session.get(url) as response:
                if response.status != 200:
                    return {
                        "success": False,
                        "app_id": app_id,
                        "error": f"HTTP {response.status}",
                        "fetched_at": fetched_at,
                    }

                # iTunes API returns text/javascript instead of application/json
                # We need to explicitly tell aiohttp to parse it as JSON
                data = await response.json(content_type=None)

                # Check if we got results
                result_count = data.get("resultCount", 0)
                if result_count == 0:
                    return {
                        "success": False,
                        "app_id": app_id,
                        "error": "App not found in App Store",
                        "fetched_at": fetched_at,
                    }

                # Extract the first result (should be the only one for ID lookup)
                app_data = data.get("results", [{}])[0]

                return self._parse_app_data(app_id, app_data, fetched_at)

        except aiohttp.ClientError as exc:
            return {
                "success": False,
                "app_id": app_id,
                "error": f"Network error: {exc}",
                "fetched_at": fetched_at,
            }
        except Exception as exc:
            return {
                "success": False,
                "app_id": app_id,
                "error": f"Unexpected error: {exc}",
                "fetched_at": fetched_at,
            }

    def _parse_app_data(
        self, app_id: str, app_data: Dict[str, Any], fetched_at: str
    ) -> Dict[str, Any]:
        """Parse the API response into a standardized format."""
        try:
            # Extract key fields
            app_name = app_data.get("trackName", "")
            developer = app_data.get("artistName", "")
            bundle_id = app_data.get("bundleId", "")

            # Category information
            primary_genre = app_data.get("primaryGenreName", "")
            genres = app_data.get("genres", [])

            # Ratings and reviews
            rating = app_data.get("averageUserRating", 0.0)
            rating_count = app_data.get("userRatingCount", 0)

            # App description
            description = app_data.get("description", "")

            # Additional metadata
            price = app_data.get("price", 0.0)
            currency = app_data.get("currency", "USD")
            content_rating = app_data.get("contentAdvisoryRating", "")

            # Version and update info
            version = app_data.get("version", "")
            release_date = app_data.get("releaseDate", "")
            current_version_release_date = app_data.get("currentVersionReleaseDate", "")

            # URLs
            app_store_url = app_data.get("trackViewUrl", "")
            icon_url = app_data.get("artworkUrl512", app_data.get("artworkUrl100", ""))

            # Size
            file_size_bytes = app_data.get("fileSizeBytes", "0")
            try:
                file_size_mb = int(file_size_bytes) / (1024 * 1024)
            except (ValueError, TypeError):
                file_size_mb = 0.0

            # Supported devices
            supported_devices = app_data.get("supportedDevices", [])

            # Build content for LLM classification
            content_for_llm = self._build_content_for_llm(
                app_name=app_name,
                developer=developer,
                primary_genre=primary_genre,
                genres=genres,
                description=description,
                rating=rating,
                rating_count=rating_count,
                content_rating=content_rating,
            )

            return {
                "success": True,
                "app_id": app_id,
                "bundle_id": bundle_id,
                "app_name": app_name,
                "developer": developer,
                "primary_genre": primary_genre,
                "genres": genres,
                "rating": round(rating, 2) if rating else 0.0,
                "rating_count": rating_count,
                "description": description,
                "content_rating": content_rating,
                "price": price,
                "currency": currency,
                "version": version,
                "release_date": release_date,
                "last_updated": current_version_release_date,
                "app_store_url": app_store_url,
                "icon_url": icon_url,
                "file_size_mb": round(file_size_mb, 2),
                "supported_devices": supported_devices,
                "content_for_llm": content_for_llm,
                "fetched_at": fetched_at,
                "platform": "ios",
            }

        except Exception as exc:
            logger.error("Error parsing app data for %s: %s", app_id, exc)
            return {
                "success": False,
                "app_id": app_id,
                "error": f"Error parsing response: {exc}",
                "fetched_at": fetched_at,
            }

    def _build_content_for_llm(
        self,
        app_name: str,
        developer: str,
        primary_genre: str,
        genres: List[str],
        description: str,
        rating: float,
        rating_count: int,
        content_rating: str,
    ) -> str:
        """Build a text content block for LLM classification."""
        # Truncate description if too long
        max_desc_length = 2000
        if len(description) > max_desc_length:
            description = description[:max_desc_length] + "..."

        content_parts = [
            f"App Name: {app_name}",
            f"Developer: {developer}",
            f"Primary Category: {primary_genre}",
        ]

        if genres:
            content_parts.append(f"All Categories: {', '.join(genres)}")

        if rating and rating_count:
            content_parts.append(f"Rating: {rating:.1f}/5.0 ({rating_count:,} reviews)")

        if content_rating:
            content_parts.append(f"Content Rating: {content_rating}")

        if description:
            content_parts.append(f"\nDescription:\n{description}")

        return "\n".join(content_parts)

    async def fetch_batch(self, app_ids: List[str]) -> List[Dict[str, Any]]:
        """
        Fetch metadata for multiple iOS apps.

        Args:
            app_ids: List of App Store IDs

        Returns:
            List of metadata dictionaries
        """
        tasks = [self.fetch_app_metadata(app_id) for app_id in app_ids]
        return await asyncio.gather(*tasks, return_exceptions=False)

    def get_stats(self) -> Dict[str, Any]:
        """Get client statistics."""
        return {
            "request_count": self.request_count,
            "success_count": self.success_count,
            "error_count": self.error_count,
            "success_rate": (
                self.success_count / self.request_count * 100 if self.request_count > 0 else 0.0
            ),
        }
