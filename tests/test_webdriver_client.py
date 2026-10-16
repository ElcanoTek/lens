# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""webdriver_client against a fake W3C WebDriver endpoint.

The fake serves the handful of commands the deep pass uses, records every
request, and can be told to answer a command with a W3C error, a non-JSON
gateway page, or nothing at all.
"""

import json
import socket
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import scraper_client
import webdriver_client
from scraper_client import ScraperClient, ScraperSession
from webdriver_client import (
    InvalidSessionIdException,
    RemoteSession,
    SessionNotCreatedException,
    TimeoutException,
    WebDriverException,
)

PAGE = "<html><head><title>Fake page</title></head><body><p>Rendered body</p></body></html>"


class FakeWebDriver:
    def __init__(self):
        self.requests = []
        self.sessions = set()
        self.url = "about:blank"
        self.fail = {}  # (method, suffix) -> (status, body bytes)
        self.counter = 0
        fake = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def _reply(self, status, payload):
                body = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def _handle(self, method):
                length = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(length)) if length else None
                fake.requests.append((method, self.path, body))
                prefix = "/wd/hub/session"
                assert self.path.startswith(prefix), self.path
                rest = self.path[len(prefix) :]
                if method == "POST" and rest == "":
                    if ("POST", "/session") in fake.fail:
                        return self._reply(*fake.fail[("POST", "/session")])
                    fake.counter += 1
                    session = f"s{fake.counter}"
                    fake.sessions.add(session)
                    caps = body["capabilities"]["alwaysMatch"]
                    return self._reply(200, {"value": {"sessionId": session, "capabilities": caps}})
                session, _, suffix = rest.lstrip("/").partition("/")
                suffix = "/" + suffix if suffix else ""
                if (method, suffix) in fake.fail:
                    return self._reply(*fake.fail[(method, suffix)])
                if session not in fake.sessions:
                    return self._reply(
                        404,
                        {"value": {"error": "invalid session id", "message": "no such session"}},
                    )
                if method == "DELETE" and suffix == "":
                    fake.sessions.discard(session)
                    return self._reply(200, {"value": None})
                if method == "POST" and suffix == "/timeouts":
                    return self._reply(200, {"value": None})
                if method == "POST" and suffix == "/url":
                    fake.url = body["url"]
                    return self._reply(200, {"value": None})
                if method == "GET" and suffix == "/url":
                    return self._reply(200, {"value": fake.url})
                if method == "GET" and suffix == "/source":
                    return self._reply(200, {"value": PAGE})
                return self._reply(404, {"value": {"error": "unknown command", "message": "?"}})

            def do_GET(self):
                self._handle("GET")

            def do_POST(self):
                self._handle("POST")

            def do_DELETE(self):
                self._handle("DELETE")

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.endpoint = f"http://127.0.0.1:{self.server.server_address[1]}"
        self.thread = threading.Thread(
            target=self.server.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True
        )
        self.thread.start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


@pytest.fixture
def fake():
    server = FakeWebDriver()
    yield server
    server.close()


def _caps():
    return webdriver_client.chrome_capabilities(
        arguments=["--headless=new"],
        exclude_switches=["enable-automation"],
        use_automation_extension=False,
        page_load_strategy="eager",
        page_load_timeout=12,
    )


def test_session_lifecycle(fake):
    driver = RemoteSession(f"{fake.endpoint}/wd/hub", _caps())
    driver.set_page_load_timeout(7.5)
    driver.get("https://example.com/")
    assert driver.current_url == "https://example.com/"
    assert driver.page_source == PAGE
    driver.quit()
    driver.quit()  # idempotent: no second DELETE

    methods = [(method, path) for method, path, _ in fake.requests]
    assert methods == [
        ("POST", "/wd/hub/session"),
        ("POST", "/wd/hub/session/s1/timeouts"),
        ("POST", "/wd/hub/session/s1/url"),
        ("GET", "/wd/hub/session/s1/url"),
        ("GET", "/wd/hub/session/s1/source"),
        ("DELETE", "/wd/hub/session/s1"),
    ]
    assert fake.requests[1][2] == {"pageLoad": 7500}
    assert fake.sessions == set()
    with pytest.raises(InvalidSessionIdException):
        driver.get("https://example.com/")


def test_new_session_payload(fake):
    RemoteSession(f"{fake.endpoint}/wd/hub", _caps()).quit()
    body = fake.requests[0][2]
    assert body == {
        "capabilities": {
            "alwaysMatch": {
                "browserName": "chrome",
                "pageLoadStrategy": "eager",
                "timeouts": {"pageLoad": 12000},
                "goog:chromeOptions": {
                    "args": ["--headless=new"],
                    "excludeSwitches": ["enable-automation"],
                    "useAutomationExtension": False,
                },
            },
            "firstMatch": [{}],
        }
    }


@pytest.mark.parametrize(
    "error,exception",
    [
        ("timeout", TimeoutException),
        ("invalid session id", InvalidSessionIdException),
        ("unknown error", WebDriverException),
        ("chrome not reachable", WebDriverException),
    ],
)
def test_w3c_errors_map_to_exception_types(fake, error, exception):
    fake.fail[("POST", "/url")] = (500, {"value": {"error": error, "message": "boom"}})
    driver = RemoteSession(f"{fake.endpoint}/wd/hub", _caps())
    with pytest.raises(exception) as raised:
        driver.get("https://slow.example/")
    assert raised.value.error == error
    # The code leads the message, so the scraper's restart signatures match.
    assert str(raised.value) == f"{error}: boom"
    assert isinstance(raised.value, WebDriverException)
    driver.quit()


def test_session_not_created(fake):
    fake.fail[("POST", "/session")] = (
        500,
        {"value": {"error": "session not created", "message": "Chrome failed to start"}},
    )
    with pytest.raises(SessionNotCreatedException, match="Chrome failed to start"):
        RemoteSession(f"{fake.endpoint}/wd/hub", _caps())


def test_non_json_gateway_error_is_a_webdriver_exception(fake):
    fake.fail[("GET", "/source")] = (502, b"<html>Bad Gateway</html>")
    driver = RemoteSession(f"{fake.endpoint}/wd/hub", _caps())
    with pytest.raises(WebDriverException, match="HTTP 502"):
        _ = driver.page_source
    driver.quit()


def test_unreachable_endpoint_is_a_webdriver_exception():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    with pytest.raises(WebDriverException, match="unreachable"):
        RemoteSession(f"http://127.0.0.1:{port}/wd/hub", _caps(), command_timeout=2)


def test_proxy_environment_is_ignored(fake, monkeypatch):
    # A proxy pointed at a closed port would fail every command if honoured.
    monkeypatch.setenv("http_proxy", "http://127.0.0.1:9")
    monkeypatch.setenv("HTTP_PROXY", "http://127.0.0.1:9")
    monkeypatch.delenv("no_proxy", raising=False)
    monkeypatch.delenv("NO_PROXY", raising=False)
    driver = RemoteSession(f"{fake.endpoint}/wd/hub", _caps())
    assert driver.page_source == PAGE
    driver.quit()


def _deep_client(fake, **kwargs):
    client = ScraperClient(mode="deep", request_timeout=9, chrome_wait_after_load=0, **kwargs)
    client._chrome_endpoint = fake.endpoint
    return client


async def test_scraper_deep_pass_through_the_fake_endpoint(fake):
    client = _deep_client(fake)
    session = ScraperSession("chrome-0", driver=client._create_webdriver())
    result = await client._scrape_with_chrome(session, "https://example.com/", 9)
    assert result["title"] == "Fake page"
    assert result["content"] == "Fake page Rendered body"
    assert result["status_code"] == 200

    caps = fake.requests[0][2]["capabilities"]["alwaysMatch"]
    args = caps["goog:chromeOptions"]["args"]
    assert "--headless=new" in args and "--disable-blink-features=AutomationControlled" in args
    assert any(a.startswith("--user-agent=") for a in args)
    assert caps["pageLoadStrategy"] == "eager"
    assert caps["timeouts"] == {"pageLoad": 9000}

    client.sessions = [session]
    await client.cleanup()
    assert fake.sessions == set()  # cleanup quit the browser session


async def test_scraper_wraps_navigation_timeouts(fake):
    fake.fail[("POST", "/url")] = (500, {"value": {"error": "timeout", "message": "page load"}})
    client = _deep_client(fake, keep_container=True)
    client._is_container_running = lambda: True
    session = ScraperSession("chrome-0", driver=client._create_webdriver())
    with pytest.raises(RuntimeError, match="Chrome scraping failed: timeout: page load"):
        await client._scrape_with_chrome(session, "https://slow.example/", 9)
    client._safe_quit_driver(session.driver)


async def test_scraper_recovers_when_the_session_is_lost(fake):
    client = _deep_client(fake, keep_container=True)
    restarts = []
    client._stop_chrome_container = lambda: restarts.append("stop")
    client._ensure_chrome_container = lambda: restarts.append("start")
    session = ScraperSession("chrome-0", driver=client._create_webdriver())
    fake.sessions.clear()  # Chrome crashed: the endpoint forgot the session

    result = await client._scrape_with_chrome(session, "https://example.com/", 9)
    assert result["title"] == "Fake page"
    assert restarts == ["stop", "start"]
    assert session.driver.session_id == "s2"


def test_create_webdriver_quits_when_setup_fails(fake):
    fake.fail[("POST", "/timeouts")] = (500, {"value": {"error": "unknown error", "message": "x"}})
    client = _deep_client(fake)
    with pytest.raises(WebDriverException):
        client._create_webdriver()
    assert ("DELETE", "/wd/hub/session/s1", None) in fake.requests
    assert fake.sessions == set()


def test_scraper_exports_the_exception_the_callers_catch():
    assert scraper_client.WebDriverException is WebDriverException
