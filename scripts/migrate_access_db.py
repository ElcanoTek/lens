#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""Safely move Lens's persistent access database out of the release tree."""

from __future__ import annotations

import argparse
import os
import pwd
import re
import shlex
import sqlite3
import tempfile
from pathlib import Path
from urllib.parse import quote

_ACCESS_DB_ASSIGNMENT = re.compile(
    r"^(?P<prefix>\s*(?:export\s+)?LENS_ACCESS_DB\s*=\s*)(?P<value>.*?)(?P<newline>\r?\n?)$"
)


def _sync_directory(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _ensure_state_directory(path: Path, owner_uid: int, owner_gid: int) -> None:
    if path.is_symlink():
        raise RuntimeError(f"refusing symlink at access database directory: {path}")
    path.mkdir(parents=True, exist_ok=True)
    if not path.is_dir():
        raise RuntimeError(f"access database parent is not a directory: {path}")
    current = path.stat()
    if (current.st_uid, current.st_gid) != (owner_uid, owner_gid):
        os.chown(path, owner_uid, owner_gid)
    path.chmod(0o750)


def _validate_database(path: Path) -> None:
    uri = f"file:{quote(str(path.absolute()), safe='/')}?mode=ro"
    with sqlite3.connect(uri, uri=True) as connection:
        result = connection.execute("PRAGMA quick_check").fetchone()
    if result != ("ok",):
        raise RuntimeError(f"access database failed quick_check: {result!r}")


def migrate_access_database(
    legacy_path: str | Path,
    target_path: str | Path,
    owner_uid: int,
    owner_gid: int,
) -> bool:
    """Atomically copy a legacy SQLite DB, without replacing an existing target."""
    legacy = Path(legacy_path)
    target = Path(target_path)
    _ensure_state_directory(target.parent, owner_uid, owner_gid)
    if target.is_symlink():
        raise RuntimeError(f"refusing symlink at access database target: {target}")
    if target.exists():
        if not target.is_file():
            raise RuntimeError(f"access database target is not a regular file: {target}")
        _validate_database(target)
        target_stat = target.stat()
        if (target_stat.st_uid, target_stat.st_gid) != (owner_uid, owner_gid):
            os.chown(target, owner_uid, owner_gid)
        target.chmod(0o600)
        return False
    if legacy.is_symlink():
        raise RuntimeError(f"refusing symlink at legacy access database path: {legacy}")
    if not legacy.is_file():
        return False

    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{target.name}.migrating-", dir=target.parent
    )
    os.close(descriptor)
    temporary = Path(temporary_name)
    try:
        source_uri = f"file:{quote(str(legacy.absolute()), safe='/')}?mode=ro"
        with sqlite3.connect(source_uri, uri=True) as source:
            with sqlite3.connect(temporary) as destination:
                source.backup(destination)
                result = destination.execute("PRAGMA quick_check").fetchone()
                if result != ("ok",):
                    raise RuntimeError(f"migrated access database failed quick_check: {result!r}")

        temporary.chmod(0o600)
        temporary_stat = temporary.stat()
        if (temporary_stat.st_uid, temporary_stat.st_gid) != (owner_uid, owner_gid):
            os.chown(temporary, owner_uid, owner_gid)
        with temporary.open("rb") as migrated_file:
            os.fsync(migrated_file.fileno())
        try:
            # A hard link is an atomic create-without-replace operation. Both
            # paths live in the target directory, so it also cannot cross a
            # filesystem boundary.
            os.link(temporary, target)
        except FileExistsError:
            return False
        _sync_directory(target.parent)
        return True
    finally:
        temporary.unlink(missing_ok=True)


def _unquote_shell_value(value: str) -> str | None:
    try:
        parsed = shlex.split(value, comments=True, posix=True)
    except ValueError:
        return None
    return parsed[0] if len(parsed) == 1 else None


def configured_access_path(env_file: str | Path) -> str | None:
    path = Path(env_file)
    if not path.is_file():
        return None
    configured = None
    for line in path.read_text().splitlines(keepends=True):
        match = _ACCESS_DB_ASSIGNMENT.match(line)
        if match:
            configured = _unquote_shell_value(match.group("value"))
            if configured is None:
                raise RuntimeError(f"invalid LENS_ACCESS_DB assignment in {path}")
    return configured


def rewrite_legacy_env_path(
    env_file: str | Path,
    legacy_path: str,
    target_path: str,
) -> bool:
    """Rewrite only Lens's generated legacy default, preserving custom paths."""
    path = Path(env_file)
    if not path.is_file():
        return False

    original = path.read_text()
    rewritten_lines: list[str] = []
    changed = False
    for line in original.splitlines(keepends=True):
        match = _ACCESS_DB_ASSIGNMENT.match(line)
        if match and _unquote_shell_value(match.group("value")) == legacy_path:
            line = f'{match.group("prefix")}"{target_path}"{match.group("newline")}'
            changed = True
        rewritten_lines.append(line)
    if not changed:
        return False

    file_stat = path.stat()
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "w") as output:
            output.write("".join(rewritten_lines))
            output.flush()
            os.fsync(output.fileno())
        temporary.chmod(file_stat.st_mode & 0o7777)
        temporary_stat = temporary.stat()
        if (temporary_stat.st_uid, temporary_stat.st_gid) != (
            file_stat.st_uid,
            file_stat.st_gid,
        ):
            os.chown(temporary, file_stat.st_uid, file_stat.st_gid)
        os.replace(temporary, path)
        _sync_directory(path.parent)
    finally:
        temporary.unlink(missing_ok=True)
    return True


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--legacy", required=True, type=Path)
    parser.add_argument("--target", required=True, type=Path)
    parser.add_argument("--env-file", required=True, type=Path)
    parser.add_argument("--owner", required=True)
    args = parser.parse_args()

    account = pwd.getpwnam(args.owner)
    configured = configured_access_path(args.env_file)
    if configured not in (None, str(args.legacy), str(args.target)):
        print(f"custom LENS_ACCESS_DB preserved: {configured}")
        return 0

    migrated = migrate_access_database(
        args.legacy,
        args.target,
        account.pw_uid,
        account.pw_gid,
    )
    rewritten = rewrite_legacy_env_path(args.env_file, str(args.legacy), str(args.target))
    if migrated:
        print(f"migrated Lens access database to {args.target}")
    elif args.target.exists():
        print(f"Lens access database ready at {args.target}")
    else:
        print(f"Lens access database directory ready at {args.target.parent}")
    if rewritten:
        print(f"updated LENS_ACCESS_DB in {args.env_file}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
