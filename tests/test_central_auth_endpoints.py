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
from central_auth import (
    AuthenticatedPrincipal,
    CodeExchangeRejectedError,
    csrf_token_for_session,
)


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
        # The Secure flag on this cookie is fixed when the SessionMiddleware is
        # constructed at import, from LENS_AUTH_COOKIE_SECURE (default on);
        # this test's monkeypatched env cannot change it, so the flag itself
        # is covered by test_transaction_cookie_is_secure_by_default.
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


@pytest.mark.asyncio
async def test_startup_and_health_fail_when_central_auth_is_misconfigured(
    monkeypatch, tmp_path
) -> None:
    monkeypatch.setenv("LENS_AUTH_MODE", "central")
    monkeypatch.setenv("LENS_SESSION_SECRET", "test-session-secret-with-at-least-32-bytes")
    monkeypatch.setenv("LENS_ACCESS_DB", str(tmp_path / "access.db"))
    monkeypatch.setenv("AUTH_ISSUER_URL", "https://auth.example.com")
    monkeypatch.setenv("LENS_PUBLIC_URL", "https://lens.example.com")
    monkeypatch.setenv("AUTH_CLIENT_ID", "lens")
    monkeypatch.setenv("AUTH_CLIENT_SECRET", "a-test-secret-that-is-at-least-32-bytes")
    monkeypatch.setenv("AUTH_SIGNING_PUBKEY", "")
    monkeypatch.delenv("AUTH_SIGNING_PREVIOUS_PUBKEYS", raising=False)
    auth_provider.clear_provider_cache()

    with pytest.raises(RuntimeError, match="AUTH_SIGNING_PUBKEY"):
        async with web_service._lifespan(web_service.app):
            pass
    with pytest.raises(RuntimeError, match="AUTH_SIGNING_PUBKEY"):
        await web_service.health()

    auth_provider.clear_provider_cache()


@pytest.mark.asyncio
async def test_central_logout_stays_signed_out_until_the_user_chooses_to_sign_in(
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
    auth_provider.clear_provider_cache()
    provider = auth_provider.get_auth_provider()
    provider.store.grant_access("alice@example.com", now=1_000)
    session = provider.store.create_session("account-123", "alice@example.com", now=1_000)

    transport = httpx.ASGITransport(app=web_service.app)
    async with httpx.AsyncClient(transport=transport, base_url="http://lens.example.com") as client:
        client.cookies.set("lens_session", session.token, domain="lens.example.com", path="/")
        response = await client.post(
            "/logout",
            data={"csrf_token": csrf_token_for_session(session.token)},
            follow_redirects=False,
        )

        assert response.status_code == 303
        assert response.headers["location"] == "/signed-out"
        assert provider.store.get_identity(session.token) is None
        assert client.cookies.get("lens_session") is None

        signed_out = await client.get(response.headers["location"], follow_redirects=False)
        assert signed_out.status_code == 200
        assert "You are signed out" in signed_out.text
        assert 'href="/auth/login?next=%2F"' in signed_out.text
        assert "location" not in signed_out.headers

    auth_provider.clear_provider_cache()


@pytest.mark.asyncio
async def test_health_reports_the_validated_auth_mode(monkeypatch) -> None:
    monkeypatch.setenv("LENS_AUTH_MODE", "elcano")
    auth_provider.clear_provider_cache()

    payload = await web_service.health()

    assert payload["status"] == "ok"
    assert payload["auth_mode"] == "elcano"
    auth_provider.clear_provider_cache()


@pytest.mark.asyncio
async def test_legacy_logout_still_uses_the_elcano_auth_logout(monkeypatch) -> None:
    monkeypatch.setenv("LENS_AUTH_MODE", "elcano")
    monkeypatch.setattr(auth_provider.auth_cookie, "AUTH_LOGIN_URL", "https://auth.elcanotek.com")
    auth_provider.clear_provider_cache()

    transport = httpx.ASGITransport(app=web_service.app)
    async with httpx.AsyncClient(
        transport=transport, base_url="https://lens.example.com"
    ) as client:
        response = await client.post("/logout", follow_redirects=False)

    assert response.status_code == 303
    assert response.headers["location"] == "https://auth.elcanotek.com/logout"
    auth_provider.clear_provider_cache()


@pytest.mark.asyncio
async def test_pages_carry_nonce_csp_and_other_responses_a_closed_one(
    monkeypatch, tmp_path
) -> None:
    import re

    monkeypatch.delenv("LENS_AUTH_MODE", raising=False)
    monkeypatch.setenv("AUTH_SIGNING_PUBKEY", "")
    auth_provider.clear_provider_cache()
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir()
    output_dir.mkdir()
    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)
    monkeypatch.setattr(web_service, "manager", web_service.JobManager())
    monkeypatch.setattr(
        "auth_cookie.current_identity", lambda _request: {"email": "alice@example.com"}
    )
    transport = httpx.ASGITransport(app=web_service.app)
    async with httpx.AsyncClient(
        transport=transport, base_url="https://lens.example.com"
    ) as client:
        page = await client.get("/")
        assert page.status_code == 200
        csp = page.headers["content-security-policy"]
        match = re.search(r"script-src 'self' 'nonce-([A-Za-z0-9_-]+)'", csp)
        assert match, csp
        nonce = match.group(1)
        assert "default-src 'self'" in csp
        assert "style-src 'self';" in csp
        assert "unsafe-inline" not in csp
        assert "frame-ancestors 'none'" in csp
        assert "form-action" not in csp
        body = page.text
        assert ' style="' not in body
        # Every inline script carries this response's nonce; none is bare.
        assert "<script>" not in body
        assert f'<script nonce="{nonce}">' in body
        assert not re.search(r' on[a-z]+="', body)
        second = await client.get("/")
        assert second.headers["content-security-policy"] != csp

        health = await client.get("/health")
        assert health.headers["content-security-policy"] == web_service.CSP_NON_PAGE


def test_transaction_cookie_is_secure_by_default(monkeypatch) -> None:
    monkeypatch.delenv("LENS_AUTH_COOKIE_SECURE", raising=False)
    assert web_service._cookie_secure() is True
    monkeypatch.setenv("LENS_AUTH_COOKIE_SECURE", "0")
    assert web_service._cookie_secure() is False
