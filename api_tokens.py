# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""Scoped bearer tokens for programmatic Lens callers (Fleet, scripts).

Browser sign-in (legacy cookie or central Auth) is for people. A machine
caller gets one of these instead: an operator mints it on the host with
``lens token create``, it is shown once, and only its SHA-256 is stored.
Tokens are 256-bit random strings, so an unsalted hash is enough to make a
stolen database useless; there is nothing to brute-force.

Scopes are deliberately narrow and additive:

``jobs:read``
    List jobs and input files, read status and progress, page through
    results, and mint short-lived download links.
``jobs:run``
    Queue new jobs (upload an input or name a library file) and cancel
    queued or running ones. It cannot delete jobs or files, and nothing
    here can manage users, tokens or settings.

No token is an admin key. Deleting runs and files stays a dashboard action.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import hmac
import json
import os
import re
import secrets
import sqlite3
import sys
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Iterable, List, Optional

SCOPE_READ = "jobs:read"
SCOPE_RUN = "jobs:run"
SCOPES = (SCOPE_READ, SCOPE_RUN)
TOKEN_PREFIX = "lens_"
# Shown in listings and logs so an operator can tell tokens apart without
# ever seeing a secret again.
DISPLAY_PREFIX_CHARS = 12
DEFAULT_DB_PATH = "/var/lib/lens/api_tokens.db"
# Download links are bearer-free so a host-side fetcher (Fleet's
# download_url) can pull them, which makes them secrets of their own. They
# are scoped to one file of one job, expire quickly, and die with the token
# that minted them.
DOWNLOAD_LINK_SECONDS = 15 * 60
# last_used_at is a convenience for operators, not an audit log; writing it
# on every request would turn reads into writes.
LAST_USED_RESOLUTION_SECONDS = 60
LABEL_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9 ._:@-]{0,79}")


class TokenError(Exception):
    """Raised for an unusable token; the message is safe to show a caller."""


@dataclass(frozen=True)
class ApiToken:
    id: int
    label: str
    prefix: str
    scopes: tuple
    created_at: str
    last_used_at: Optional[str]
    revoked_at: Optional[str]

    @property
    def actor(self) -> str:
        return f"token:{self.label}"

    def allows(self, scope: str) -> bool:
        return scope in self.scopes


def db_path() -> str:
    # Read per call, never cached in a module global: tests point it at a
    # tmp directory and the deployed default lives outside the checkout.
    return os.getenv("LENS_API_TOKEN_DB", DEFAULT_DB_PATH)


def _utc_now() -> str:
    return datetime.now(tz=timezone.utc).isoformat(timespec="seconds")


def _hash(raw: str) -> str:
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()


def _connect() -> sqlite3.Connection:
    path = db_path()
    directory = os.path.dirname(path)
    if directory:
        os.makedirs(directory, mode=0o700, exist_ok=True)
    connection = sqlite3.connect(path, timeout=5)
    connection.row_factory = sqlite3.Row
    connection.execute(
        """
        CREATE TABLE IF NOT EXISTS api_tokens (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            label TEXT NOT NULL,
            prefix TEXT NOT NULL,
            token_hash TEXT NOT NULL UNIQUE,
            scopes TEXT NOT NULL,
            created_at TEXT NOT NULL,
            last_used_at TEXT,
            revoked_at TEXT
        )
        """
    )
    connection.execute(
        "CREATE TABLE IF NOT EXISTS api_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)"
    )
    try:
        os.chmod(path, 0o600)
    except OSError:
        pass
    return connection


def _row_to_token(row: sqlite3.Row) -> ApiToken:
    return ApiToken(
        id=int(row["id"]),
        label=row["label"],
        prefix=row["prefix"],
        scopes=tuple(s for s in row["scopes"].split() if s in SCOPES),
        created_at=row["created_at"],
        last_used_at=row["last_used_at"],
        revoked_at=row["revoked_at"],
    )


def normalize_scopes(scopes: Iterable[str]) -> tuple:
    wanted = []
    for scope in scopes:
        scope = str(scope).strip()
        if scope not in SCOPES:
            raise ValueError(f"unknown scope {scope!r}; choose from {', '.join(SCOPES)}")
        if scope not in wanted:
            wanted.append(scope)
    if not wanted:
        raise ValueError("a token needs at least one scope")
    return tuple(s for s in SCOPES if s in wanted)


def create_token(label: str, scopes: Iterable[str]) -> tuple:
    """Mint a token. Returns ``(ApiToken, raw_secret)``; the secret is never stored."""
    label = " ".join(str(label or "").split())
    if not LABEL_RE.fullmatch(label):
        raise ValueError("label must be 1-80 characters: letters, digits, space . _ : @ -")
    normalized = normalize_scopes(scopes)
    raw = TOKEN_PREFIX + secrets.token_urlsafe(32)
    with _connect() as connection:
        cursor = connection.execute(
            "INSERT INTO api_tokens (label, prefix, token_hash, scopes, created_at)"
            " VALUES (?, ?, ?, ?, ?)",
            (label, raw[:DISPLAY_PREFIX_CHARS], _hash(raw), " ".join(normalized), _utc_now()),
        )
        row = connection.execute(
            "SELECT * FROM api_tokens WHERE id = ?", (cursor.lastrowid,)
        ).fetchone()
    return _row_to_token(row), raw


def list_tokens() -> List[ApiToken]:
    with _connect() as connection:
        rows = connection.execute("SELECT * FROM api_tokens ORDER BY id").fetchall()
    return [_row_to_token(row) for row in rows]


def revoke_token(token_id: int) -> bool:
    with _connect() as connection:
        cursor = connection.execute(
            "UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL",
            (_utc_now(), int(token_id)),
        )
    return cursor.rowcount == 1


def _active_token_by_id(token_id: int) -> Optional[ApiToken]:
    with _connect() as connection:
        row = connection.execute(
            "SELECT * FROM api_tokens WHERE id = ? AND revoked_at IS NULL", (int(token_id),)
        ).fetchone()
    return _row_to_token(row) if row else None


def verify_token(raw: str) -> ApiToken:
    """Resolve a presented bearer secret to its active token or raise TokenError."""
    raw = (raw or "").strip()
    if not raw.startswith(TOKEN_PREFIX) or len(raw) > 200:
        raise TokenError("Invalid API token")
    if not os.path.exists(db_path()):
        # No token was ever minted on this host; do not create the database
        # as a side effect of an unauthenticated request.
        raise TokenError("Invalid API token")
    with _connect() as connection:
        row = connection.execute(
            "SELECT * FROM api_tokens WHERE token_hash = ?", (_hash(raw),)
        ).fetchone()
        if row is None:
            raise TokenError("Invalid API token")
        if row["revoked_at"]:
            raise TokenError("This API token has been revoked")
        token = _row_to_token(row)
        if not token.scopes:
            raise TokenError("This API token carries no usable scope")
        last = row["last_used_at"]
        stale = True
        if last:
            try:
                age = time.time() - datetime.fromisoformat(last).timestamp()
                stale = age >= LAST_USED_RESOLUTION_SECONDS
            except ValueError:
                pass
        if stale:
            connection.execute(
                "UPDATE api_tokens SET last_used_at = ? WHERE id = ?", (_utc_now(), token.id)
            )
    return token


# --- Signed download links -------------------------------------------------


def _signing_key() -> bytes:
    """Per-host key for download links, created on first use and kept with the tokens."""
    with _connect() as connection:
        row = connection.execute(
            "SELECT value FROM api_meta WHERE key = 'download_signing_key'"
        ).fetchone()
        if row:
            return base64.urlsafe_b64decode(row["value"])
        key = secrets.token_bytes(32)
        connection.execute(
            "INSERT OR IGNORE INTO api_meta (key, value) VALUES ('download_signing_key', ?)",
            (base64.urlsafe_b64encode(key).decode(),),
        )
        row = connection.execute(
            "SELECT value FROM api_meta WHERE key = 'download_signing_key'"
        ).fetchone()
    return base64.urlsafe_b64decode(row["value"])


def _b64(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def _unb64(text: str) -> bytes:
    return base64.urlsafe_b64decode(text + "=" * (-len(text) % 4))


def sign_download(token: ApiToken, job_id: str, kind: str, ttl: int = DOWNLOAD_LINK_SECONDS):
    """Return ``(ticket, expires_at_epoch)`` for one file of one job."""
    expires = int(time.time()) + ttl
    payload = json.dumps(
        {"t": token.id, "j": job_id, "k": kind, "e": expires}, separators=(",", ":")
    ).encode()
    signature = hmac.new(_signing_key(), payload, hashlib.sha256).digest()
    return f"{_b64(payload)}.{_b64(signature)}", expires


def verify_download(ticket: str) -> tuple:
    """Return ``(token, job_id, kind)`` for a valid, unexpired ticket or raise TokenError."""
    try:
        payload_text, signature_text = ticket.split(".", 1)
        payload = _unb64(payload_text)
        signature = _unb64(signature_text)
    except (ValueError, TypeError) as exc:
        raise TokenError("Invalid download link") from exc
    if not os.path.exists(db_path()):
        raise TokenError("Invalid download link")
    expected = hmac.new(_signing_key(), payload, hashlib.sha256).digest()
    if not hmac.compare_digest(expected, signature):
        raise TokenError("Invalid download link")
    try:
        claims = json.loads(payload)
        token_id, job_id, kind, expires = claims["t"], claims["j"], claims["k"], claims["e"]
    except (ValueError, KeyError, TypeError) as exc:
        raise TokenError("Invalid download link") from exc
    if not isinstance(expires, int) or expires < time.time():
        raise TokenError("This download link has expired; ask for a new one")
    token = _active_token_by_id(token_id)
    if token is None or not token.allows(SCOPE_READ):
        raise TokenError("The token behind this download link is no longer valid")
    return token, str(job_id), str(kind)


# --- Operator CLI (`lens token ...`) ----------------------------------------


def _print_table(tokens: List[ApiToken]) -> None:
    if not tokens:
        print("No API tokens. Create one with: lens token create --label NAME")
        return
    print(f"{'ID':>4}  {'STATE':8}  {'PREFIX':14}  {'SCOPES':20}  {'LAST USED':25}  LABEL")
    for token in tokens:
        state = "revoked" if token.revoked_at else "active"
        print(
            f"{token.id:>4}  {state:8}  {token.prefix + '…':14}  {' '.join(token.scopes):20}  "
            f"{token.last_used_at or 'never':25}  {token.label}"
        )


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        prog="lens token", description="Manage scoped API tokens for Fleet and other callers."
    )
    sub = parser.add_subparsers(dest="command", required=True)
    create = sub.add_parser("create", help="mint a token (shown once)")
    create.add_argument("--label", required=True, help="who holds it, e.g. fleet-elcano")
    create.add_argument(
        "--scope",
        action="append",
        choices=SCOPES,
        help="repeatable; default grants both jobs:read and jobs:run",
    )
    create.add_argument("--json", action="store_true", help="print machine-readable output")
    sub.add_parser("list", help="show tokens (never their secrets)")
    revoke = sub.add_parser("revoke", help="revoke a token by id")
    revoke.add_argument("id", type=int)
    args = parser.parse_args(argv)

    try:
        if args.command == "create":
            token, raw = create_token(args.label, args.scope or SCOPES)
            if args.json:
                print(
                    json.dumps(
                        {
                            "id": token.id,
                            "label": token.label,
                            "scopes": list(token.scopes),
                            "token": raw,
                        }
                    )
                )
            else:
                print(
                    f"Created token {token.id} ({token.label}) with scopes: "
                    f"{' '.join(token.scopes)}"
                )
                print("Copy it now; Lens stores only its hash and cannot show it again:\n")
                print(f"  {raw}\n")
                base = os.getenv("LENS_PUBLIC_URL", "").strip().rstrip("/") or "https://<this host>"
                print("For Fleet, set in its .env:")
                print(f"  LENS_MCP_URL={base}/mcp")
                print("  LENS_API_TOKEN=<the token above>")
        elif args.command == "list":
            _print_table(list_tokens())
        elif args.command == "revoke":
            if revoke_token(args.id):
                print(f"Revoked token {args.id}. Its download links stopped working too.")
            else:
                print(f"No active token with id {args.id}", file=sys.stderr)
                return 1
    except (ValueError, sqlite3.Error, OSError) as exc:
        print(f"lens token: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
