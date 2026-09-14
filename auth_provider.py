# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Opt-in central auth alongside Lens's existing Elcano magic-link cookie."""

from __future__ import annotations

import base64
import hashlib
import hmac
import os
import secrets
import time
import urllib.parse
from typing import Protocol

from fastapi import Request
from fastapi.responses import RedirectResponse

import auth_cookie
from central_auth import (
    LOGIN_TRANSACTION_SECONDS,
    AuthTransactionError,
    CentralAuthClient,
    CentralAuthStore,
    CentralIdentity,
    csrf_token_for_session,
    require_auth_signing_public_keys,
)


class AuthProvider(Protocol):
    mode: str

    def identity(self, request: Request) -> dict | CentralIdentity | None: ...

    def unauthenticated_response(self, request: Request) -> RedirectResponse: ...


class ElcanoAuthProvider:
    """The unchanged shared magic-link cookie path."""

    mode = "elcano"

    def identity(self, request: Request) -> dict | None:
        return auth_cookie.current_identity(request)

    def unauthenticated_response(self, request: Request) -> RedirectResponse:
        return auth_cookie.login_redirect(request)

    def logout_response(self) -> RedirectResponse:
        return RedirectResponse(url=f"{auth_cookie.AUTH_LOGIN_URL}/logout", status_code=303)


class CentralAuthProvider:
    mode = "central"

    def __init__(
        self, store: CentralAuthStore, client: CentralAuthClient, *, cookie_secure: bool = True
    ) -> None:
        self.store = store
        self.client = client
        self.cookie_secure = cookie_secure
        self.cookie_name = "__Host-lens_session" if cookie_secure else "lens_session"

    @classmethod
    def from_env(cls) -> CentralAuthProvider:
        secure = os.getenv("LENS_AUTH_COOKIE_SECURE", "1").strip().lower() in {
            "1",
            "true",
            "yes",
            "on",
        }
        allow_insecure = os.getenv("AUTH_ALLOW_INSECURE_HTTP", "0").strip().lower() in {
            "1",
            "true",
            "yes",
            "on",
        }
        if not secure and not allow_insecure:
            raise RuntimeError("Central auth requires a Secure Lens cookie")
        if len(os.getenv("LENS_SESSION_SECRET", "").encode()) < 32:
            raise RuntimeError("LENS_SESSION_SECRET must contain at least 32 bytes")
        require_auth_signing_public_keys()
        return cls(CentralAuthStore.from_env(), CentralAuthClient.from_env(), cookie_secure=secure)

    def identity(self, request: Request) -> CentralIdentity | None:
        return self.store.get_identity(request.cookies.get(self.cookie_name))

    def unauthenticated_response(self, request: Request) -> RedirectResponse:
        target = request.url.path
        if request.url.query:
            target += f"?{request.url.query}"
        if len(target) > 2048:
            target = "/"
        return RedirectResponse(
            url=f"/auth/login?next={urllib.parse.quote(target, safe='')}", status_code=303
        )

    def begin_login(self, request: Request, next_path: str) -> RedirectResponse:
        state = secrets.token_urlsafe(32)
        nonce = secrets.token_urlsafe(32)
        verifier = secrets.token_urlsafe(64)
        challenge = (
            base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest())
            .rstrip(b"=")
            .decode()
        )
        request.session["central_auth_transaction"] = {
            "state": state,
            "nonce": nonce,
            "verifier": verifier,
            "next": _safe_local_path(next_path),
            "created_at": int(time.time()),
        }
        response = RedirectResponse(
            self.client.authorization_url(state=state, code_challenge=challenge, nonce=nonce),
            status_code=303,
        )
        response.headers["Cache-Control"] = "no-store"
        return response

    def complete_login(self, request: Request, *, code: str, state: str) -> tuple[str, str]:
        transaction = request.session.pop("central_auth_transaction", None)
        if not isinstance(transaction, dict):
            raise AuthTransactionError("Missing login transaction")
        created = transaction.get("created_at")
        expected_state = transaction.get("state")
        nonce = transaction.get("nonce")
        verifier = transaction.get("verifier")
        if (
            isinstance(created, bool)
            or not isinstance(created, int)
            or created + LOGIN_TRANSACTION_SECONDS <= int(time.time())
            or not isinstance(expected_state, str)
            or not hmac.compare_digest(expected_state.encode(), state.encode())
            or not isinstance(nonce, str)
            or not isinstance(verifier, str)
        ):
            raise AuthTransactionError("Invalid login transaction")
        principal = self.client.exchange(code=code, verifier=verifier, expected_nonce=nonce)
        issued = self.store.create_session(principal.subject, principal.email)
        return issued.token, _safe_local_path(str(transaction.get("next") or "/"))

    def set_session_cookie(self, response: RedirectResponse, token: str) -> None:
        response.set_cookie(
            self.cookie_name,
            token,
            secure=self.cookie_secure,
            httponly=True,
            samesite="lax",
            path="/",
            max_age=None,
        )

    def clear_session_cookie(self, response: RedirectResponse) -> None:
        response.delete_cookie(
            self.cookie_name,
            secure=self.cookie_secure,
            httponly=True,
            samesite="lax",
            path="/",
        )

    def csrf_token(self, request: Request) -> str:
        token = request.cookies.get(self.cookie_name, "")
        return csrf_token_for_session(token) if token else ""

    def valid_csrf(self, request: Request, submitted: str) -> bool:
        token = request.cookies.get(self.cookie_name, "")
        if not token or not submitted:
            return False
        return hmac.compare_digest(csrf_token_for_session(token), submitted)


def _safe_local_path(raw: str) -> str:
    # The path rides in the signed login-transaction cookie; an unbounded value
    # would push that cookie past the browser's 4 KB limit and silently break
    # sign-in. Control characters have no place in a redirect target.
    if len(raw) > 2048 or any(ord(char) < 32 or ord(char) == 127 for char in raw):
        return "/"
    parsed = urllib.parse.urlsplit(raw)
    if (
        not raw.startswith("/")
        or raw.startswith("//")
        or parsed.scheme
        or parsed.netloc
        or "\\" in raw
    ):
        return "/"
    return raw


_provider_cache: tuple[tuple[str, ...], AuthProvider] | None = None


def _fingerprint() -> tuple[str, ...]:
    return tuple(
        os.getenv(name, "")
        for name in (
            "LENS_AUTH_MODE",
            "LENS_ACCESS_DB",
            "LENS_AUTH_COOKIE_SECURE",
            "AUTH_ALLOW_INSECURE_HTTP",
            "AUTH_SIGNING_PUBKEY",
            "AUTH_SIGNING_PREVIOUS_PUBKEYS",
            "AUTH_ISSUER_URL",
            "LENS_PUBLIC_URL",
            "AUTH_CLIENT_ID",
            "AUTH_CLIENT_SECRET",
            "LENS_SESSION_SECRET",
            "LENS_SESSION_IDLE_SECONDS",
            "LENS_SESSION_ABSOLUTE_SECONDS",
        )
    )


def clear_provider_cache() -> None:
    global _provider_cache
    _provider_cache = None


def get_auth_provider() -> AuthProvider:
    global _provider_cache
    fingerprint = _fingerprint()
    if _provider_cache is not None and _provider_cache[0] == fingerprint:
        return _provider_cache[1]
    mode = os.getenv("LENS_AUTH_MODE", "elcano").strip().lower()
    if mode == "elcano":
        provider: AuthProvider = ElcanoAuthProvider()
    elif mode == "central":
        provider = CentralAuthProvider.from_env()
    else:
        raise RuntimeError("LENS_AUTH_MODE must be either 'elcano' or 'central'")
    _provider_cache = (fingerprint, provider)
    return provider
