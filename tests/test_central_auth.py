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
import central_auth
import central_auth_admin
from central_auth import (
    AccessProvisionEvent,
    AuthKeyResolver,
    CentralAuthClient,
    CentralAuthError,
    CentralAuthStore,
    CodeExchangeRejectedError,
    verify_access_token,
    verify_id_token,
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


def _mint_access(
    private_key: Ed25519PrivateKey,
    *,
    action: str = "grant",
    version: object = 1,
    **overrides: object,
) -> tuple[str, str]:
    public = private_key.public_key().public_bytes_raw()
    kid = base64.urlsafe_b64encode(hashlib.sha256(public).digest()[:16]).rstrip(b"=").decode()
    header = {"typ": "access+jwt", "alg": "EdDSA", "kid": kid}
    claims = {
        "iss": "https://auth.example.com",
        "sub": "account-123",
        "aud": "lens",
        "email": "alice@example.com",
        "iat": 1_000,
        "exp": 1_300,
        "jti": f"access-{version}",
        "events": {
            "urn:elcanotek:event:application-access": {
                "action": action,
                "version": version,
            }
        },
    }
    claims.update(overrides)
    body = f"{_encode(header)}.{_encode(claims)}"
    signature = base64.urlsafe_b64encode(private_key.sign(body.encode())).rstrip(b"=").decode()
    return f"{body}.{signature}", base64.b64encode(public).decode()


def _mint_id_token(
    private_key: Ed25519PrivateKey,
    *,
    header_overrides: dict | None = None,
    **overrides: object,
) -> tuple[str, str]:
    """An id_token shaped like Auth's internal/token/identity.go output."""
    public = private_key.public_key().public_bytes_raw()
    kid = base64.urlsafe_b64encode(hashlib.sha256(public).digest()[:16]).rstrip(b"=").decode()
    header = {"typ": "JWT", "alg": "EdDSA", "kid": kid, **(header_overrides or {})}
    claims = {
        "iss": "https://auth.example.com",
        "sub": "account-123",
        "aud": "lens",
        "email": "Alice@Example.com",
        "iat": 1_000,
        "exp": 1_300,
        "nonce": "expected-nonce",
        "auth_time": 1_000,
        "amr": ["pwd"],
        "acr": "urn:elcanotek:loa:1",
    }
    claims.update(overrides)
    for key in [k for k, v in overrides.items() if v is None]:
        del claims[key]
    body = f"{_encode(header)}.{_encode(claims)}"
    signature = base64.urlsafe_b64encode(private_key.sign(body.encode())).rstrip(b"=").decode()
    return f"{body}.{signature}", base64.b64encode(public).decode()


def _token_endpoint(monkeypatch, body: dict, captured: dict | None = None) -> None:
    """Stand in for Auth's /token: answer every exchange with `body`."""

    class Response:
        def __enter__(self):
            return self

        def __exit__(self, *args):
            return None

        def read(self, _limit):
            return json.dumps(body).encode()

    class Opener:
        def open(self, request, timeout):
            if captured is not None:
                captured["request"] = request
                captured["timeout"] = timeout
            return Response()

    monkeypatch.setattr("central_auth.urllib.request.build_opener", lambda *_args: Opener())


def _client() -> CentralAuthClient:
    return CentralAuthClient(
        issuer_url="https://auth.example.com",
        public_url="https://lens.example.com",
        client_id="lens",
        client_secret="a-random-client-secret-with-32-bytes",
    )


def _static_resolver(*public_keys_b64: str) -> AuthKeyResolver:
    def no_jwks(url, timeout):
        raise OSError("no JWKS in this test")

    return AuthKeyResolver(
        "https://auth.example.com", list(public_keys_b64), fetch=no_jwks, now=lambda: 1_000.0
    )


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


def test_access_token_rejects_wrong_audience_and_invalid_version() -> None:
    private_key = Ed25519PrivateKey.generate()
    raw, public_key = _mint_access(private_key, version=4)

    event = verify_access_token(
        raw,
        issuer="https://auth.example.com",
        audience="lens",
        public_keys=[public_key],
        now=1_010,
    )
    assert event == AccessProvisionEvent(
        "access-4",
        "account-123",
        "alice@example.com",
        "https://auth.example.com",
        1_000,
        4,
        True,
    )

    with pytest.raises(CentralAuthError):
        verify_access_token(
            raw,
            issuer="https://auth.example.com",
            audience="explorer",
            public_keys=[public_key],
            now=1_010,
        )

    malformed, _ = _mint_access(private_key, version=True)
    with pytest.raises(CentralAuthError):
        verify_access_token(
            malformed,
            issuer="https://auth.example.com",
            audience="lens",
            public_keys=[public_key],
            now=1_010,
        )


def test_access_provisioning_is_ordered_and_revocation_ends_sessions(tmp_path) -> None:
    store = CentralAuthStore(tmp_path / "access.db")
    grant = AccessProvisionEvent(
        "grant-2",
        "account-123",
        "alice@example.com",
        "https://auth.example.com",
        1_000,
        2,
        True,
    )
    stale_revoke = AccessProvisionEvent(
        "revoke-1",
        grant.subject,
        grant.email,
        grant.issuer,
        1_001,
        1,
        False,
    )
    revoke = AccessProvisionEvent(
        "revoke-3",
        grant.subject,
        grant.email,
        grant.issuer,
        1_002,
        3,
        False,
    )

    assert store.apply_access_provisioning(grant, now=1_010)
    issued = store.create_session(grant.subject, grant.email, now=1_011)
    assert not store.apply_access_provisioning(stale_revoke, now=1_012)
    assert store.is_allowed(grant.email)

    assert store.apply_access_provisioning(revoke, now=1_013)
    assert not store.is_allowed(grant.email)
    assert store.get_identity(issued.token, now=1_014) is None
    assert not store.apply_access_provisioning(revoke, now=1_015)


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
    assert "revoked alice@example.com" in capsys.readouterr().out

    # Revoking an email that is not allowed changes nothing and must say so.
    monkeypatch.setattr("sys.argv", ["central_auth_admin.py", "revoke", "alice@example.com"])
    assert central_auth_admin.main() == 1
    captured = capsys.readouterr()
    assert "not currently allowed" in captured.err
    assert "revoked" not in captured.out

    # A malformed email is an operator error, not a traceback.
    monkeypatch.setattr("sys.argv", ["central_auth_admin.py", "grant", "not-an-email"])
    assert central_auth_admin.main() == 1
    assert "valid email" in capsys.readouterr().err


def test_code_exchange_uses_basic_pkce_and_validates_identity(monkeypatch) -> None:
    captured: dict = {}
    signer = Ed25519PrivateKey.generate()
    now = int(time.time())
    id_token, public_b64 = _mint_id_token(signer, iat=now, exp=now + 300)
    _token_endpoint(
        monkeypatch,
        {
            "sub": "account-123",
            "email": "Alice@Example.com",
            "nonce": "expected-nonce",
            "iss": "https://auth.example.com",
            "aud": "lens",
            "exp": now + 300,
            "id_token": id_token,
        },
        captured,
    )

    principal = _client().exchange(
        code="single-use-code",
        verifier="pkce-verifier",
        expected_nonce="expected-nonce",
        key_resolver=_static_resolver(public_b64),
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


def test_login_prunes_revoked_and_absolutely_expired_sessions(tmp_path) -> None:
    import sqlite3

    store = CentralAuthStore(tmp_path / "access.db", idle_seconds=60, absolute_seconds=120)
    store.grant_access("alice@example.com", now=1_000)
    revoked = store.create_session("account-1", "alice@example.com", now=1_000)
    assert store.revoke_session(revoked.token, now=1_001)
    store.create_session("account-1", "alice@example.com", now=1_002)  # absolute expiry 1_122
    live = store.create_session("account-1", "alice@example.com", now=1_100)  # still valid at 1_130

    latest = store.create_session("account-1", "alice@example.com", now=1_130)

    with sqlite3.connect(store.path) as connection:
        remaining = {row[0] for row in connection.execute("SELECT token_hash FROM sessions")}
    assert remaining == {
        central_auth._token_hash(live.token),
        central_auth._token_hash(latest.token),
    }
    assert store.get_identity(live.token, now=1_131) is not None
    assert store.get_identity(latest.token, now=1_131) is not None


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
        client.exchange(code="c", verifier="v", expected_nonce="n", key_resolver=_static_resolver())


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


def _jwks_for(*public_keys_b64: str) -> bytes:
    keys = []
    for encoded in public_keys_b64:
        raw = base64.b64decode(encoded)
        keys.append(
            {
                "kty": "OKP",
                "crv": "Ed25519",
                "kid": base64.urlsafe_b64encode(hashlib.sha256(raw).digest()[:16])
                .rstrip(b"=")
                .decode(),
                "x": base64.urlsafe_b64encode(raw).rstrip(b"=").decode(),
            }
        )
    return json.dumps({"keys": keys}).encode()


def test_key_resolver_merges_static_and_published_keys_and_survives_fetch_failure() -> None:
    old_key = Ed25519PrivateKey.generate()
    new_key = Ed25519PrivateKey.generate()
    old_b64 = base64.b64encode(old_key.public_key().public_bytes_raw()).decode()
    new_b64 = base64.b64encode(new_key.public_key().public_bytes_raw()).decode()
    clock = {"now": 1_000.0}
    calls = {"n": 0, "fail": False}

    def fetch(url, timeout):
        calls["n"] += 1
        assert url == "https://auth.example.com/jwks.json"
        if calls["fail"]:
            raise OSError("auth unreachable")
        return _jwks_for(new_b64)

    resolver = AuthKeyResolver(
        "https://auth.example.com/", [old_b64], fetch=fetch, now=lambda: clock["now"]
    )
    assert resolver.public_keys() == [old_b64, new_b64]
    assert calls["n"] == 1
    assert resolver.public_keys() == [old_b64, new_b64]
    assert calls["n"] == 1
    clock["now"] += 11 * 60
    calls["fail"] = True
    assert resolver.public_keys() == [old_b64, new_b64]
    assert calls["n"] == 2

    rotated_key = Ed25519PrivateKey.generate()
    rotated_b64 = base64.b64encode(rotated_key.public_key().public_bytes_raw()).decode()
    raw, _ = _mint_logout(rotated_key)
    resolver._fetch = lambda url, timeout: _jwks_for(new_b64, rotated_b64)
    clock["now"] += 61
    assert rotated_b64 in resolver.keys_for_token(raw)
    fetched_after = resolver._last_attempt
    unknown_raw, _ = _mint_logout(Ed25519PrivateKey.generate())
    resolver.keys_for_token(unknown_raw)
    assert resolver._last_attempt == fetched_after


def test_logout_token_verifies_against_a_key_only_published_in_jwks() -> None:
    signer = Ed25519PrivateKey.generate()
    signer_b64 = base64.b64encode(signer.public_key().public_bytes_raw()).decode()
    raw, _ = _mint_logout(signer)
    resolver = AuthKeyResolver(
        "https://auth.example.com",
        [],
        fetch=lambda url, timeout: _jwks_for(signer_b64),
        now=lambda: 1_000.0,
    )
    event = verify_logout_token(
        raw,
        issuer="https://auth.example.com",
        audience="lens",
        public_keys=resolver.keys_for_token(raw),
        now=1_010,
    )
    assert event.subject == "account-123"


def test_id_token_identity_comes_from_the_signed_token() -> None:
    signer = Ed25519PrivateKey.generate()
    raw, public_b64 = _mint_id_token(signer)
    principal = verify_id_token(
        raw,
        issuer="https://auth.example.com/",
        audience="lens",
        expected_nonce="expected-nonce",
        public_keys=[public_b64],
        now=1_010,
    )
    assert (principal.subject, principal.email) == ("account-123", "alice@example.com")


@pytest.mark.parametrize(
    ("header_overrides", "overrides"),
    [
        ({"typ": "logout+jwt"}, {}),
        ({"typ": None}, {}),
        ({"alg": "none"}, {}),
        ({"alg": "HS256"}, {}),
        ({"kid": "some-other-key"}, {}),
        ({}, {"nonce": "another-login"}),
        ({}, {"nonce": None}),
        ({}, {"aud": "explorer"}),
        ({}, {"aud": ["lens"]}),
        ({}, {"iss": "https://evil.example.com"}),
        ({}, {"iss": "https://auth.example.com/"}),
        ({}, {"iss": None}),
        ({}, {"exp": 900}),
        ({}, {"exp": None}),
        ({}, {"exp": True}),
        ({}, {"iat": None}),
        ({}, {"iat": 5_000}),
        ({}, {"sub": ""}),
        ({}, {"sub": None}),
        ({}, {"email": "not-an-email"}),
        ({}, {"email": None}),
    ],
)
def test_id_token_refuses_anything_but_this_logins_auth_assertion(
    header_overrides, overrides
) -> None:
    signer = Ed25519PrivateKey.generate()
    raw, public_b64 = _mint_id_token(signer, header_overrides=header_overrides, **overrides)
    with pytest.raises(CentralAuthError):
        verify_id_token(
            raw,
            issuer="https://auth.example.com",
            audience="lens",
            expected_nonce="expected-nonce",
            public_keys=[public_b64],
            now=1_010,
        )


def test_id_token_refuses_a_bad_signature_and_an_unknown_key() -> None:
    signer = Ed25519PrivateKey.generate()
    raw, public_b64 = _mint_id_token(signer)
    header, claims, signature = raw.split(".")
    tampered_claims = _encode({**json.loads(base64.urlsafe_b64decode(claims + "==")), "sub": "x"})
    stranger_b64 = base64.b64encode(
        Ed25519PrivateKey.generate().public_key().public_bytes_raw()
    ).decode()
    for token, keys in [
        (f"{header}.{tampered_claims}.{signature}", [public_b64]),
        (f"{header}.{claims}.{signature[:-4]}AAAA", [public_b64]),
        (raw, [stranger_b64]),
        (raw, []),
        (f"{header}.{claims}", [public_b64]),
    ]:
        with pytest.raises(CentralAuthError):
            verify_id_token(
                token,
                issuer="https://auth.example.com",
                audience="lens",
                expected_nonce="expected-nonce",
                public_keys=keys,
                now=1_010,
            )


def test_code_exchange_trusts_only_the_signed_claims(monkeypatch) -> None:
    """The /token JSON repeats the claims unsigned; a response whose unsigned
    copy names someone else still signs in the id_token's user, and one
    without an id_token signs in nobody."""
    signer = Ed25519PrivateKey.generate()
    now = int(time.time())
    id_token, public_b64 = _mint_id_token(signer, iat=now, exp=now + 300)
    unsigned = {
        "sub": "account-999",
        "email": "mallory@example.com",
        "nonce": "expected-nonce",
        "iss": "https://auth.example.com",
        "aud": "lens",
        "exp": now + 300,
    }
    _token_endpoint(monkeypatch, {**unsigned, "id_token": id_token})
    principal = _client().exchange(
        code="c",
        verifier="v",
        expected_nonce="expected-nonce",
        key_resolver=_static_resolver(public_b64),
    )
    assert (principal.subject, principal.email) == ("account-123", "alice@example.com")

    for body in [unsigned, {**unsigned, "id_token": None}, {**unsigned, "id_token": 7}, ["x"]]:
        _token_endpoint(monkeypatch, body)
        with pytest.raises(CentralAuthError):
            _client().exchange(
                code="c",
                verifier="v",
                expected_nonce="expected-nonce",
                key_resolver=_static_resolver(public_b64),
            )


def test_code_exchange_refuses_an_id_token_signed_by_another_key(monkeypatch) -> None:
    configured = Ed25519PrivateKey.generate()
    configured_b64 = base64.b64encode(configured.public_key().public_bytes_raw()).decode()
    now = int(time.time())
    forged, _ = _mint_id_token(Ed25519PrivateKey.generate(), iat=now, exp=now + 300)
    _token_endpoint(monkeypatch, {"id_token": forged})
    with pytest.raises(CentralAuthError):
        _client().exchange(
            code="c",
            verifier="v",
            expected_nonce="expected-nonce",
            key_resolver=_static_resolver(configured_b64),
        )


def test_code_exchange_accepts_a_key_only_published_in_jwks(monkeypatch) -> None:
    """A rotated Auth key needs no env edit: an unknown kid refreshes /jwks.json."""
    static = Ed25519PrivateKey.generate()
    static_b64 = base64.b64encode(static.public_key().public_bytes_raw()).decode()
    rotated = Ed25519PrivateKey.generate()
    now = int(time.time())
    id_token, rotated_b64 = _mint_id_token(rotated, iat=now, exp=now + 300)
    _token_endpoint(monkeypatch, {"id_token": id_token})
    resolver = AuthKeyResolver(
        "https://auth.example.com",
        [static_b64],
        fetch=lambda url, timeout: _jwks_for(static_b64, rotated_b64),
        now=lambda: 1_000.0,
    )
    principal = _client().exchange(
        code="c", verifier="v", expected_nonce="expected-nonce", key_resolver=resolver
    )
    assert principal.subject == "account-123"


def _resign(raw: str, private_key: Ed25519PrivateKey, **header_changes: object) -> str:
    """The same claims under a changed header, validly signed."""
    header_part, claims_part, _ = raw.split(".")
    header = json.loads(base64.urlsafe_b64decode(header_part + "=" * (-len(header_part) % 4)))
    header.update(header_changes)
    body = f"{_encode(header)}.{claims_part}"
    signature = base64.urlsafe_b64encode(private_key.sign(body.encode())).rstrip(b"=").decode()
    return f"{body}.{signature}"


@pytest.mark.parametrize("kid", ["é", "kid with spaces", "", 7, None])
def test_a_malformed_kid_is_an_invalid_token_not_a_crash(kid) -> None:
    signer = Ed25519PrivateKey.generate()
    id_token, public_b64 = _mint_id_token(signer)
    logout, _ = _mint_logout(signer)
    access, _ = _mint_access(signer)
    common = {"issuer": "https://auth.example.com", "audience": "lens", "public_keys": [public_b64]}
    with pytest.raises(CentralAuthError):
        verify_id_token(
            _resign(id_token, signer, kid=kid), expected_nonce="expected-nonce", now=1_010, **common
        )
    with pytest.raises(CentralAuthError):
        verify_logout_token(_resign(logout, signer, kid=kid), now=1_200, **common)
    with pytest.raises(CentralAuthError):
        verify_access_token(_resign(access, signer, kid=kid), now=1_200, **common)


@pytest.mark.parametrize("field", ["nonce", "sub", "email"])
def test_id_token_with_unencodable_text_is_refused_not_a_crash(field) -> None:
    signer = Ed25519PrivateKey.generate()
    value = {"nonce": "\ud800", "sub": "account-\ud800", "email": "al\ud800ice@example.com"}[field]
    raw, public_b64 = _mint_id_token(signer, **{field: value})
    with pytest.raises(CentralAuthError):
        verify_id_token(
            raw,
            issuer="https://auth.example.com",
            audience="lens",
            expected_nonce="expected-nonce",
            public_keys=[public_b64],
            now=1_010,
        )


def test_an_empty_id_token_is_refused_without_fetching_keys(monkeypatch) -> None:
    fetches = []

    def fetch(url, timeout):
        fetches.append(url)
        return _jwks_for()

    resolver = AuthKeyResolver("https://auth.example.com", [], fetch=fetch, now=lambda: 1_000.0)
    _token_endpoint(monkeypatch, {"id_token": ""})
    with pytest.raises(CentralAuthError):
        _client().exchange(
            code="c", verifier="v", expected_nonce="expected-nonce", key_resolver=resolver
        )
    assert fetches == []


def _rotation_fixture(fetch):
    static = Ed25519PrivateKey.generate()
    static_b64 = base64.b64encode(static.public_key().public_bytes_raw()).decode()
    remote = Ed25519PrivateKey.generate()
    remote_b64 = base64.b64encode(remote.public_key().public_bytes_raw()).decode()
    clock = {"now": 1_000.0}
    published = {"keys": [static_b64, remote_b64], "fail": False, "calls": 0}

    def jwks(url, timeout):
        published["calls"] += 1
        if published["fail"]:
            raise OSError("auth unreachable")
        return _jwks_for(*published["keys"])

    resolver = AuthKeyResolver(
        "https://auth.example.com", [static_b64], fetch=fetch or jwks, now=lambda: clock["now"]
    )
    return resolver, clock, published, (static, static_b64), (remote, remote_b64)


def test_a_retired_jwks_key_stops_verifying_once_the_cache_is_stale() -> None:
    resolver, clock, published, _, (remote, remote_b64) = _rotation_fixture(None)
    token, _ = _mint_id_token(remote)
    assert remote_b64 in resolver.keys_for_token(token)
    clock["now"] += 5 * 60
    assert remote_b64 in resolver.keys_for_token(token), "fresh cache is trusted"
    published["keys"] = published["keys"][:1]  # Auth retires the remote key
    clock["now"] += 6 * 60  # past the 10-minute cache
    assert remote_b64 not in resolver.keys_for_token(token)
    with pytest.raises(CentralAuthError):
        verify_id_token(
            token,
            issuer="https://auth.example.com",
            audience="lens",
            expected_nonce="expected-nonce",
            public_keys=resolver.keys_for_token(token),
            now=1_010,
        )


def test_a_cached_jwks_key_survives_a_failed_refresh() -> None:
    resolver, clock, published, _, (remote, remote_b64) = _rotation_fixture(None)
    token, _ = _mint_id_token(remote)
    assert remote_b64 in resolver.keys_for_token(token)
    published["fail"] = True
    clock["now"] += 11 * 60
    assert remote_b64 in resolver.keys_for_token(token)
    assert published["calls"] == 2, "a stale remote key prompts a refresh"


def test_a_static_key_never_waits_on_jwks() -> None:
    def must_not_fetch(url, timeout):
        raise AssertionError("a static-key token fetched /jwks.json")

    resolver, clock, _, (static, static_b64), _ = _rotation_fixture(must_not_fetch)
    token, _ = _mint_id_token(static)
    assert static_b64 in resolver.keys_for_token(token)  # cold cache
    clock["now"] += 60 * 60
    assert static_b64 in resolver.keys_for_token(token)  # long-stale cache
