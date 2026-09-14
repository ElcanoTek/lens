# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

import base64
import hashlib
import json
import time
import urllib.error
import urllib.parse

import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

import auth_provider
import central_auth_admin
from central_auth import (
    CentralAuthClient,
    CentralAuthError,
    CentralAuthStore,
    CodeExchangeRejectedError,
    verify_logout_token,
)


def _encode(value: object) -> str:
    return (
        base64.urlsafe_b64encode(json.dumps(value, separators=(",", ":")).encode())
        .rstrip(b"=")
        .decode()
    )


def _mint_logout(
    private_key: Ed25519PrivateKey,
    *,
    audience: str = "lens",
    issuer: object = "https://auth.example.com",
    **overrides: object,
) -> tuple[str, str]:
    public = private_key.public_key().public_bytes_raw()
    kid = base64.urlsafe_b64encode(hashlib.sha256(public).digest()[:16]).rstrip(b"=").decode()
    header = {"typ": "logout+jwt", "alg": "EdDSA", "kid": kid}
    claims = {
        "iss": issuer,
        "sub": "account-123",
        "aud": audience,
        "iat": 1_000,
        "exp": 1_300,
        "jti": "event-123",
        "events": {"http://schemas.openid.net/event/backchannel-logout": {}},
    }
    claims.update(overrides)
    for key in [k for k, v in overrides.items() if v is None]:
        del claims[key]
    body = f"{_encode(header)}.{_encode(claims)}"
    signature = base64.urlsafe_b64encode(private_key.sign(body.encode())).rstrip(b"=").decode()
    return f"{body}.{signature}", base64.b64encode(public).decode()


def test_local_allowlist_sessions_and_backchannel_revocation(tmp_path) -> None:
    store = CentralAuthStore(tmp_path / "access.db")
    assert not store.is_allowed("alice@example.com")
    store.grant_access("Alice@Example.com", now=1_000)
    first = store.create_session("account-123", "alice@example.com", now=1_000)
    other = store.create_session("account-456", "alice@example.com", now=1_000)

    assert store.consume_logout_event(
        "event-123", "https://auth.example.com", "account-123", 1_001, now=1_002
    )
    assert not store.consume_logout_event(
        "event-123", "https://auth.example.com", "account-123", 1_001, now=1_003
    )
    assert store.get_identity(first.token, now=1_004) is None
    assert store.get_identity(other.token, now=1_004) is not None


def test_logout_token_rejects_wrong_audience() -> None:
    private_key = Ed25519PrivateKey.generate()
    raw, public_key = _mint_logout(private_key)
    event = verify_logout_token(
        raw,
        issuer="https://auth.example.com",
        audience="lens",
        public_keys=[public_key],
        now=1_010,
    )
    assert event.event_id == "event-123"

    with pytest.raises(CentralAuthError):
        verify_logout_token(
            raw,
            issuer="https://auth.example.com",
            audience="explorer",
            public_keys=[public_key],
            now=1_010,
        )

    malformed_issuer, _ = _mint_logout(private_key, issuer=123)
    with pytest.raises(CentralAuthError):
        verify_logout_token(
            malformed_issuer,
            issuer="https://auth.example.com",
            audience="lens",
            public_keys=[public_key],
            now=1_010,
        )


def test_access_admin_cli_grants_lists_and_revokes(monkeypatch, tmp_path, capsys) -> None:
    monkeypatch.setenv("LENS_ACCESS_DB", str(tmp_path / "access.db"))
    monkeypatch.setattr("sys.argv", ["central_auth_admin.py", "grant", "Alice@Example.com"])
    assert central_auth_admin.main() == 0

    monkeypatch.setattr("sys.argv", ["central_auth_admin.py", "list"])
    assert central_auth_admin.main() == 0
    assert "alice@example.com" in capsys.readouterr().out

    monkeypatch.setattr("sys.argv", ["central_auth_admin.py", "revoke", "alice@example.com"])
    assert central_auth_admin.main() == 0
    assert not CentralAuthStore.from_env().is_allowed("alice@example.com")


def test_code_exchange_uses_basic_pkce_and_validates_identity(monkeypatch) -> None:
    captured = {}

    class Response:
        def __enter__(self):
            return self

        def __exit__(self, *args):
            return None

        def read(self, _limit):
            return json.dumps(
                {
                    "sub": "account-123",
                    "email": "Alice@Example.com",
                    "nonce": "expected-nonce",
                    "iss": "https://auth.example.com",
                    "aud": "lens",
                    "exp": int(time.time()) + 300,
                }
            ).encode()

    class Opener:
        def open(self, request, timeout):
            captured["request"] = request
            captured["timeout"] = timeout
            return Response()

    monkeypatch.setattr("central_auth.urllib.request.build_opener", lambda *_args: Opener())
    client = CentralAuthClient(
        issuer_url="https://auth.example.com",
        public_url="https://lens.example.com",
        client_id="lens",
        client_secret="a-random-client-secret-with-32-bytes",
    )

    principal = client.exchange(
        code="single-use-code",
        verifier="pkce-verifier",
        expected_nonce="expected-nonce",
    )

    assert principal.email == "alice@example.com"
    assert captured["timeout"] == 10
    request = captured["request"]
    assert request.full_url == "https://auth.example.com/token"
    assert request.get_header("Authorization").startswith("Basic ")
    assert urllib.parse.parse_qs(request.data.decode()) == {
        "grant_type": ["authorization_code"],
        "code": ["single-use-code"],
        "redirect_uri": ["https://lens.example.com/auth/callback"],
        "client_id": ["lens"],
        "code_verifier": ["pkce-verifier"],
    }


@pytest.mark.parametrize(
    "overrides", [{"exp": None}, {"exp": 1_000}, {"exp": "soon"}, {"exp": True}]
)
def test_logout_token_requires_a_live_expiry(overrides) -> None:
    private_key = Ed25519PrivateKey.generate()
    raw, public_key = _mint_logout(private_key, **overrides)
    with pytest.raises(CentralAuthError):
        verify_logout_token(
            raw,
            issuer="https://auth.example.com",
            audience="lens",
            public_keys=[public_key],
            now=1_200,
        )


def test_replay_table_is_pruned_after_retention(tmp_path) -> None:
    import sqlite3

    store = CentralAuthStore(tmp_path / "access.db")
    week = 7 * 24 * 60 * 60
    assert store.consume_logout_event("old", "https://auth.example.com", "a", 1_000, now=1_000)
    assert store.consume_logout_event(
        "new", "https://auth.example.com", "b", 1_000 + week, now=1_001 + week
    )
    with sqlite3.connect(store.path) as connection:
        ids = {row[0] for row in connection.execute("SELECT event_id FROM revocation_events")}
    assert ids == {"new"}


def test_http_400_from_token_endpoint_is_a_rejected_code(monkeypatch) -> None:
    class Opener:
        def open(self, request, timeout):
            raise urllib.error.HTTPError(request.full_url, 400, "Bad Request", {}, None)

    monkeypatch.setattr("central_auth.urllib.request.build_opener", lambda *_args: Opener())
    client = CentralAuthClient(
        issuer_url="https://auth.example.com",
        public_url="https://lens.example.com",
        client_id="lens",
        client_secret="a-random-client-secret-with-32-bytes",
    )
    with pytest.raises(CodeExchangeRejectedError):
        client.exchange(code="c", verifier="v", expected_nonce="n")


def test_safe_local_path_bounds_length_and_control_characters() -> None:
    assert auth_provider._safe_local_path("/jobs?x=1") == "/jobs?x=1"
    assert auth_provider._safe_local_path("/" + "a" * 2048) == "/"
    assert auth_provider._safe_local_path("/jobs\r\nSet-Cookie: x") == "/"
    assert auth_provider._safe_local_path("//evil.example") == "/"


def test_central_mode_refuses_to_start_without_the_auth_public_key(monkeypatch, tmp_path) -> None:
    monkeypatch.setenv("LENS_AUTH_MODE", "central")
    monkeypatch.setenv("LENS_SESSION_SECRET", "test-session-secret-with-at-least-32-bytes")
    monkeypatch.setenv("LENS_ACCESS_DB", str(tmp_path / "access.db"))
    monkeypatch.setenv("AUTH_ISSUER_URL", "https://auth.example.com")
    monkeypatch.setenv("LENS_PUBLIC_URL", "https://lens.example.com")
    monkeypatch.setenv("AUTH_CLIENT_SECRET", "a-test-secret-that-is-at-least-32-bytes")
    monkeypatch.setenv("AUTH_SIGNING_PUBKEY", "")
    monkeypatch.delenv("AUTH_SIGNING_PREVIOUS_PUBKEYS", raising=False)
    auth_provider.clear_provider_cache()
    with pytest.raises(RuntimeError, match="AUTH_SIGNING_PUBKEY"):
        auth_provider.get_auth_provider()
    auth_provider.clear_provider_cache()


def test_session_touch_is_rate_limited_to_one_write_per_minute(tmp_path) -> None:
    import sqlite3

    from central_auth import _token_hash

    store = CentralAuthStore(tmp_path / "access.db")
    store.grant_access("alice@example.com", now=1_000)
    issued = store.create_session("account-123", "alice@example.com", now=1_000)

    def stamps():
        with sqlite3.connect(store.path) as connection:
            return connection.execute(
                "SELECT last_seen_at, idle_expires_at FROM sessions WHERE token_hash = ?",
                (_token_hash(issued.token),),
            ).fetchone()

    assert stamps() == (1_000, 1_000 + store.idle_seconds)
    assert store.get_identity(issued.token, now=1_030) is not None
    assert store.get_identity(issued.token, now=1_059) is not None
    assert stamps() == (1_000, 1_000 + store.idle_seconds)
    assert store.get_identity(issued.token, now=1_060) is not None
    assert stamps() == (1_060, 1_060 + store.idle_seconds)
    assert store.get_identity(issued.token, now=1_060 + store.idle_seconds) is None
