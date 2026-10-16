"""Each deep-crawl run gets its own Chrome container, so parallel jobs can't
reuse and then stop each other's browser."""

import asyncio
import os
import subprocess

import pytest

import scraper_client
from scraper_client import ScraperClient


class _Podman:
    def __init__(self, existing=()):
        self.existing = list(existing)
        self.calls = []

    def __call__(self, client, *args, check=True):
        self.calls.append(args)
        if args[:2] == ("ps", "-a"):
            return subprocess.CompletedProcess(args, 0, "\n".join(self.existing), "")
        return subprocess.CompletedProcess(args, 0, "", "")


@pytest.fixture
def podman(monkeypatch):
    fake = _Podman()
    monkeypatch.setattr(
        ScraperClient, "_podman", lambda self, *a, check=True: fake(self, *a, check=check)
    )
    return fake


def test_each_run_claims_its_own_name_and_ports(podman):
    first = ScraperClient(mode="deep")
    first._claim_private_container()
    assert first.chrome_container_name == f"elcano-lens-chromedriver-{os.getpid()}"
    assert first.chrome_port != 4444 and first.chrome_vnc_port != 7900
    assert first._chrome_endpoint == f"http://127.0.0.1:{first.chrome_port}"


def test_vnc_stays_off_when_disabled(podman):
    client = ScraperClient(mode="deep", chrome_vnc_port=None)
    client._claim_private_container()
    assert client.chrome_vnc_port is None


def test_orphans_from_dead_runs_are_removed(podman, monkeypatch):
    alive, dead = 111, 222
    podman.existing = [
        f"elcano-lens-chromedriver-{alive}",
        f"elcano-lens-chromedriver-{dead}",
        "elcano-lens-chromedriver",  # a keep_container instance: not ours to touch
        "something-else-333",
    ]

    def fake_kill(pid, sig):
        if pid == dead:
            raise ProcessLookupError
        return None

    monkeypatch.setattr(scraper_client.os, "kill", fake_kill)
    ScraperClient(mode="deep")._claim_private_container()
    removed = [args[-1] for args in podman.calls if args[:2] == ("rm", "-f")]
    assert removed == [f"elcano-lens-chromedriver-{dead}"]


def test_keep_container_uses_the_configured_name(monkeypatch, podman):
    ensured = []
    monkeypatch.setattr(
        ScraperClient,
        "_ensure_chrome_container",
        lambda self: ensured.append(self.chrome_container_name),
    )
    client = ScraperClient(mode="deep", keep_container=True)
    asyncio.run(client.__aenter__())
    assert ensured == ["elcano-lens-chromedriver"] and client.chrome_port == 4444


def test_default_deep_run_claims_before_starting(monkeypatch, podman):
    ensured = []
    monkeypatch.setattr(
        ScraperClient,
        "_ensure_chrome_container",
        lambda self: ensured.append(self.chrome_container_name),
    )
    client = ScraperClient(mode="deep")
    asyncio.run(client.__aenter__())
    assert ensured == [f"elcano-lens-chromedriver-{os.getpid()}"]
