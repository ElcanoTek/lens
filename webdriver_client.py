# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""A minimal W3C WebDriver client for the deep-scrape Chrome container.

The deep pass needs five WebDriver commands: open a session, navigate, read
the current URL, read the page source, and close the session (plus setting
the page-load timeout). That is a handful of JSON-over-HTTP calls, so this
module makes them with the standard library instead of depending on the
Selenium bindings and their transitive tree.

The calls are synchronous, like Selenium's: ``ScraperClient`` runs them in
``asyncio.to_thread``.

Errors follow the W3C error codes. ``timeout`` raises ``TimeoutException``,
``session not created`` raises ``SessionNotCreatedException``, ``invalid session
id`` raises ``InvalidSessionIdException``, and every other error (including the
endpoint being unreachable) raises ``WebDriverException``, the base class the
scraper catches. The exception text starts with the W3C error code, so the
scraper's restart signatures ("invalid session id", "chrome not reachable" …)
still match.

Requests always go straight to the endpoint: proxy variables in the
environment are ignored, because the container is on loopback and a proxy
would only see (or break) the browser traffic.
"""

from __future__ import annotations

import http.client
import json
import urllib.error
import urllib.request
from typing import Any, Dict, Optional, Tuple
from urllib.parse import quote

# Selenium's default HTTP timeout for a WebDriver command.
DEFAULT_COMMAND_TIMEOUT = 120.0
# Navigation blocks until the page-load timeout fires; leave room past it so
# the browser's own timeout error arrives instead of a socket timeout.
_NAVIGATION_MARGIN = 30.0
# Error text from the endpoint is quoted into logs; keep it bounded.
_MAX_ERROR_TEXT = 500


class WebDriverException(Exception):
    """A WebDriver command failed. ``error`` is the W3C error code."""

    def __init__(self, message: str, error: str = "unknown error") -> None:
        super().__init__(message)
        self.error = error


class TimeoutException(WebDriverException):
    """The browser gave up on the command (W3C ``timeout``)."""


class SessionNotCreatedException(WebDriverException):
    """The endpoint refused to start a browser (W3C ``session not created``)."""


class InvalidSessionIdException(WebDriverException):
    """The session is gone, e.g. Chrome crashed (W3C ``invalid session id``)."""


_ERRORS = {
    "timeout": TimeoutException,
    "session not created": SessionNotCreatedException,
    "invalid session id": InvalidSessionIdException,
}

_NO_PROXY_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def chrome_capabilities(
    *,
    arguments: list,
    exclude_switches: list,
    use_automation_extension: bool,
    page_load_strategy: str,
    page_load_timeout: Optional[float] = None,
) -> Dict[str, Any]:
    """The ``alwaysMatch`` capabilities for a Chrome session."""
    capabilities: Dict[str, Any] = {
        "browserName": "chrome",
        "pageLoadStrategy": page_load_strategy,
        "goog:chromeOptions": {
            "args": list(arguments),
            "excludeSwitches": list(exclude_switches),
            "useAutomationExtension": use_automation_extension,
        },
    }
    if page_load_timeout is not None:
        capabilities["timeouts"] = {"pageLoad": _milliseconds(page_load_timeout)}
    return capabilities


def _milliseconds(seconds: float) -> int:
    return int(float(seconds) * 1000)


def _decode_reply(status: Optional[int], body: bytes) -> Tuple[Any, Dict[str, Any]]:
    """Return ``(value, payload)`` from a reply, or raise the WebDriver error
    it carries."""
    try:
        payload = json.loads(body.decode("utf-8")) if body else {}
    except (UnicodeDecodeError, json.JSONDecodeError):
        payload = None
    value = payload.get("value") if isinstance(payload, dict) else None
    if isinstance(value, dict) and value.get("error"):
        error = str(value["error"])
        message = str(value.get("message") or "")[:_MAX_ERROR_TEXT]
        raise _ERRORS.get(error, WebDriverException)(f"{error}: {message}", error)
    if status is not None and not 200 <= status < 300:
        text = body.decode("utf-8", "replace")[:_MAX_ERROR_TEXT]
        raise WebDriverException(f"unknown error: HTTP {status}: {text}")
    if not isinstance(payload, dict):
        raise WebDriverException("unknown error: response is not a JSON object")
    return payload.get("value"), payload


class RemoteSession:
    """One browser session on a WebDriver endpoint.

    Creating the object starts the session; ``quit()`` ends it and is safe to
    call more than once. The interface (``set_page_load_timeout``, ``get``,
    ``current_url``, ``page_source``, ``quit``) is the subset of Selenium's
    ``webdriver.Remote`` the scraper used.
    """

    def __init__(
        self,
        command_executor: str,
        capabilities: Dict[str, Any],
        *,
        command_timeout: float = DEFAULT_COMMAND_TIMEOUT,
    ) -> None:
        self._base = command_executor.rstrip("/")
        self._command_timeout = command_timeout
        self._page_load_timeout: Optional[float] = None
        timeouts = capabilities.get("timeouts") or {}
        if "pageLoad" in timeouts:
            self._page_load_timeout = timeouts["pageLoad"] / 1000
        self.session_id: Optional[str] = None
        value, payload = self._request(
            "POST",
            "/session",
            {"capabilities": {"alwaysMatch": capabilities, "firstMatch": [{}]}},
        )
        session_id = value.get("sessionId") if isinstance(value, dict) else None
        session_id = session_id or payload.get("sessionId")  # pre-W3C servers
        if not session_id:
            raise SessionNotCreatedException(
                "session not created: the endpoint returned no session id",
                "session not created",
            )
        self.session_id = str(session_id)

    # -- transport --------------------------------------------------------

    def _request(
        self,
        method: str,
        path: str,
        body: Optional[Dict[str, Any]] = None,
        *,
        timeout: Optional[float] = None,
    ):
        data = json.dumps(body).encode("utf-8") if body is not None else None
        request = urllib.request.Request(
            self._base + path,
            data=data,
            method=method,
            headers={
                "Accept": "application/json",
                "Content-Type": "application/json;charset=UTF-8",
            },
        )
        try:
            with _NO_PROXY_OPENER.open(request, timeout=timeout or self._command_timeout) as reply:
                return _decode_reply(reply.status, reply.read())
        except urllib.error.HTTPError as exc:
            with exc:
                return _decode_reply(exc.code, exc.read())
        except WebDriverException:
            raise
        # Refused, reset, socket timeout, truncated reply, malformed URL.
        except (OSError, http.client.HTTPException, ValueError) as exc:
            raise WebDriverException(
                f"unknown error: WebDriver endpoint unreachable: {exc}"
            ) from exc

    def _session_path(self, suffix: str = "") -> str:
        if self.session_id is None:
            raise InvalidSessionIdException(
                "invalid session id: the session was already closed", "invalid session id"
            )
        return f"/session/{quote(self.session_id, safe='')}{suffix}"

    # -- commands ---------------------------------------------------------

    def set_page_load_timeout(self, seconds: float) -> None:
        self._request("POST", self._session_path("/timeouts"), {"pageLoad": _milliseconds(seconds)})
        self._page_load_timeout = float(seconds)

    def get(self, url: str) -> None:
        wait = self._command_timeout
        if self._page_load_timeout is not None:
            wait = max(wait, self._page_load_timeout + _NAVIGATION_MARGIN)
        self._request("POST", self._session_path("/url"), {"url": url}, timeout=wait)

    @property
    def current_url(self) -> str:
        value, _ = self._request("GET", self._session_path("/url"))
        return str(value)

    @property
    def page_source(self) -> str:
        value, _ = self._request("GET", self._session_path("/source"))
        return str(value)

    def quit(self) -> None:
        """End the session. Later calls do nothing."""
        if self.session_id is None:
            return
        path = self._session_path()
        self.session_id = None
        self._request("DELETE", path)
