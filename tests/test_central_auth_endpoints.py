# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

import base64
import hashlib
import json
from urllib.parse import parse_qs, urlsplit

import httpx
import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

import auth_provider
import web_service
from central_auth import AuthenticatedPrincipal, CodeExchangeRejectedError


class FakeCentralClient:
    issuer_url = "http://auth.example.com"
    client_id = "lens"
    exchange_error: Exception | None = None

    @classmethod
    def from_env(cls):
        return cls()

    def authorization_url(self, *, state: str, code_challenge: str, nonce: str) -> str:
        return "http://auth.example.com/authorize?" + auth_provider.urllib.parse.urlencode(
            {"state": state, "code_challenge": code_challenge, "nonce": nonce}
        )

    def exchange(self, *, code: str, verifier: str, expected_nonce: str):
        if type(self).exchange_error is not None:
            raise type(self).exchange_error
        assert code == "one-time-code"
        assert verifier
        return AuthenticatedPrincipal("account-123", "alice@example.com", expected_nonce)


def _logout_token(private_key: Ed25519PrivateKey) -> str:
    public = private_key.public_key().public_bytes_raw()
    kid = base64.urlsafe_b64encode(hashlib.sha256(public).digest()[:16]).rstrip(b"=").decode()

    def encode(value):
        return (
            base64.urlsafe_b64encode(json.dumps(value, separators=(",", ":")).encode())
            .rstrip(b"=")
            .decode()
        )

    body = f"{encode({'typ': 'logout+jwt', 'alg': 'EdDSA', 'kid': kid})}.{encode({'iss': 'http://auth.example.com', 'sub': 'account-123', 'aud': 'lens', 'iat': 1000, 'exp': 2_000_000_000, 'jti': 'event-123', 'events': {'http://schemas.openid.net/event/backchannel-logout': {}}})}"
    signature = base64.urlsafe_b64encode(private_key.sign(body.encode())).rstrip(b"=").decode()
    return f"{body}.{signature}"


@pytest.mark.asyncio
async def test_central_login_creates_lens_only_session_and_backchannel_revokes(
    monkeypatch, tmp_path
) -> None:
    private_key = Ed25519PrivateKey.generate()
    monkeypatch.setenv("LENS_AUTH_MODE", "central")
    monkeypatch.setenv("LENS_SESSION_SECRET", "test-session-secret-with-at-least-32-bytes")
    monkeypatch.setenv("LENS_ACCESS_DB", str(tmp_path / "access.db"))
    monkeypatch.setenv("LENS_AUTH_COOKIE_SECURE", "0")
    monkeypatch.setenv("AUTH_ALLOW_INSECURE_HTTP", "1")
    monkeypatch.setenv("AUTH_ISSUER_URL", "http://auth.example.com")
    monkeypatch.setenv("LENS_PUBLIC_URL", "http://lens.example.com")
    monkeypatch.setenv("AUTH_CLIENT_ID", "lens")
    monkeypatch.setenv("AUTH_CLIENT_SECRET", "a-test-secret-that-is-at-least-32-bytes")
    monkeypatch.setenv(
        "AUTH_SIGNING_PUBKEY",
        base64.b64encode(private_key.public_key().public_bytes_raw()).decode(),
    )
    monkeypatch.setattr(auth_provider, "CentralAuthClient", FakeCentralClient)
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir()
    output_dir.mkdir()
    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)
    monkeypatch.setattr(web_service, "manager", web_service.JobManager())
    auth_provider.clear_provider_cache()
    provider = auth_provider.get_auth_provider()
    provider.store.grant_access("alice@example.com", now=1_000)

    transport = httpx.ASGITransport(app=web_service.app)
    async with httpx.AsyncClient(
        transport=transport, base_url="https://lens.example.com"
    ) as client:
        start = await client.get("/auth/login", follow_redirects=False)
        transaction_cookie = start.headers.get("set-cookie", "").lower()
        assert "httponly" in transaction_cookie
        assert "secure" in transaction_cookie
        assert "samesite=lax" in transaction_cookie
        assert "max-age=600" in transaction_cookie
        query = parse_qs(urlsplit(start.headers["location"]).query)
        callback = await client.get(
            "/auth/callback",
            params={"code": "one-time-code", "state": query["state"][0]},
            follow_redirects=False,
        )
        assert callback.status_code == 303
        session = client.cookies.get("lens_session")
        assert session
        assert provider.store.get_identity(session) is not None

        logout = await client.post(
            "/auth/backchannel-logout", data={"logout_token": _logout_token(private_key)}
        )
        assert logout.status_code == 204
        assert provider.store.get_identity(session) is None

        oversized = await client.post(
            "/auth/backchannel-logout", data={"logout_token": "x" * 20_001}
        )
        assert oversized.status_code == 413

        # A consumed or superseded code is a retry for the user, not an outage.
        FakeCentralClient.exchange_error = CodeExchangeRejectedError("consumed")
        try:
            start = await client.get("/auth/login", follow_redirects=False)
            query = parse_qs(urlsplit(start.headers["location"]).query)
            rejected = await client.get(
                "/auth/callback",
                params={"code": "one-time-code", "state": query["state"][0]},
                follow_redirects=False,
            )
        finally:
            FakeCentralClient.exchange_error = None
        assert rejected.status_code == 400
        assert "Try again" in rejected.text


def test_legacy_magic_mode_remains_default(monkeypatch) -> None:
    monkeypatch.delenv("LENS_AUTH_MODE", raising=False)
    auth_provider.clear_provider_cache()
    assert auth_provider.get_auth_provider().mode == "elcano"
