# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""Exercise deployment data preservation and diagnostics without host writes."""

import base64
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

from scripts import doctor

ROOT = Path(__file__).resolve().parents[1]
LIB = ROOT / "scripts/lib/deploy.sh"


def bash(script, *args, **kwargs):
    return subprocess.run(
        ["bash", "-euc", script, "test", *map(str, args)],
        text=True,
        capture_output=True,
        **kwargs,
    )


@pytest.mark.skipif(not shutil.which("rsync"), reason="deployment integration needs rsync")
def test_deploy_and_rollback_preserve_instance_data(tmp_path):
    live, stage, backup = [tmp_path / name for name in ("live", "stage", "backup")]
    for tree in (live, stage, backup):
        tree.mkdir()
    state = [
        ".env",
        ".env.shared",
        "config.json",
        "firecrawl/.env",
        "managed-files/results.csv",
        "data/access.db",
        ".local/podman",
        ".config/containers",
        "progress.json",
    ]
    for name in state:
        path = live / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("operator-owned")
        path = stage / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("must not overwrite")
    for tree, version in ((live, "old"), (stage, "new")):
        (tree / "app.py").write_text(version)
        (tree / ".venv").mkdir()
        (tree / ".venv/python").write_text(version)
        (tree / "deploy").mkdir()
        (tree / "deploy/lens-cli").write_text(version)
    (stage / "new-only.py").write_text("new")
    unit, cli = tmp_path / "unit", tmp_path / "cli"
    unit.write_text("old unit")
    result = bash(
        """
source "$1"
rsync -a "${LENS_STATE_EXCLUDES[@]}" "$2/" "$4/"
cp -a "$2/.venv" "$4/.venv"
cp "$5" "$4/lens.service.previous"
rsync -a --delete "${LENS_STATE_EXCLUDES[@]}" "$3/" "$2/"
cp "$3/.venv/python" "$2/.venv/python"
echo 'new unit' > "$5"
lens_restore_release "$2" "$4" "$5" "$6"
""",
        LIB,
        live,
        stage,
        backup,
        unit,
        cli,
    )
    assert result.returncode == 0, result.stderr
    assert (live / "app.py").read_text() == "old"
    assert (live / ".venv/python").read_text() == "old"
    assert unit.read_text() == "old unit"
    assert cli.read_text() == "old"
    assert not (live / "new-only.py").exists()
    for name in state:
        assert (live / name).read_text() == "operator-owned"
        assert not (backup / name).exists()


def test_fedora_latest_excludes_beta_rawhide_and_fails_closed():
    result = bash(
        'source "$1"; lens_latest_fedora',
        LIB,
        input=json.dumps([{"version": v} for v in ("43", "44", "45 Beta", "rawhide")]),
    )
    assert result.returncode == 0
    assert result.stdout.strip() == "44"
    result = bash('source "$1"; lens_latest_fedora', LIB, input='[{"version":"45 Beta"}]')
    assert result.returncode != 0


def test_host_dry_run_never_invokes_package_manager(tmp_path):
    dnf = tmp_path / "dnf"
    dnf.write_text("#!/bin/sh\necho MUTATED >&2\nexit 99\n")
    dnf.chmod(0o755)
    env = {**os.environ, "PATH": f"{tmp_path}:{os.environ['PATH']}"}
    for command in ("update", "tools"):
        result = subprocess.run(
            ["bash", str(ROOT / "scripts/host.sh"), command, "--dry-run", "--yes"],
            env=env,
            text=True,
            capture_output=True,
        )
        assert result.returncode == 0, result.stderr
        assert "MUTATED" not in result.stderr
        assert "dnf --refresh" in result.stdout


@pytest.mark.parametrize("code,expected", [(0, 0), (100, 0), (1, 1)])
def test_host_check_distinguishes_updates_from_dnf_failure(tmp_path, code, expected):
    dnf = tmp_path / "dnf"
    dnf.write_text(f"#!/bin/sh\nexit {code}\n")
    dnf.chmod(0o755)
    result = subprocess.run(
        ["bash", str(ROOT / "scripts/host.sh"), "check"],
        env={**os.environ, "PATH": f"{tmp_path}:{os.environ['PATH']}"},
        capture_output=True,
    )
    assert result.returncode == expected


def test_doctor_env_never_executes_or_discloses_values(tmp_path):
    (tmp_path / ".venv").symlink_to(sys.prefix, target_is_directory=True)
    marker = tmp_path / "executed"
    key = base64.b64encode(b"x" * 32).decode()
    (tmp_path / ".env").write_text(
        f"OPENROUTER_API_KEY='$(touch {marker})'\nAUTH_SIGNING_PUBKEY={key}\nLENS_AUTH_MODE=central\n"
    )
    code, output = doctor.inspect_env(tmp_path)
    assert code == 0
    data = json.loads(output)
    assert data["key_ok"]
    assert "AUTH_CLIENT_SECRET" in data["missing"]
    assert not data["session_ok"]
    assert not marker.exists()
    assert key not in output
    assert "touch" not in output


def test_doctor_reports_broken_install_without_crashing_or_repairs(tmp_path, monkeypatch):
    commands = []

    def missing(*args, **kwargs):
        commands.append(args)
        return 127, ""

    monkeypatch.setattr(doctor, "run", missing)
    monkeypatch.setattr(doctor.shutil, "which", lambda name: None)
    checks = doctor.diagnose(tmp_path / "missing", tmp_path / "src", "lens")
    assert any(c["name"] == "readiness" and c["status"] == "fail" for c in checks)
    assert any(c["name"] == "configuration" and c["status"] == "fail" for c in checks)
    assert not any(
        token in ("install", "upgrade", "restart", "start", "pull", "fetch")
        for cmd in commands
        for token in cmd
    )


def test_doctor_timeout_is_a_failure():
    assert doctor.run(sys.executable, "-c", "import time; time.sleep(5)", timeout=0.01) == (127, "")


@pytest.mark.parametrize("strict,expected", [(False, 0), (True, 1)])
def test_doctor_json_exit_contract(monkeypatch, capsys, strict, expected):
    monkeypatch.setattr(
        doctor,
        "diagnose",
        lambda *args: [{"name": "podman", "status": "warn", "detail": "optional runtime missing"}],
    )
    monkeypatch.setattr(sys, "argv", ["doctor", "--json"] + (["--strict"] if strict else []))
    assert doctor.main() == expected
    report = json.loads(capsys.readouterr().out)
    assert report["ok"] == (not strict)
    assert report["checks"][0]["status"] == "warn"


def test_doctor_does_not_accept_login_redirect_as_readiness(tmp_path, monkeypatch):
    def redirected(*args, **kwargs):
        return (0, "302") if args[0] == "curl" else (127, "")

    monkeypatch.setattr(doctor, "run", redirected)
    monkeypatch.setattr(doctor.shutil, "which", lambda name: None)
    checks = doctor.diagnose(tmp_path, tmp_path, "lens", "https://lens.example.com")
    for name in ("readiness", "public-tls"):
        assert next(c for c in checks if c["name"] == name)["status"] == "fail"
