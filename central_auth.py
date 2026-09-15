# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Central authentication client and Lens-scoped authorization/session store."""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import json
import os
import secrets
import sqlite3
import time
import unicodedata
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

BACKCHANNEL_LOGOUT_EVENT = "http://schemas.openid.net/event/backchannel-logout"
CLOCK_SKEW_SECONDS = 60
# Application sessions are deliberately short. Expiry costs the user only a
# redirect: the code handoff signs them back in silently while the 30-day
# central Auth session is live. The short limit bounds a stolen Lens cookie
# and forces a daily re-check with Auth that the account is still enabled.
# One day absolute, 12 hours idle is the Elcano convention for every
# application session (owner decision 2026-09-15; see Auth's
# docs/AUTH_V2_IMPLEMENTATION.md "Application session conventions").
DEFAULT_IDLE_SECONDS = 12 * 60 * 60
DEFAULT_ABSOLUTE_SECONDS = 24 * 60 * 60
# How often a validated session rewrites last_seen_at / idle_expires_at. Every
# request reads the session; only a request more than this long after the
# previous touch writes. The idle limit therefore behaves as "12 hours minus
# at most one minute", never longer, and a page's burst of requests costs one
# SQLite write instead of one per request. One minute is the convention for
# every Elcano service with its own sessions (Auth, Explorer, Lens, and
# anything built later); keep it a constant, not a setting.
SESSION_TOUCH_SECONDS = 60
LOGIN_TRANSACTION_SECONDS = 10 * 60
MAX_TOKEN_RESPONSE_BYTES = 64 * 1024


class CentralAuthError(Exception):
    """A safe central-auth failure that contains no credential material."""


class AccessDeniedError(CentralAuthError):
    """The central identity is not enabled for this Lens deployment."""


class AuthTransactionError(CentralAuthError):
    """The callback does not match a live browser login transaction."""


class CodeExchangeRejectedError(CentralAuthError):
    """Auth refused the code (consumed, expired, or superseded): a retry, not an outage."""


REVOCATION_EVENT_RETENTION_SECONDS = 7 * 24 * 60 * 60


@dataclass(frozen=True)
class AuthenticatedPrincipal:
    subject: str
    email: str
    nonce: str


@dataclass(frozen=True)
class CentralIdentity:
    subject: str
    email: str


@dataclass(frozen=True)
class IssuedSession:
    token: str


@dataclass(frozen=True)
class LogoutEvent:
    event_id: str
    subject: str
    issuer: str
    issued_at: int


def normalize_email(email: str) -> str:
    normalized = unicodedata.normalize("NFC", email).strip().casefold()
    if not normalized or len(normalized) > 254 or normalized.count("@") != 1:
        raise ValueError("A valid email address is required")
    local, domain = normalized.split("@", 1)
    if not local or not domain or domain.startswith(".") or domain.endswith("."):
        raise ValueError("A valid email address is required")
    if any(unicodedata.category(char).startswith("C") for char in normalized):
        raise ValueError("Email contains unsupported control characters")
    return normalized


def _token_hash(token: str) -> str:
    return hashlib.sha256(token.encode("ascii")).hexdigest()


def csrf_token_for_session(token: str) -> str:
    return hmac.new(token.encode("ascii"), b"lens-central-csrf-v1", hashlib.sha256).hexdigest()


def _decode_b64url(segment: str) -> bytes:
    alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
    if not segment or any(char not in alphabet for char in segment):
        raise CentralAuthError("The logout token was invalid")
    try:
        return base64.b64decode(segment + "=" * (-len(segment) % 4), altchars=b"-_", validate=True)
    except (ValueError, binascii.Error) as exc:
        raise CentralAuthError("The logout token was invalid") from exc


def verify_logout_token(
    raw: str,
    *,
    issuer: str,
    audience: str,
    public_keys: list[str],
    now: int | None = None,
) -> LogoutEvent:
    if len(raw) > 16_384 or len(raw.split(".")) != 3:
        raise CentralAuthError("The logout token was invalid")
    header_part, claims_part, signature_part = raw.split(".")
    try:
        header = json.loads(_decode_b64url(header_part))
        claims = json.loads(_decode_b64url(claims_part))
        signature = _decode_b64url(signature_part)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise CentralAuthError("The logout token was invalid") from exc
    if (
        not isinstance(header, dict)
        or not isinstance(claims, dict)
        or header.get("typ") != "logout+jwt"
        or header.get("alg") != "EdDSA"
    ):
        raise CentralAuthError("The logout token was invalid")

    verified = False
    for encoded in public_keys:
        try:
            public = base64.b64decode(encoded.strip(), validate=True)
            kid = (
                base64.urlsafe_b64encode(hashlib.sha256(public).digest()[:16]).rstrip(b"=").decode()
            )
            if len(public) != 32 or not hmac.compare_digest(str(header.get("kid", "")), kid):
                continue
            Ed25519PublicKey.from_public_bytes(public).verify(
                signature, f"{header_part}.{claims_part}".encode("ascii")
            )
            verified = True
            break
        except (ValueError, InvalidSignature, UnicodeEncodeError):
            continue
    if not verified:
        raise CentralAuthError("The logout token was invalid")

    timestamp = int(time.time() if now is None else now)
    subject, event_id, issued_at = claims.get("sub"), claims.get("jti"), claims.get("iat")
    expires_at = claims.get("exp")
    token_issuer = claims.get("iss")
    events = claims.get("events")
    if (
        isinstance(expires_at, bool)
        or not isinstance(expires_at, int)
        or expires_at + CLOCK_SKEW_SECONDS <= timestamp
        or not isinstance(token_issuer, str)
        or token_issuer.rstrip("/") != issuer.rstrip("/")
        or claims.get("aud") != audience
        or not isinstance(subject, str)
        or not subject
        or len(subject) > 255
        or not isinstance(event_id, str)
        or not event_id
        or len(event_id) > 255
        or isinstance(issued_at, bool)
        or not isinstance(issued_at, int)
        or issued_at <= 0
        or issued_at > timestamp + CLOCK_SKEW_SECONDS
        or not isinstance(events, dict)
        or not isinstance(events.get(BACKCHANNEL_LOGOUT_EVENT), dict)
        or "nonce" in claims
    ):
        raise CentralAuthError("The logout token was invalid")
    return LogoutEvent(event_id, subject, issuer.rstrip("/"), issued_at)


def auth_signing_public_keys() -> list[str]:
    """Statically configured keys: AUTH_SIGNING_PUBKEY plus previous keys."""
    keys = [os.getenv("AUTH_SIGNING_PUBKEY", "")]
    keys.extend(os.getenv("AUTH_SIGNING_PREVIOUS_PUBKEYS", "").split(","))
    return [key.strip() for key in keys if key.strip()]


JWKS_CACHE_SECONDS = 10 * 60
JWKS_MIN_REFRESH_SECONDS = 60
MAX_JWKS_BYTES = 64 * 1024


def _fetch_jwks(url: str, timeout: float) -> bytes:
    request = urllib.request.Request(url, headers={"Accept": "application/json"})
    with urllib.request.build_opener(_NoRedirect).open(request, timeout=timeout) as response:
        raw = response.read(MAX_JWKS_BYTES + 1)
    if len(raw) > MAX_JWKS_BYTES:
        raise CentralAuthError("The JWKS document was too large")
    return raw


class AuthKeyResolver:
    """Auth's Ed25519 public keys: static env keys plus Auth's published JWKS.

    Auth rotates its signing key by publishing the new key alongside the old
    one at /jwks.json. Reading that document here makes rotation a one-sided
    change on Auth instead of an env edit on every application host. Static
    keys stay as the bootstrap and offline fallback: a fetch failure keeps
    whatever was cached, and verification never depends on Auth being up.
    """

    def __init__(
        self,
        issuer_url: str,
        static_keys: list[str],
        *,
        fetch=None,
        timeout_seconds: float = 5,
        now=time.time,
    ) -> None:
        self.jwks_url = issuer_url.rstrip("/") + "/jwks.json"
        self.static_keys = list(static_keys)
        # Looked up at call time when None so tests can replace the module
        # function and production never captures a stale reference.
        self._fetch = fetch
        self._timeout = timeout_seconds
        self._now = now
        self._remote_keys: list[str] = []
        self._fetched_at: float | None = None
        self._last_attempt: float | None = None

    def public_keys(self) -> list[str]:
        if self._fetched_at is None or self._now() - self._fetched_at > JWKS_CACHE_SECONDS:
            self.refresh()
        seen: set[str] = set()
        out: list[str] = []
        for key in self.static_keys + self._remote_keys:
            if key not in seen:
                seen.add(key)
                out.append(key)
        return out

    def refresh(self, *, force: bool = False) -> bool:
        """Fetch the JWKS; True when the cache was updated. Rate-limited to
        once a minute so unknown-kid tokens cannot amplify requests to Auth."""
        now = self._now()
        if (
            not force
            and self._last_attempt is not None
            and now - self._last_attempt < JWKS_MIN_REFRESH_SECONDS
        ):
            return False
        self._last_attempt = now
        fetcher = self._fetch if self._fetch is not None else _fetch_jwks
        try:
            document = json.loads(fetcher(self.jwks_url, self._timeout))
        except (CentralAuthError, urllib.error.URLError, TimeoutError, OSError, ValueError):
            return False
        keys = document.get("keys") if isinstance(document, dict) else None
        if not isinstance(keys, list):
            return False
        parsed: list[str] = []
        for entry in keys:
            if not isinstance(entry, dict) or entry.get("kty") != "OKP":
                continue
            if entry.get("crv") != "Ed25519" or not isinstance(entry.get("x"), str):
                continue
            try:
                raw_key = base64.urlsafe_b64decode(entry["x"] + "=" * (-len(entry["x"]) % 4))
            except (ValueError, binascii.Error):
                continue
            if len(raw_key) == 32:
                parsed.append(base64.b64encode(raw_key).decode("ascii"))
        self._remote_keys = parsed
        self._fetched_at = now
        return True

    def keys_for_token(self, raw_token: str) -> list[str]:
        """Keys to try for one token: refresh once if its kid is unknown."""
        keys = self.public_keys()
        kid = _token_kid(raw_token)
        if kid is not None and not any(_kid_for_key(key) == kid for key in keys):
            if self.refresh():
                keys = self.public_keys()
        return keys


def _token_kid(raw_token: str) -> str | None:
    parts = raw_token.split(".")
    if len(parts) != 3:
        return None
    try:
        header = json.loads(_decode_b64url(parts[0]))
    except (CentralAuthError, UnicodeDecodeError, json.JSONDecodeError):
        return None
    kid = header.get("kid") if isinstance(header, dict) else None
    return kid if isinstance(kid, str) else None


def _kid_for_key(encoded_key: str) -> str | None:
    try:
        raw_key = base64.b64decode(encoded_key.strip(), validate=True)
    except (ValueError, binascii.Error):
        return None
    if len(raw_key) != 32:
        return None
    return base64.urlsafe_b64encode(hashlib.sha256(raw_key).digest()[:16]).rstrip(b"=").decode()


def require_auth_signing_public_keys() -> list[str]:
    """Central mode cannot verify back-channel logout without the auth public key.

    Fail at startup instead of answering every revocation with 400 forever.
    """
    keys = auth_signing_public_keys()
    if not keys:
        raise RuntimeError(
            "AUTH_SIGNING_PUBKEY is required in central mode: run `auth pubkey` on the auth host. "
            "Rotation keys are fetched from Auth's /jwks.json at runtime, but one static key "
            "is needed so verification works even when Auth is unreachable at startup."
        )
    for encoded in keys:
        try:
            raw = base64.b64decode(encoded, validate=True)
        except (ValueError, binascii.Error) as exc:
            raise RuntimeError("AUTH_SIGNING_PUBKEY is not valid base64") from exc
        if len(raw) != 32:
            raise RuntimeError("AUTH_SIGNING_PUBKEY must decode to a 32-byte Ed25519 key")
    return keys


def _origin(name: str, raw: str, *, allow_http: bool = False) -> str:
    parsed = urllib.parse.urlsplit(raw.strip())
    schemes = {"https", "http"} if allow_http else {"https"}
    if (
        parsed.scheme not in schemes
        or not parsed.netloc
        or parsed.username
        or parsed.password
        or parsed.query
        or parsed.fragment
        or parsed.path not in {"", "/"}
    ):
        raise RuntimeError(f"{name} must be an HTTPS origin without a path or query")
    return f"{parsed.scheme}://{parsed.netloc}".rstrip("/")


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class CentralAuthClient:
    def __init__(
        self,
        *,
        issuer_url: str,
        public_url: str,
        client_id: str,
        client_secret: str,
        allow_insecure_http: bool = False,
    ) -> None:
        self.issuer_url = _origin("AUTH_ISSUER_URL", issuer_url, allow_http=allow_insecure_http)
        self.public_url = _origin("LENS_PUBLIC_URL", public_url, allow_http=allow_insecure_http)
        self.client_id = client_id.strip()
        self.client_secret = client_secret
        if not self.client_id or ":" in self.client_id or len(self.client_id) > 128:
            raise RuntimeError("AUTH_CLIENT_ID must be 1-128 characters without ':'")
        if len(client_secret.encode()) < 32 or len(client_secret) > 256 or ":" in client_secret:
            raise RuntimeError("AUTH_CLIENT_SECRET must contain 32-256 bytes without ':'")

    @classmethod
    def from_env(cls) -> CentralAuthClient:
        return cls(
            issuer_url=os.getenv("AUTH_ISSUER_URL", ""),
            public_url=os.getenv("LENS_PUBLIC_URL", ""),
            client_id=os.getenv("AUTH_CLIENT_ID", "lens"),
            client_secret=os.getenv("AUTH_CLIENT_SECRET", ""),
            allow_insecure_http=os.getenv("AUTH_ALLOW_INSECURE_HTTP", "0").lower()
            in {"1", "true", "yes"},
        )

    @property
    def callback_url(self) -> str:
        return f"{self.public_url}/auth/callback"

    def authorization_url(self, *, state: str, code_challenge: str, nonce: str) -> str:
        query = urllib.parse.urlencode(
            {
                "response_type": "code",
                "client_id": self.client_id,
                "redirect_uri": self.callback_url,
                "scope": "openid email",
                "state": state,
                "code_challenge": code_challenge,
                "code_challenge_method": "S256",
                "nonce": nonce,
            }
        )
        return f"{self.issuer_url}/authorize?{query}"

    def exchange(self, *, code: str, verifier: str, expected_nonce: str) -> AuthenticatedPrincipal:
        body = urllib.parse.urlencode(
            {
                "grant_type": "authorization_code",
                "code": code,
                "redirect_uri": self.callback_url,
                "client_id": self.client_id,
                "code_verifier": verifier,
            }
        ).encode()
        credentials = base64.b64encode(f"{self.client_id}:{self.client_secret}".encode()).decode()
        request = urllib.request.Request(
            f"{self.issuer_url}/token",
            data=body,
            method="POST",
            headers={
                "Authorization": f"Basic {credentials}",
                "Content-Type": "application/x-www-form-urlencoded",
                "Accept": "application/json",
                "Cache-Control": "no-store",
            },
        )
        try:
            with urllib.request.build_opener(_NoRedirect).open(request, timeout=10) as response:
                raw = response.read(MAX_TOKEN_RESPONSE_BYTES + 1)
        except urllib.error.HTTPError as exc:
            # 400 is Auth's invalid_grant: consumed, expired, or superseded by a
            # newer /authorize for this browser (two tabs). The user retries.
            if exc.code == 400:
                raise CodeExchangeRejectedError(
                    "The authentication service rejected the sign-in code"
                ) from exc
            raise CentralAuthError("The authentication service rejected the code exchange") from exc
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise CentralAuthError("The authentication service rejected the code exchange") from exc
        if len(raw) > MAX_TOKEN_RESPONSE_BYTES:
            raise CentralAuthError("The authentication response was invalid")
        try:
            payload = json.loads(raw)
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise CentralAuthError("The authentication response was invalid") from exc
        if not isinstance(payload, dict):
            raise CentralAuthError("The authentication response was invalid")
        now = int(time.time())
        subject, email, nonce = payload.get("sub"), payload.get("email"), payload.get("nonce")
        token_issuer = payload.get("iss")
        if (
            not isinstance(subject, str)
            or not subject
            or not isinstance(email, str)
            or not isinstance(nonce, str)
            or not hmac.compare_digest(nonce.encode(), expected_nonce.encode())
            or not isinstance(token_issuer, str)
            or token_issuer.rstrip("/") != self.issuer_url
            or payload.get("aud") != self.client_id
            or isinstance(payload.get("exp"), bool)
            or not isinstance(payload.get("exp"), int)
            or payload["exp"] + CLOCK_SKEW_SECONDS <= now
        ):
            raise CentralAuthError("The authentication response was invalid")
        try:
            normalized = normalize_email(email)
        except ValueError as exc:
            raise CentralAuthError("The authentication response was invalid") from exc
        return AuthenticatedPrincipal(subject, normalized, nonce)


class CentralAuthStore:
    def __init__(
        self,
        path: str | Path,
        *,
        idle_seconds: int = DEFAULT_IDLE_SECONDS,
        absolute_seconds: int = DEFAULT_ABSOLUTE_SECONDS,
    ) -> None:
        self.path = Path(path)
        self.idle_seconds = idle_seconds
        self.absolute_seconds = absolute_seconds
        if idle_seconds <= 0 or absolute_seconds <= 0 or idle_seconds > absolute_seconds:
            raise ValueError("Invalid session timeouts")
        self._initialize()

    @classmethod
    def from_env(cls) -> CentralAuthStore:
        return cls(
            os.getenv("LENS_ACCESS_DB", "/var/lib/lens/access.db"),
            idle_seconds=int(os.getenv("LENS_SESSION_IDLE_SECONDS", str(DEFAULT_IDLE_SECONDS))),
            absolute_seconds=int(
                os.getenv("LENS_SESSION_ABSOLUTE_SECONDS", str(DEFAULT_ABSOLUTE_SECONDS))
            ),
        )

    def _connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(self.path, timeout=5)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA busy_timeout = 5000")
        return connection

    def _initialize(self) -> None:
        self.path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        with self._connect() as connection:
            connection.execute("PRAGMA journal_mode = WAL")
            connection.executescript(
                """
                CREATE TABLE IF NOT EXISTS access_entries (
                    email TEXT PRIMARY KEY COLLATE NOCASE,
                    enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0, 1)),
                    created_at INTEGER NOT NULL,
                    updated_at INTEGER NOT NULL
                );
                CREATE TABLE IF NOT EXISTS sessions (
                    token_hash TEXT PRIMARY KEY,
                    subject TEXT NOT NULL,
                    email TEXT NOT NULL,
                    created_at INTEGER NOT NULL,
                    last_seen_at INTEGER NOT NULL,
                    idle_expires_at INTEGER NOT NULL,
                    absolute_expires_at INTEGER NOT NULL,
                    revoked_at INTEGER
                );
                CREATE INDEX IF NOT EXISTS sessions_subject_idx ON sessions(subject);
                CREATE TABLE IF NOT EXISTS revocation_events (
                    event_id TEXT PRIMARY KEY,
                    issuer TEXT NOT NULL,
                    subject TEXT NOT NULL,
                    issued_at INTEGER NOT NULL,
                    received_at INTEGER NOT NULL
                );
                """
            )
        os.chmod(self.path, 0o600)

    def grant_access(self, email: str, *, now: int | None = None) -> None:
        normalized = normalize_email(email)
        timestamp = int(time.time() if now is None else now)
        with self._connect() as connection:
            connection.execute(
                """INSERT INTO access_entries(email, enabled, created_at, updated_at)
                VALUES(?, 1, ?, ?) ON CONFLICT(email) DO UPDATE
                SET enabled = 1, updated_at = excluded.updated_at""",
                (normalized, timestamp, timestamp),
            )

    def revoke_access(self, email: str, *, now: int | None = None) -> bool:
        normalized = normalize_email(email)
        timestamp = int(time.time() if now is None else now)
        with self._connect() as connection:
            connection.execute("BEGIN IMMEDIATE")
            cursor = connection.execute(
                "UPDATE access_entries SET enabled = 0, updated_at = ? WHERE email = ? AND enabled = 1",
                (timestamp, normalized),
            )
            connection.execute(
                "UPDATE sessions SET revoked_at = ? WHERE email = ? AND revoked_at IS NULL",
                (timestamp, normalized),
            )
        return cursor.rowcount > 0

    def list_access(self) -> list[str]:
        with self._connect() as connection:
            rows = connection.execute(
                "SELECT email FROM access_entries WHERE enabled = 1 ORDER BY email"
            ).fetchall()
        return [str(row["email"]) for row in rows]

    def is_allowed(self, email: str) -> bool:
        try:
            normalized = normalize_email(email)
        except ValueError:
            return False
        with self._connect() as connection:
            row = connection.execute(
                "SELECT enabled FROM access_entries WHERE email = ?", (normalized,)
            ).fetchone()
        return row is not None and bool(row["enabled"])

    def create_session(self, subject: str, email: str, *, now: int | None = None) -> IssuedSession:
        normalized = normalize_email(email)
        if not subject or len(subject) > 255:
            raise CentralAuthError("The authenticated subject was invalid")
        timestamp = int(time.time() if now is None else now)
        token = secrets.token_urlsafe(32)
        absolute = timestamp + self.absolute_seconds
        idle = min(timestamp + self.idle_seconds, absolute)
        with self._connect() as connection:
            connection.execute("BEGIN IMMEDIATE")
            allowed = connection.execute(
                "SELECT 1 FROM access_entries WHERE email = ? AND enabled = 1", (normalized,)
            ).fetchone()
            if allowed is None:
                raise AccessDeniedError("Email is not on this Lens access list")
            connection.execute(
                """INSERT INTO sessions(token_hash, subject, email, created_at, last_seen_at,
                idle_expires_at, absolute_expires_at, revoked_at) VALUES(?, ?, ?, ?, ?, ?, ?, NULL)""",
                (_token_hash(token), subject, normalized, timestamp, timestamp, idle, absolute),
            )
        return IssuedSession(token)

    def get_identity(self, token: str | None, *, now: int | None = None) -> CentralIdentity | None:
        if not token:
            return None
        try:
            token_hash = _token_hash(token)
        except (UnicodeEncodeError, ValueError):
            return None
        timestamp = int(time.time() if now is None else now)
        with self._connect() as connection:
            # Plain read first: most requests fall inside the touch interval
            # and must not take the write lock.
            row = connection.execute(
                """SELECT s.subject, s.email, s.last_seen_at, s.idle_expires_at,
                          s.absolute_expires_at, a.enabled
                FROM sessions s JOIN access_entries a ON a.email = s.email
                WHERE s.token_hash = ? AND s.revoked_at IS NULL""",
                (token_hash,),
            ).fetchone()
            if (
                row is None
                or not row["enabled"]
                or timestamp >= row["idle_expires_at"]
                or timestamp >= row["absolute_expires_at"]
            ):
                if row is not None:
                    connection.execute(
                        "UPDATE sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL",
                        (timestamp, token_hash),
                    )
                return None
            if timestamp - int(row["last_seen_at"]) >= SESSION_TOUCH_SECONDS:
                next_idle = min(timestamp + self.idle_seconds, row["absolute_expires_at"])
                connection.execute(
                    """UPDATE sessions SET last_seen_at = ?, idle_expires_at = ?
                    WHERE token_hash = ? AND revoked_at IS NULL""",
                    (timestamp, next_idle, token_hash),
                )
        return CentralIdentity(str(row["subject"]), str(row["email"]))

    def revoke_session(self, token: str | None, *, now: int | None = None) -> bool:
        if not token:
            return False
        timestamp = int(time.time() if now is None else now)
        try:
            token_hash = _token_hash(token)
        except (UnicodeEncodeError, ValueError):
            return False
        with self._connect() as connection:
            cursor = connection.execute(
                "UPDATE sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL",
                (timestamp, token_hash),
            )
        return cursor.rowcount > 0

    def consume_logout_event(
        self,
        event_id: str,
        issuer: str,
        subject: str,
        issued_at: int,
        *,
        now: int | None = None,
    ) -> bool:
        timestamp = int(time.time() if now is None else now)
        with self._connect() as connection:
            connection.execute("BEGIN IMMEDIATE")
            # Replay protection only needs to outlive a token's acceptance
            # window (exp plus skew, minutes). Keep a week for forensics.
            connection.execute(
                "DELETE FROM revocation_events WHERE received_at < ?",
                (timestamp - REVOCATION_EVENT_RETENTION_SECONDS,),
            )
            cursor = connection.execute(
                """INSERT OR IGNORE INTO revocation_events(event_id, issuer, subject, issued_at, received_at)
                VALUES(?, ?, ?, ?, ?)""",
                (event_id, issuer, subject, issued_at, timestamp),
            )
            if cursor.rowcount:
                connection.execute(
                    "UPDATE sessions SET revoked_at = ? WHERE subject = ? AND revoked_at IS NULL",
                    (timestamp, subject),
                )
        return cursor.rowcount > 0
