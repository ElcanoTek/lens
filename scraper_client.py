# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Asynchronous HTTP scraper used for site analysis."""

import asyncio
import json
import logging
import os
import subprocess
import time
import urllib.error
import urllib.request
from http.client import RemoteDisconnected
from datetime import datetime
from typing import Any, Dict, List, Optional, Tuple
from urllib.parse import urlparse

import aiohttp
from bs4 import BeautifulSoup

try:  # Optional dependency for deep scraping mode
    from selenium import webdriver
    from selenium.common.exceptions import WebDriverException
    from selenium.webdriver.chrome.options import Options

    SELENIUM_AVAILABLE = True
except ImportError:  # pragma: no cover - optional dependency
    webdriver = None  # type: ignore[assignment]
    WebDriverException = Exception  # type: ignore[assignment]
    Options = None  # type: ignore[assignment]
    SELENIUM_AVAILABLE = False

logger = logging.getLogger(__name__)

# The LLM prompt only consumes the first MAX_CONTENT_CHARS characters of
# extracted text, so every scrape backend truncates to the same budget.
MAX_CONTENT_CHARS = 4000

# Cap on how much of a response body we buffer. The pipeline only keeps the
# first MAX_CONTENT_CHARS characters of extracted text, so anything beyond
# this is waste — and without a cap a single huge response could exhaust
# memory.
MAX_RESPONSE_BYTES = 5 * 1024 * 1024

# Firecrawl's /v2/scrape rejects timeouts above 300 seconds.
FIRECRAWL_MAX_TIMEOUT_MS = 300_000


def _truncate_content(text: str) -> str:
    if len(text) > MAX_CONTENT_CHARS:
        return text[:MAX_CONTENT_CHARS] + "..."
    return text


class _RetryableStatusError(Exception):
    """Transient Firecrawl response (5xx/408/429) — worth another attempt."""


async def firecrawl_service_ready(
    url: str,
    session: Optional["aiohttp.ClientSession"] = None,
    timeout: float = 5.0,
) -> bool:
    """True when a Firecrawl API answers at *url*.

    Checks the response body for the service's identity banner, not just the
    status code — an unrelated service squatting on the port must not count
    as available (it would silently absorb a whole retry pass).
    """
    owned = session is None
    if owned:
        session = aiohttp.ClientSession()
    try:
        async with session.get(
            f"{url.rstrip('/')}/", timeout=aiohttp.ClientTimeout(total=timeout)
        ) as response:
            if response.status >= 500:
                return False
            body = await response.text()
            return "firecrawl" in body.lower()
    except (aiohttp.ClientError, asyncio.TimeoutError, OSError):
        return False
    finally:
        if owned:
            await session.close()


def _browser_headers(user_agent: str) -> Dict[str, str]:
    """Headers matching what a real browser sends on a top-level navigation.

    Sites with basic bot filtering reject requests that carry a User-Agent
    with none of the companion headers a browser always includes.
    """
    return {
        "User-Agent": user_agent,
        "Accept": (
            "text/html,application/xhtml+xml,application/xml;q=0.9,"
            "image/avif,image/webp,*/*;q=0.8"
        ),
        "Accept-Language": "en-US,en;q=0.9",
        "Upgrade-Insecure-Requests": "1",
        "Sec-Fetch-Dest": "document",
        "Sec-Fetch-Mode": "navigate",
        "Sec-Fetch-Site": "none",
        "Sec-Fetch-User": "?1",
    }


class ScraperSession:
    """Represents a simple HTTP scraping session."""

    def __init__(self, session_id: str, driver: Optional[Any] = None):
        self.id = session_id
        self.in_use = False
        self.last_used: Optional[float] = None
        self.driver = driver

    def __repr__(self) -> str:  # pragma: no cover - debug helper
        return f"ScraperSession(id={self.id!r}, in_use={self.in_use})"


class ScraperClient:
    """Async client for performing HTTP scraping with lightweight session management."""

    def __init__(
        self,
        *,
        request_timeout: int = 30,
        max_retries: int = 3,
        retry_delay: float = 2.0,
        user_agent: str = "Mozilla/5.0",
        mode: str = "direct",
        podman_binary: str = "podman",
        chrome_image: str = "docker.io/selenium/standalone-chrome:latest",
        chrome_container_name: str = "elcano-lens-chromedriver",
        chrome_port: int = 4444,
        chrome_vnc_port: Optional[int] = 7900,
        chrome_extra_args: Optional[List[str]] = None,
        chrome_startup_timeout: int = 45,
        chrome_wait_after_load: float = 1.0,
        keep_container: bool = False,
        reject_redirects: bool = True,
        chrome_pull_policy: str = "always",
        firecrawl_url: str = "http://127.0.0.1:3002",
        firecrawl_timeout: int = 60,
        firecrawl_wait_for: int = 0,
        firecrawl_max_age_ms: Optional[int] = None,
        firecrawl_proxy: str = "basic",
    ) -> None:
        self.request_timeout = request_timeout
        self.max_retries = max_retries
        self.retry_delay = retry_delay
        self.user_agent = user_agent
        self.mode = mode.lower()
        self.podman_binary = podman_binary
        self.chrome_image = chrome_image
        self.chrome_container_name = chrome_container_name
        self.chrome_port = chrome_port
        self.chrome_vnc_port = chrome_vnc_port
        if chrome_extra_args is None:
            self.chrome_extra_args = []
        elif isinstance(chrome_extra_args, (list, tuple)):
            self.chrome_extra_args = [str(arg) for arg in chrome_extra_args]
        else:
            self.chrome_extra_args = [str(chrome_extra_args)]
        self.chrome_startup_timeout = chrome_startup_timeout
        self.chrome_wait_after_load = chrome_wait_after_load
        self.keep_container = keep_container
        self.reject_redirects = reject_redirects
        self.chrome_pull_policy = chrome_pull_policy
        self.firecrawl_url = firecrawl_url.rstrip("/")
        self.firecrawl_timeout = firecrawl_timeout
        self.firecrawl_wait_for = firecrawl_wait_for
        self.firecrawl_max_age_ms = firecrawl_max_age_ms
        self.firecrawl_proxy = firecrawl_proxy
        self._container_started = False
        self._chrome_endpoint = f"http://127.0.0.1:{self.chrome_port}"

        self._web_session: Optional[aiohttp.ClientSession] = None
        self.sessions: List[ScraperSession] = []
        self.session_semaphore: Optional[asyncio.Semaphore] = None

    async def __aenter__(self) -> "ScraperClient":
        if self.mode not in {"direct", "deep", "firecrawl"}:
            raise ValueError(f"Unsupported scrape mode: {self.mode}")

        if self.mode == "deep":
            if not SELENIUM_AVAILABLE:
                raise RuntimeError(
                    "Deep scrape mode requires the 'selenium' package. "
                    "Install it with `pip install selenium`."
                )

            await asyncio.to_thread(self._ensure_chrome_container)
            self._web_session = None
        elif self.mode == "firecrawl":
            # Requests are proxied through the local Firecrawl service, which
            # applies its own per-page timeout — allow headroom on top of it.
            timeout = aiohttp.ClientTimeout(total=self.firecrawl_timeout + 30)
            self._web_session = aiohttp.ClientSession(timeout=timeout)
            await self._check_firecrawl_service()
        else:
            timeout = aiohttp.ClientTimeout(total=self.request_timeout + 10)
            self._web_session = aiohttp.ClientSession(
                timeout=timeout,
                headers=_browser_headers(self.user_agent),
            )
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb) -> None:
        await self.cleanup()
        if self._web_session:
            await self._web_session.close()
            self._web_session = None
        if self.mode == "deep" and self._container_started and not self.keep_container:
            await asyncio.to_thread(self._stop_chrome_container)

    async def create_session_pool(self, num_sessions: int) -> None:
        """Initialise a pool of reusable scraping sessions."""
        requested_sessions = num_sessions

        if num_sessions <= 0:
            raise ValueError("Number of sessions must be at least 1")

        if self.mode == "deep" and num_sessions > 1:
            logger.warning(
                "Deep scrape mode supports only a single concurrent browser session. "
                "Reducing requested sessions from %s to 1.",
                num_sessions,
            )
            num_sessions = 1

        logger.info(
            "Creating %s local scraping sessions (requested %s)...",
            num_sessions,
            requested_sessions,
        )

        self.session_semaphore = asyncio.Semaphore(num_sessions)
        if self.mode == "deep":
            self.sessions = []
            for i in range(num_sessions):
                driver = await asyncio.to_thread(self._create_webdriver)
                self.sessions.append(ScraperSession(f"chrome-{i}", driver=driver))
        else:
            self.sessions = [ScraperSession(f"local-{i}") for i in range(num_sessions)]

    async def get_available_session(self) -> ScraperSession:
        """Acquire an available session from the pool."""
        if not self.session_semaphore:
            raise RuntimeError("Session pool has not been initialised")

        await self.session_semaphore.acquire()

        try:
            for session in self.sessions:
                if not session.in_use:
                    session.in_use = True
                    session.last_used = time.time()
                    return session

            raise RuntimeError("No available sessions in pool")
        except BaseException:
            # Never hold the permit on failure — a leaked permit would shrink
            # the pool permanently and eventually deadlock all workers.
            self.session_semaphore.release()
            raise

    def release_session(self, session: ScraperSession) -> None:
        """Return a session to the pool."""
        session.in_use = False
        if self.session_semaphore:
            self.session_semaphore.release()

    async def scrape_site(
        self,
        session: ScraperSession,
        url: str,
        timeout: Optional[int] = None,
    ) -> Dict[str, Any]:
        """Scrape a website using direct HTTP requests."""
        start_time = time.time()
        timeout = timeout or self.request_timeout

        try:
            if not url.startswith(("http://", "https://")):
                url = f"https://{url}"

            scraped_at = datetime.now().isoformat()
            if self.mode == "deep":
                scrape_result = await self._scrape_with_chrome(session, url, timeout)
            elif self.mode == "firecrawl":
                scrape_result = await self._scrape_with_firecrawl(url)
            else:
                scrape_result = await self._scrape_direct(url, timeout)

            processing_time = time.time() - start_time

            return {
                "success": True,
                "url": url,
                "title": scrape_result.get("title", ""),
                "content": scrape_result.get("content", ""),
                "meta_description": scrape_result.get("meta_description", ""),
                "content_length": len(scrape_result.get("content", "")),
                "processing_time": processing_time,
                "status_code": scrape_result.get("status_code", 200),
                "mode": self.get_mode(),
                "scraped_at": scraped_at,
            }
        except Exception as exc:
            processing_time = time.time() - start_time
            logger.error("Failed to scrape %s: %s", url, exc)
            return {
                "success": False,
                "url": url,
                "error": str(exc),
                "processing_time": processing_time,
                "mode": self.get_mode(),
                "scraped_at": datetime.now().isoformat(),
            }

    async def _scrape_direct(self, url: str, timeout: int) -> Dict[str, Any]:
        if not self._web_session:
            raise RuntimeError("Web session is not initialised")

        try:
            html, status_code = await self._fetch_html_with_retries(url, timeout)
        except RuntimeError as exc:
            # Bare domains sometimes have no A record (or no listener) while
            # the www. host works. Only worth trying when the connection
            # itself failed — an HTTP-level error would just repeat.
            fallback_url = self._www_fallback_url(url)
            if fallback_url is None or not self._is_connection_failure(exc):
                raise
            logger.info(
                "Connection to %s failed; retrying via %s", url, fallback_url
            )
            html, status_code = await self._fetch_html_with_retries(
                fallback_url, timeout
            )
            url = fallback_url
        parsed = self._parse_html(url, html)
        parsed["status_code"] = status_code
        return parsed

    @staticmethod
    def _www_fallback_url(url: str) -> Optional[str]:
        parsed = urlparse(url)
        host = parsed.netloc
        # Only rewrite plain second-level hosts; anything with an explicit
        # subdomain (or port/credentials) is left alone.
        if not host or host.startswith("www.") or "@" in host or ":" in host:
            return None
        if host.count(".") != 1:
            return None
        return parsed._replace(netloc=f"www.{host}").geturl()

    @staticmethod
    def _is_connection_failure(exc: Exception) -> bool:
        cause = exc.__cause__
        # TimeoutError is an OSError subclass (3.11+), but a hung host is not
        # a connection failure — falling back to www. would just hang again.
        if isinstance(cause, TimeoutError):
            return False
        return isinstance(
            cause,
            (aiohttp.ClientConnectorError, aiohttp.ServerDisconnectedError, OSError),
        )

    def _get_host(self, url: str) -> str:
        """Extract the host from a URL, normalizing www. prefix."""
        parsed = urlparse(url)
        host = parsed.netloc.lower()
        # Strip port if present
        if ":" in host:
            host = host.split(":")[0]
        # Normalize www. prefix
        if host.startswith("www."):
            host = host[4:]
        return host

    def _ensure_same_host(self, original_url: str, final_url: str) -> None:
        """Enforce the cross-host redirect policy shared by every backend."""
        if not self.reject_redirects:
            return
        original_host = self._get_host(original_url)
        final_host = self._get_host(final_url)
        if original_host != final_host:
            raise RuntimeError(f"URL redirected to different host: {final_host}")

    async def _fetch_html_with_retries(
        self, url: str, timeout: int
    ) -> Tuple[str, int]:
        if not self._web_session:
            raise RuntimeError("Web session is not initialised")

        last_exception = None
        
        for attempt in range(1, self.max_retries + 1):
            try:
                async with self._web_session.get(
                    url, timeout=timeout, allow_redirects=True
                ) as response:
                    self._ensure_same_host(url, str(response.url))

                    raw = await self._read_body(response)
                    try:
                        text = raw.decode(response.charset or "utf-8", errors="ignore")
                    except LookupError:
                        # Server declared a charset Python doesn't know.
                        text = raw.decode("utf-8", errors="ignore")
                    return text, response.status
            except RuntimeError:
                # Re-raise RuntimeError (including redirect errors) without retry
                raise
            except aiohttp.ClientConnectorDNSError as exc:
                # DNS resolution won't heal within a retry cycle; fail fast so
                # dead domains don't hold a session slot through backoff sleeps.
                last_exception = exc
                logger.warning(
                    "DNS resolution failed for %s: %s", url, str(exc)[:100]
                )
                break
            except Exception as exc:
                last_exception = exc
                if attempt < self.max_retries:
                    delay = self.retry_delay * (2 ** (attempt - 1))
                    logger.warning(
                        "Attempt %d/%d to fetch %s failed: %s. Retrying in %.1fs...",
                        attempt, self.max_retries, url, str(exc)[:100], delay
                    )
                    await asyncio.sleep(delay)
                else:
                    logger.error(
                        "All %d attempts to fetch %s failed: %s",
                        self.max_retries, url, str(exc)[:100]
                    )

        raise RuntimeError(f"Failed to fetch {url}: {last_exception}") from last_exception

    @staticmethod
    async def _read_body(response: "aiohttp.ClientResponse") -> bytes:
        """Read the full response body, capped at MAX_RESPONSE_BYTES.

        StreamReader.read(n) returns whatever is currently buffered (up to n
        bytes), NOT the full body — a single call typically stops at the first
        128 KiB chunk boundary. On modern sites that first chunk is almost all
        <head> markup, so text extraction came back near-empty and real pages
        failed validation as "content too short". Loop until EOF or the cap.
        """
        chunks: List[bytes] = []
        total = 0
        while total < MAX_RESPONSE_BYTES:
            chunk = await response.content.read(MAX_RESPONSE_BYTES - total)
            if not chunk:
                break
            chunks.append(chunk)
            total += len(chunk)
        return b"".join(chunks)

    def _parse_html(self, url: str, html: str) -> Dict[str, Any]:
        soup = BeautifulSoup(html, "html.parser")

        for element in soup(["script", "style", "noscript"]):
            element.decompose()

        title = soup.title.string.strip() if soup.title and soup.title.string else ""
        meta_description_tag = soup.find("meta", attrs={"name": "description"})
        meta_description = ""
        if meta_description_tag and meta_description_tag.get("content"):
            meta_description = meta_description_tag["content"].strip()

        text_content = _truncate_content(soup.get_text(separator=" ", strip=True))

        return {
            "title": title,
            "meta_description": meta_description,
            "content": text_content,
            "url": url,
        }

    # ------------------------------------------------------------------
    # Firecrawl helpers
    # ------------------------------------------------------------------

    async def _check_firecrawl_service(self) -> None:
        """Verify the local Firecrawl API is reachable before accepting work."""
        if not self._web_session:
            raise RuntimeError("Web session is not initialised")
        if not await firecrawl_service_ready(
            self.firecrawl_url, session=self._web_session
        ):
            # __aexit__ never runs when __aenter__ raises, so close the
            # session here or it leaks its connector.
            await self._web_session.close()
            self._web_session = None
            raise RuntimeError(
                f"Firecrawl service is not reachable at {self.firecrawl_url}. "
                "Start it with `scripts/firecrawl.sh up`."
            )
        logger.info("Firecrawl service is ready at %s", self.firecrawl_url)

    async def _scrape_with_firecrawl(self, url: str) -> Dict[str, Any]:
        """Scrape a URL through the local Firecrawl service (markdown output)."""
        if not self._web_session:
            raise RuntimeError("Web session is not initialised")

        payload: Dict[str, Any] = {
            "url": url,
            "formats": ["markdown"],
            "onlyMainContent": True,
            "timeout": min(self.firecrawl_timeout * 1000, FIRECRAWL_MAX_TIMEOUT_MS),
            # "basic" = one render attempt, no stealth-proxy escalation. See the
            # firecrawl_proxy note in config.py for why escalation is futile
            # (and counterproductive) on a self-hosted stack.
            "proxy": self.firecrawl_proxy,
        }
        if self.firecrawl_wait_for > 0:
            payload["waitFor"] = self.firecrawl_wait_for
        if self.firecrawl_max_age_ms is not None:
            payload["maxAge"] = self.firecrawl_max_age_ms

        last_exception: Optional[Exception] = None
        for attempt in range(1, self.max_retries + 1):
            try:
                async with self._web_session.post(
                    f"{self.firecrawl_url}/v2/scrape", json=payload
                ) as response:
                    # Status check before JSON parsing: transient errors (a
                    # restarting API serves HTML 502s, a saturated queue sheds
                    # load with 429s) must surface as their status, not as a
                    # JSON decode error.
                    if response.status >= 500 or response.status in (408, 429):
                        detail = (await response.text())[:200]
                        # Anti-bot / retry-limit failures are deterministic:
                        # the service already exhausted its own render attempts
                        # against a site that is blocking us. Retrying from here
                        # just re-hammers that site (wasted time + ban risk), so
                        # surface it as a permanent failure instead of retrying.
                        if (
                            "SCRAPE_RETRY_LIMIT" in detail
                            or "document_antibot" in detail
                        ):
                            raise RuntimeError(
                                f"Firecrawl could not bypass anti-bot protection "
                                f"(HTTP {response.status})"
                            )
                        raise _RetryableStatusError(
                            f"HTTP {response.status}: {detail}"
                        )
                    body = await response.json(content_type=None)
                    if not isinstance(body, dict):
                        raise RuntimeError(
                            f"Firecrawl returned an unexpected response: "
                            f"{str(body)[:100]}"
                        )
                    if not body.get("success", False):
                        # The service already exhausted its own fetch attempts;
                        # a retry from here would not change the outcome.
                        raise RuntimeError(
                            f"Firecrawl scrape failed: "
                            f"{body.get('error', 'unknown error')}"
                        )
                    return self._parse_firecrawl_document(url, body.get("data") or {})
            except RuntimeError:
                raise
            except Exception as exc:
                last_exception = exc
                if attempt < self.max_retries:
                    delay = self.retry_delay * (2 ** (attempt - 1))
                    logger.warning(
                        "Attempt %d/%d to scrape %s via Firecrawl failed: %s. "
                        "Retrying in %.1fs...",
                        attempt, self.max_retries, url, str(exc)[:100], delay,
                    )
                    await asyncio.sleep(delay)

        raise RuntimeError(
            f"Failed to scrape {url} via Firecrawl: {last_exception}"
        ) from last_exception

    def _parse_firecrawl_document(
        self, url: str, document: Dict[str, Any]
    ) -> Dict[str, Any]:
        metadata = document.get("metadata") or {}

        final_url = metadata.get("url") or metadata.get("sourceURL") or url
        self._ensure_same_host(url, final_url)

        content = _truncate_content(document.get("markdown") or "")

        status_code = metadata.get("statusCode")
        try:
            status_code = int(status_code)
        except (TypeError, ValueError):
            status_code = 200

        return {
            "title": metadata.get("title") or "",
            "meta_description": metadata.get("description") or "",
            "content": content,
            "url": url,
            "status_code": status_code,
        }

    async def cleanup(self) -> None:
        """Reset session state."""
        logger.info("Cleaning up scraper sessions...")
        active_sessions = [session for session in self.sessions if session.in_use]
        if self.mode == "deep":
            for session in self.sessions:
                driver = getattr(session, "driver", None)
                if driver:
                    await asyncio.to_thread(self._safe_quit_driver, driver)
        self.sessions.clear()
        if self.session_semaphore:
            for _ in active_sessions:
                self.session_semaphore.release()
            self.session_semaphore = None

    def get_session_stats(self) -> Dict[str, Any]:
        total_sessions = len(self.sessions)
        in_use_sessions = sum(1 for session in self.sessions if session.in_use)
        available_sessions = total_sessions - in_use_sessions

        return {
            "total_sessions": total_sessions,
            "in_use": in_use_sessions,
            "available": available_sessions,
            "utilization": (in_use_sessions / total_sessions * 100)
            if total_sessions > 0
            else 0,
        }

    def get_mode(self) -> str:
        if self.mode in {"deep", "firecrawl"}:
            return self.mode
        return "direct"

    # ------------------------------------------------------------------
    # Deep scrape helpers
    # ------------------------------------------------------------------

    def _podman_env(self) -> Dict[str, str]:
        """Environment for podman subprocesses.

        Rootless podman needs XDG_RUNTIME_DIR to find its runtime root
        (/run/user/<uid>). systemd services with User= don't get it set, so
        default it when the directory exists (it does once lingering is
        enabled for the service user — bootstrap/update handle that).
        """
        env = dict(os.environ)
        if not env.get("XDG_RUNTIME_DIR"):
            runtime_dir = f"/run/user/{os.getuid()}"
            if os.path.isdir(runtime_dir):
                env["XDG_RUNTIME_DIR"] = runtime_dir
        return env

    def _podman(self, *args: str, check: bool = True) -> subprocess.CompletedProcess:
        return subprocess.run(
            [self.podman_binary, *args],
            check=check,
            capture_output=True,
            text=True,
            env=self._podman_env(),
        )

    def _local_image_exists(self) -> bool:
        try:
            return self._podman("image", "exists", self.chrome_image, check=False).returncode == 0
        except FileNotFoundError:
            return False

    def _ensure_chrome_container(self) -> None:
        """Ensure the Podman Chrome container is running."""
        if self._is_container_running():
            logger.debug(
                "Chromedriver container %s already running",
                self.chrome_container_name,
            )
            self._container_started = True
            return

        logger.info(
            "Starting Chrome container %s using image %s on port %s",
            self.chrome_container_name,
            self.chrome_image,
            self.chrome_port,
        )

        def run_command_for(pull_policy: str) -> List[str]:
            command = [
                self.podman_binary,
                "run",
                f"--pull={pull_policy}",
                "--rm",
                "-d",
                "--name",
                self.chrome_container_name,
                "-p",
                f"{self.chrome_port}:4444",
            ]
            if self.chrome_vnc_port:
                command.extend(["-p", f"{self.chrome_vnc_port}:7900"])
            command.extend(self.chrome_extra_args)
            command.append(self.chrome_image)
            return command

        try:
            subprocess.run(
                run_command_for(self.chrome_pull_policy),
                check=True,
                capture_output=True,
                text=True,
                env=self._podman_env(),
            )
        except subprocess.CalledProcessError as exc:  # pragma: no cover - podman failure
            error_output = exc.stderr.strip() or exc.stdout.strip()
            if "short-name resolution" in error_output.lower():
                error_output = (
                    "Podman short-name enforcement blocked pulling the Chrome image. "
                    "Set `deep_scrape_image` to a fully qualified reference such as "
                    "'docker.io/selenium/standalone-chrome:latest'."
                )
                raise RuntimeError(
                    f"Failed to start Chrome container: {error_output}"
                ) from exc

            # A pre-seeded image should survive registry/userns pull failures:
            # retry once against the local copy before giving up.
            if self.chrome_pull_policy != "never" and self._local_image_exists():
                logger.warning(
                    "Pulling %s failed (%s); retrying with the locally cached image",
                    self.chrome_image,
                    error_output.splitlines()[-1] if error_output else "unknown error",
                )
                try:
                    subprocess.run(
                        run_command_for("never"),
                        check=True,
                        capture_output=True,
                        text=True,
                        env=self._podman_env(),
                    )
                except subprocess.CalledProcessError as retry_exc:
                    retry_output = retry_exc.stderr.strip() or retry_exc.stdout.strip()
                    raise RuntimeError(
                        f"Failed to start Chrome container: {retry_output}"
                    ) from retry_exc
            else:
                raise RuntimeError(
                    f"Failed to start Chrome container: {error_output}"
                ) from exc

        self._container_started = True
        self._wait_for_container_ready()

    def _stop_chrome_container(self) -> None:
        self._container_started = False
        if not self._is_container_running():
            return

        try:
            self._podman("stop", self.chrome_container_name)
        except subprocess.CalledProcessError as exc:  # pragma: no cover - podman failure
            logger.warning(
                "Failed to stop Chrome container %s: %s",
                self.chrome_container_name,
                exc.stderr.strip() or exc.stdout.strip(),
            )

    def _is_container_running(self) -> bool:
        try:
            result = self._podman("ps", "--format", "{{.Names}}")
        except FileNotFoundError as exc:
            raise RuntimeError(
                f"Podman executable '{self.podman_binary}' not found."
            ) from exc
        except subprocess.CalledProcessError:
            return False

        running_names = {name.strip() for name in result.stdout.splitlines() if name.strip()}
        return self.chrome_container_name in running_names

    def _wait_for_container_ready(self) -> None:
        status_url = f"{self._chrome_endpoint}/wd/hub/status"
        deadline = time.time() + self.chrome_startup_timeout

        while time.time() < deadline:
            try:
                with urllib.request.urlopen(status_url, timeout=2) as response:
                    if response.status != 200:
                        raise urllib.error.URLError(
                            f"Unexpected status code {response.status}"
                        )
                    payload = json.loads(response.read().decode("utf-8"))
                    value = payload.get("value", {}) if isinstance(payload, dict) else {}
                    ready = value.get("ready", True)
                    if ready:
                        logger.info(
                            "Chromedriver container %s is ready", self.chrome_container_name
                        )
                        return
            except (
                urllib.error.URLError,
                json.JSONDecodeError,
                TimeoutError,
                RemoteDisconnected,
                ConnectionResetError,
                ConnectionRefusedError,
                OSError,
            ) as exc:
                logger.debug(
                    "Chromedriver container %s not ready yet (%s). Retrying...",
                    self.chrome_container_name,
                    exc,
                )
                time.sleep(1)
                continue

        raise RuntimeError(
            "Timed out waiting for Chrome container to become ready"
        )

    def _create_webdriver(self):
        assert SELENIUM_AVAILABLE and webdriver is not None and Options is not None

        options = Options()
        # Return from get() at DOMContentLoaded instead of the full load event.
        # Ad-heavy pages keep loading trackers/creatives for minutes and blow
        # the renderer timeout even though the article DOM rendered long ago;
        # wait_after_load below still gives client-side JS a beat to paint.
        options.page_load_strategy = "eager"
        options.add_argument("--headless=new")
        options.add_argument("--disable-gpu")
        options.add_argument("--no-sandbox")
        options.add_argument("--disable-dev-shm-usage")
        options.add_argument("--remote-allow-origins=*")
        options.add_argument(f"--user-agent={self.user_agent}")
        # Basic stealth: hide the automation fingerprints that trivially give
        # away headless Selenium (navigator.webdriver, the automation infobar,
        # and headless Chrome's default 800x600 window).
        options.add_argument("--disable-blink-features=AutomationControlled")
        options.add_argument("--window-size=1920,1080")
        options.add_argument("--lang=en-US")
        options.add_experimental_option("excludeSwitches", ["enable-automation"])
        options.add_experimental_option("useAutomationExtension", False)
        driver = webdriver.Remote(
            command_executor=f"{self._chrome_endpoint}/wd/hub",
            options=options,
        )
        driver.set_page_load_timeout(self.request_timeout)
        return driver

    async def _scrape_with_chrome(
        self, session: ScraperSession, url: str, timeout: int
    ) -> Dict[str, Any]:
        return await self._scrape_with_chrome_internal(session, url, timeout, True)

    async def _scrape_with_chrome_internal(
        self,
        session: ScraperSession,
        url: str,
        timeout: int,
        allow_recovery: bool,
    ) -> Dict[str, Any]:
        driver = session.driver
        if not driver:
            driver = await asyncio.to_thread(self._create_webdriver)
            session.driver = driver

        try:
            await asyncio.to_thread(self._navigate_with_driver, driver, url, timeout)

            if self.reject_redirects:
                final_url = await asyncio.to_thread(lambda: driver.current_url)
                self._ensure_same_host(url, final_url)

            html = await asyncio.to_thread(self._get_page_source, driver)
            parsed = self._parse_html(url, html)
            parsed["status_code"] = 200
            return parsed
        except RuntimeError:
            # Re-raise RuntimeError (including our redirect error) without wrapping
            raise
        except WebDriverException as exc:
            if allow_recovery and self._should_restart_chrome(exc):
                logger.warning(
                    "Chrome driver error encountered (%s). Attempting recovery...",
                    exc,
                )
                await asyncio.to_thread(self._recover_chrome_session, session, exc)
                return await self._scrape_with_chrome_internal(
                    session, url, timeout, False
                )

            raise RuntimeError(f"Chrome scraping failed: {exc}") from exc

    def _navigate_with_driver(self, driver: Any, url: str, timeout: int) -> None:
        driver.set_page_load_timeout(timeout)
        driver.get(url)
        if self.chrome_wait_after_load > 0:
            time.sleep(self.chrome_wait_after_load)

    def _get_page_source(self, driver: Any) -> str:
        return driver.page_source

    def _safe_quit_driver(self, driver: Any) -> None:
        try:
            driver.quit()
        except Exception:  # pragma: no cover - best effort cleanup
            logger.debug("Ignoring error while quitting driver", exc_info=True)

    def _should_restart_chrome(self, error: Exception) -> bool:
        message = str(error).lower()
        failure_signatures = (
            "invalid session id",
            "chrome not reachable",
            "chrome failed to start",
            "disconnected",
            "cannot find session",
        )

        if any(signature in message for signature in failure_signatures):
            return True

        try:
            return not self._is_container_running()
        except RuntimeError:
            # If Podman cannot be queried we should assume a restart is needed
            return True

    def _recover_chrome_session(self, session: ScraperSession, error: Exception) -> None:
        driver = session.driver
        if driver:
            self._safe_quit_driver(driver)
            session.driver = None

        logger.warning(
            "Restarting Chrome container %s after failure: %s",
            self.chrome_container_name,
            error,
        )
        self._stop_chrome_container()

        self._ensure_chrome_container()
        session.driver = self._create_webdriver()
