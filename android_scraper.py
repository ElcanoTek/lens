# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Android Play Store scraper for elcano-lens.

This module provides functionality to scrape app metadata from the Google Play Store.
Uses similar anti-blocking patterns as the website scraper.
"""

import asyncio
import logging
import re
import time
from datetime import datetime
from typing import Any, Dict, List, Optional

import aiohttp
from bs4 import BeautifulSoup

logger = logging.getLogger(__name__)


class AndroidScraper:
    """Async scraper for fetching Android app metadata from Google Play Store."""

    # Google Play Store base URL
    BASE_URL = "https://play.google.com/store/apps/details"

    # User agent to mimic a real browser
    DEFAULT_USER_AGENT = (
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
        "(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
    )

    def __init__(
        self,
        *,
        request_timeout: int = 30,
        request_delay: float = 4.0,  # 4-second delay between requests
        max_concurrent: int = 3,  # Max 3 concurrent sessions
        max_retries: int = 3,
        retry_delay: float = 2.0,
        user_agent: Optional[str] = None,
    ) -> None:
        """
        Initialize the Android scraper.

        Args:
            request_timeout: Timeout for each request in seconds
            request_delay: Delay between requests in seconds (rate limiting)
            max_concurrent: Maximum number of concurrent requests
            max_retries: Maximum number of retry attempts
            retry_delay: Base delay between retries in seconds
            user_agent: Custom user agent string
        """
        self.request_timeout = request_timeout
        self.request_delay = request_delay
        self.max_concurrent = max_concurrent
        self.max_retries = max_retries
        self.retry_delay = retry_delay
        self.user_agent = user_agent or self.DEFAULT_USER_AGENT

        self._session: Optional[aiohttp.ClientSession] = None
        self._semaphore: Optional[asyncio.Semaphore] = None
        self._last_request_time: float = 0.0
        self._request_lock = asyncio.Lock()

        # Statistics
        self.request_count = 0
        self.success_count = 0
        self.error_count = 0

    async def __aenter__(self) -> "AndroidScraper":
        """Async context manager entry."""
        timeout = aiohttp.ClientTimeout(total=self.request_timeout)
        headers = {
            "User-Agent": self.user_agent,
            "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
            "Accept-Language": "en-US,en;q=0.5",
            "Accept-Encoding": "gzip, deflate, br",
            "DNT": "1",
            "Connection": "keep-alive",
            "Upgrade-Insecure-Requests": "1",
        }
        self._session = aiohttp.ClientSession(timeout=timeout, headers=headers)
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

    async def fetch_app_metadata(self, package_name: str) -> Dict[str, Any]:
        """
        Fetch metadata for a single Android app.

        Args:
            package_name: The Android package name (e.g., com.instagram.android)

        Returns:
            Dictionary containing app metadata or error information
        """
        if not self._session:
            raise RuntimeError("Client not initialized. Use async context manager.")

        if not self._semaphore:
            self._semaphore = asyncio.Semaphore(self.max_concurrent)

        async with self._semaphore:
            return await self._fetch_with_retry(package_name)

    async def _fetch_with_retry(self, package_name: str) -> Dict[str, Any]:
        """Fetch app metadata with retry logic."""
        start_time = time.time()
        last_error: Optional[str] = None

        for attempt in range(self.max_retries):
            try:
                # Apply rate limiting
                await self._rate_limit()

                self.request_count += 1
                result = await self._do_fetch(package_name)

                if result.get("success"):
                    self.success_count += 1
                    result["processing_time"] = time.time() - start_time
                    return result
                else:
                    last_error = result.get("error", "Unknown error")
                    logger.warning(
                        "Attempt %d/%d for package %s failed: %s",
                        attempt + 1, self.max_retries, package_name, last_error
                    )

            except Exception as exc:
                last_error = str(exc)
                logger.warning(
                    "Attempt %d/%d for package %s raised exception: %s",
                    attempt + 1, self.max_retries, package_name, exc
                )

            # Wait before retry (exponential backoff)
            if attempt < self.max_retries - 1:
                wait_time = self.retry_delay * (2 ** attempt)
                await asyncio.sleep(wait_time)

        self.error_count += 1
        return {
            "success": False,
            "package_name": package_name,
            "error": last_error or "Max retries exceeded",
            "processing_time": time.time() - start_time,
            "fetched_at": datetime.now().isoformat(),
        }

    async def _do_fetch(self, package_name: str) -> Dict[str, Any]:
        """Perform the actual scraping request."""
        url = f"{self.BASE_URL}?id={package_name}&hl=en&gl=US"
        fetched_at = datetime.now().isoformat()

        try:
            async with self._session.get(url) as response:
                if response.status == 404:
                    return {
                        "success": False,
                        "package_name": package_name,
                        "error": "App not found on Play Store",
                        "fetched_at": fetched_at,
                    }

                if response.status != 200:
                    return {
                        "success": False,
                        "package_name": package_name,
                        "error": f"HTTP {response.status}",
                        "fetched_at": fetched_at,
                    }

                html = await response.text()

                # Check for block page
                if self._is_blocked(html):
                    return {
                        "success": False,
                        "package_name": package_name,
                        "error": "Blocked by Google Play Store",
                        "fetched_at": fetched_at,
                    }

                return self._parse_html(package_name, html, fetched_at)

        except aiohttp.ClientError as exc:
            return {
                "success": False,
                "package_name": package_name,
                "error": f"Network error: {exc}",
                "fetched_at": fetched_at,
            }
        except Exception as exc:
            return {
                "success": False,
                "package_name": package_name,
                "error": f"Unexpected error: {exc}",
                "fetched_at": fetched_at,
            }

    def _is_blocked(self, html: str) -> bool:
        """Check if the response is a block page."""
        lower_html = html.lower()[:1000]
        block_phrases = (
            "captcha",
            "unusual traffic",
            "automated queries",
            "please try again",
            "access denied",
            "we're sorry",
        )
        return any(phrase in lower_html for phrase in block_phrases)

    def _parse_html(
        self, package_name: str, html: str, fetched_at: str
    ) -> Dict[str, Any]:
        """Parse the Play Store HTML page to extract app metadata."""
        try:
            soup = BeautifulSoup(html, "html.parser")

            # Extract app name from title or h1
            app_name = self._extract_app_name(soup)
            if not app_name:
                return {
                    "success": False,
                    "package_name": package_name,
                    "error": "Could not extract app name - page might be blocked or app not found",
                    "fetched_at": fetched_at,
                }

            # Extract developer name
            developer = self._extract_developer(soup)

            # Extract category
            category = self._extract_category(soup)

            # Extract rating
            rating, rating_count = self._extract_rating(soup)

            # Extract downloads
            downloads = self._extract_downloads(soup)

            # Extract description
            description = self._extract_description(soup)

            # Extract last updated date
            last_updated = self._extract_last_updated(soup)

            # Extract content rating
            content_rating = self._extract_content_rating(soup)

            # Build content for LLM
            content_for_llm = self._build_content_for_llm(
                app_name=app_name,
                developer=developer,
                category=category,
                description=description,
                rating=rating,
                rating_count=rating_count,
                downloads=downloads,
                content_rating=content_rating,
            )

            return {
                "success": True,
                "package_name": package_name,
                "app_name": app_name,
                "developer": developer,
                "category": category,
                "rating": rating,
                "rating_count": rating_count,
                "downloads": downloads,
                "description": description,
                "content_rating": content_rating,
                "last_updated": last_updated,
                "play_store_url": f"{self.BASE_URL}?id={package_name}",
                "content_for_llm": content_for_llm,
                "fetched_at": fetched_at,
                "platform": "android",
            }

        except Exception as exc:
            logger.error("Error parsing HTML for %s: %s", package_name, exc)
            return {
                "success": False,
                "package_name": package_name,
                "error": f"Error parsing page: {exc}",
                "fetched_at": fetched_at,
            }

    def _extract_app_name(self, soup: BeautifulSoup) -> str:
        """Extract the app name from the page."""
        # Try the main h1 title
        h1 = soup.find("h1", itemprop="name")
        if h1:
            span = h1.find("span")
            if span:
                return span.get_text(strip=True)
            return h1.get_text(strip=True)

        # Try meta title
        title_tag = soup.find("title")
        if title_tag:
            title = title_tag.get_text(strip=True)
            # Remove "- Apps on Google Play" suffix
            if " - Apps on Google Play" in title:
                return title.replace(" - Apps on Google Play", "").strip()
            return title

        # Try og:title meta
        og_title = soup.find("meta", property="og:title")
        if og_title and og_title.get("content"):
            return og_title["content"]

        return ""

    def _extract_developer(self, soup: BeautifulSoup) -> str:
        """Extract the developer name."""
        # Try the developer link
        dev_link = soup.find("a", href=re.compile(r"/store/apps/dev"))
        if dev_link:
            span = dev_link.find("span")
            if span:
                return span.get_text(strip=True)
            return dev_link.get_text(strip=True)

        # Try div with specific class patterns
        for div in soup.find_all("div"):
            text = div.get_text(strip=True)
            if text and len(text) < 100:  # Developer names are typically short
                # Look for common developer name patterns in nearby elements
                next_sibling = div.find_next_sibling()
                if next_sibling and "category" in str(next_sibling).lower():
                    return text

        return ""

    def _extract_category(self, soup: BeautifulSoup) -> str:
        """Extract the app category."""
        # Try category link
        cat_link = soup.find("a", href=re.compile(r"/store/apps/category"))
        if cat_link:
            span = cat_link.find("span")
            if span:
                return span.get_text(strip=True)
            return cat_link.get_text(strip=True)

        # Try itemprop="genre"
        genre = soup.find(itemprop="genre")
        if genre:
            return genre.get_text(strip=True)

        return ""

    def _extract_rating(self, soup: BeautifulSoup) -> tuple:
        """Extract the app rating and review count."""
        rating = 0.0
        rating_count = 0

        # Try to find rating value
        # Look for aria-label with rating info
        for element in soup.find_all(attrs={"aria-label": True}):
            label = element.get("aria-label", "")
            # Pattern: "Rated X.X stars out of five stars"
            match = re.search(r"[Rr]ated\s+([\d.]+)\s+stars", label)
            if match:
                try:
                    rating = float(match.group(1))
                except ValueError:
                    pass

            # Pattern: "X reviews"
            match = re.search(r"([\d,]+)\s+reviews", label)
            if match:
                try:
                    rating_count = int(match.group(1).replace(",", ""))
                except ValueError:
                    pass

        # Try finding rating in text content
        if rating == 0.0:
            for div in soup.find_all("div"):
                text = div.get_text(strip=True)
                # Look for patterns like "4.5" followed by star icon reference
                match = re.match(r"^([\d.]+)$", text)
                if match:
                    try:
                        val = float(match.group(1))
                        if 1.0 <= val <= 5.0:
                            rating = val
                            break
                    except ValueError:
                        pass

        return rating, rating_count

    def _extract_downloads(self, soup: BeautifulSoup) -> str:
        """Extract the download count."""
        # Look for download count patterns
        download_patterns = [
            r"([\d,]+\+?)\s+downloads",
            r"([\d,]+\+?)\s+installs",
            r"([\d]+[KMB]?\+?)\s+downloads",
            r"([\d]+[KMB]?\+?)\s+installs",
        ]

        text = soup.get_text()
        for pattern in download_patterns:
            match = re.search(pattern, text, re.IGNORECASE)
            if match:
                return match.group(1)

        # Try aria-label
        for element in soup.find_all(attrs={"aria-label": True}):
            label = element.get("aria-label", "")
            for pattern in download_patterns:
                match = re.search(pattern, label, re.IGNORECASE)
                if match:
                    return match.group(1)

        return ""

    def _extract_description(self, soup: BeautifulSoup) -> str:
        """Extract the app description."""
        # Try data-g-id="description"
        desc_div = soup.find(attrs={"data-g-id": "description"})
        if desc_div:
            return desc_div.get_text(separator=" ", strip=True)

        # Try itemprop="description"
        desc_meta = soup.find(itemprop="description")
        if desc_meta:
            if desc_meta.get("content"):
                return desc_meta["content"]
            return desc_meta.get_text(separator=" ", strip=True)

        # Try og:description
        og_desc = soup.find("meta", property="og:description")
        if og_desc and og_desc.get("content"):
            return og_desc["content"]

        # Try meta description
        meta_desc = soup.find("meta", attrs={"name": "description"})
        if meta_desc and meta_desc.get("content"):
            return meta_desc["content"]

        return ""

    def _extract_last_updated(self, soup: BeautifulSoup) -> str:
        """Extract the last updated date."""
        # Look for "Updated" text followed by a date
        text = soup.get_text()
        patterns = [
            r"Updated\s+on?\s*(\w+\s+\d+,?\s+\d+)",
            r"Last updated:?\s*(\w+\s+\d+,?\s+\d+)",
        ]

        for pattern in patterns:
            match = re.search(pattern, text, re.IGNORECASE)
            if match:
                return match.group(1)

        return ""

    def _extract_content_rating(self, soup: BeautifulSoup) -> str:
        """Extract the content rating."""
        # Look for content rating indicators
        rating_patterns = [
            r"(Everyone|Teen|Mature|Adults Only)",
            r"Rated for\s+(\d+\+?)",
            r"Content rating:?\s*(\w+)",
        ]

        text = soup.get_text()
        for pattern in rating_patterns:
            match = re.search(pattern, text, re.IGNORECASE)
            if match:
                return match.group(1)

        return ""

    def _build_content_for_llm(
        self,
        app_name: str,
        developer: str,
        category: str,
        description: str,
        rating: float,
        rating_count: int,
        downloads: str,
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
        ]

        if category:
            content_parts.append(f"Category: {category}")

        if rating and rating_count:
            content_parts.append(f"Rating: {rating:.1f}/5.0 ({rating_count:,} reviews)")
        elif rating:
            content_parts.append(f"Rating: {rating:.1f}/5.0")

        if downloads:
            content_parts.append(f"Downloads: {downloads}")

        if content_rating:
            content_parts.append(f"Content Rating: {content_rating}")

        if description:
            content_parts.append(f"\nDescription:\n{description}")

        return "\n".join(content_parts)

    async def fetch_batch(self, package_names: List[str]) -> List[Dict[str, Any]]:
        """
        Fetch metadata for multiple Android apps.

        Args:
            package_names: List of Android package names

        Returns:
            List of metadata dictionaries
        """
        tasks = [self.fetch_app_metadata(pkg) for pkg in package_names]
        return await asyncio.gather(*tasks, return_exceptions=False)

    def get_stats(self) -> Dict[str, Any]:
        """Get scraper statistics."""
        return {
            "request_count": self.request_count,
            "success_count": self.success_count,
            "error_count": self.error_count,
            "success_rate": (
                self.success_count / self.request_count * 100
                if self.request_count > 0 else 0.0
            ),
        }
