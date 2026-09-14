# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

import os
import pwd
import sqlite3
import stat
import subprocess
import sys
from pathlib import Path

import pytest

from central_auth import CentralAuthStore
from scripts.migrate_access_db import (
    configured_access_path,
    migrate_access_database,
    rewrite_legacy_env_path,
)

REPO_ROOT = Path(__file__).resolve().parents[1]


def _create_access_database(path, marker: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with sqlite3.connect(path) as connection:
        connection.execute("CREATE TABLE state (marker TEXT NOT NULL)")
        connection.execute("INSERT INTO state(marker) VALUES (?)", (marker,))


def _marker(path) -> str:
    with sqlite3.connect(path) as connection:
        return connection.execute("SELECT marker FROM state").fetchone()[0]


def test_migration_copies_a_valid_database_without_removing_the_legacy_copy(tmp_path) -> None:
    legacy = tmp_path / "opt" / "lens" / "data" / "access.db"
    target = tmp_path / "var" / "lib" / "lens" / "access.db"
    _create_access_database(legacy, "existing-client-state")

    assert migrate_access_database(legacy, target, os.getuid(), os.getgid())

    assert _marker(target) == "existing-client-state"
    assert _marker(legacy) == "existing-client-state"
    assert stat.S_IMODE(target.stat().st_mode) == 0o600
    assert stat.S_IMODE(target.parent.stat().st_mode) == 0o750


def test_migration_never_overwrites_an_existing_destination(tmp_path) -> None:
    legacy = tmp_path / "opt" / "lens" / "data" / "access.db"
    target = tmp_path / "var" / "lib" / "lens" / "access.db"
    _create_access_database(legacy, "legacy")
    _create_access_database(target, "already-live")
    target.chmod(0o644)

    assert not migrate_access_database(legacy, target, os.getuid(), os.getgid())

    assert _marker(target) == "already-live"
    assert _marker(legacy) == "legacy"
    assert stat.S_IMODE(target.stat().st_mode) == 0o600


def test_invalid_existing_destination_stops_the_deployment(tmp_path) -> None:
    legacy = tmp_path / "opt" / "lens" / "data" / "access.db"
    target = tmp_path / "var" / "lib" / "lens" / "access.db"
    _create_access_database(legacy, "legacy")
    target.parent.mkdir(parents=True)
    target.write_text("corrupted")

    with pytest.raises(sqlite3.DatabaseError):
        migrate_access_database(legacy, target, os.getuid(), os.getgid())

    assert target.read_text() == "corrupted"


def test_symlink_destination_is_rejected(tmp_path) -> None:
    legacy = tmp_path / "opt" / "lens" / "data" / "access.db"
    target = tmp_path / "var" / "lib" / "lens" / "access.db"
    elsewhere = tmp_path / "elsewhere.db"
    _create_access_database(legacy, "legacy")
    _create_access_database(elsewhere, "must-not-be-used")
    target.parent.mkdir(parents=True)
    target.symlink_to(elsewhere)

    with pytest.raises(RuntimeError, match="refusing symlink"):
        migrate_access_database(legacy, target, os.getuid(), os.getgid())

    assert _marker(elsewhere) == "must-not-be-used"


def test_invalid_legacy_database_cannot_publish_a_partial_destination(tmp_path) -> None:
    legacy = tmp_path / "opt" / "lens" / "data" / "access.db"
    target = tmp_path / "var" / "lib" / "lens" / "access.db"
    legacy.parent.mkdir(parents=True)
    legacy.write_text("not a sqlite database")

    with pytest.raises(sqlite3.DatabaseError):
        migrate_access_database(legacy, target, os.getuid(), os.getgid())

    assert not target.exists()


def test_generated_legacy_env_path_is_rewritten_atomically(tmp_path) -> None:
    env_file = tmp_path / ".env"
    env_file.write_text(
        'OPENROUTER_API_KEY="secret"\n'
        'LENS_ACCESS_DB="/opt/lens/data/access.db"\n'
        'AUTH_CLIENT_ID="lens"\n'
    )
    env_file.chmod(0o600)

    assert rewrite_legacy_env_path(
        env_file,
        "/opt/lens/data/access.db",
        "/var/lib/lens/access.db",
    )

    assert 'LENS_ACCESS_DB="/var/lib/lens/access.db"' in env_file.read_text()
    assert 'OPENROUTER_API_KEY="secret"' in env_file.read_text()
    assert stat.S_IMODE(env_file.stat().st_mode) == 0o600


def test_custom_database_path_is_left_unchanged(tmp_path) -> None:
    env_file = tmp_path / ".env"
    env_file.write_text('LENS_ACCESS_DB="/srv/lens-private/access.db"\n')

    assert not rewrite_legacy_env_path(
        env_file,
        "/opt/lens/data/access.db",
        "/var/lib/lens/access.db",
    )

    assert env_file.read_text() == 'LENS_ACCESS_DB="/srv/lens-private/access.db"\n'


def test_malformed_database_path_stops_migration_instead_of_guessing(tmp_path) -> None:
    env_file = tmp_path / ".env"
    env_file.write_text('LENS_ACCESS_DB="unterminated\n')

    with pytest.raises(RuntimeError, match="invalid LENS_ACCESS_DB"):
        configured_access_path(env_file)


def test_migration_command_preserves_state_and_repoints_the_environment(tmp_path) -> None:
    legacy = tmp_path / "opt" / "lens" / "data" / "access.db"
    target = tmp_path / "var" / "lib" / "lens" / "access.db"
    env_file = tmp_path / "opt" / "lens" / ".env"
    _create_access_database(legacy, "command-migrated")
    env_file.write_text(f'LENS_ACCESS_DB="{legacy}"\n')

    completed = subprocess.run(
        [
            sys.executable,
            str(REPO_ROOT / "scripts" / "migrate_access_db.py"),
            "--legacy",
            str(legacy),
            "--target",
            str(target),
            "--env-file",
            str(env_file),
            "--owner",
            pwd.getpwuid(os.getuid()).pw_name,
        ],
        check=True,
        capture_output=True,
        text=True,
    )

    assert "migrated Lens access database" in completed.stdout
    assert _marker(target) == "command-migrated"
    assert _marker(legacy) == "command-migrated"
    assert env_file.read_text() == f'LENS_ACCESS_DB="{target}"\n'


@pytest.mark.parametrize("script_name", ["bootstrap.sh", "update.sh"])
def test_deployment_protects_legacy_state_and_runs_the_migration(script_name) -> None:
    script = (REPO_ROOT / "scripts" / script_name).read_text()

    assert "migrate_access_db.py" in script
    assert "--exclude='/data'" in script


def test_update_stops_writes_before_migrating_and_syncing() -> None:
    script = (REPO_ROOT / "scripts" / "update.sh").read_text()

    assert script.index('systemctl stop "$SERVICE"') < script.index("migrate_access_db.py")
    assert script.index("migrate_access_db.py") < script.index("rsync -a --delete")


def test_runtime_defaults_to_the_external_state_directory(monkeypatch) -> None:
    monkeypatch.delenv("LENS_ACCESS_DB", raising=False)
    monkeypatch.setattr(CentralAuthStore, "_initialize", lambda _self: None)

    assert CentralAuthStore.from_env().path == Path("/var/lib/lens/access.db")


def test_bootstrap_and_service_prepare_the_external_state_directory() -> None:
    bootstrap = (REPO_ROOT / "scripts" / "bootstrap.sh").read_text()
    service = (REPO_ROOT / "deploy" / "lens.service").read_text()

    assert 'LENS_ACCESS_DB="${LENS_ACCESS_DB:-/var/lib/lens/access.db}"' in bootstrap
    assert "StateDirectory=lens" in service
    assert "StateDirectoryMode=0750" in service
    assert "ReadWritePaths=/opt/lens /var/lib/lens" in service


def test_bootstrap_fails_when_lens_never_becomes_ready() -> None:
    bootstrap = (REPO_ROOT / "scripts" / "bootstrap.sh").read_text()

    assert 'die "lens did not become ready within 15s' in bootstrap
