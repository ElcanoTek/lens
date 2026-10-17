# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""Exercise deployment data preservation and diagnostics without host writes."""

import base64
import json
import os
import shlex
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


@pytest.mark.parametrize(
    "noninteractive,key,optional,stdin,success,expected",
    [
        ("1", "", "optional", "", True, ""),
        ("1", "preset-secret", "optional", "", True, "preset-secret"),
        ("0", "", "optional", "\n", True, ""),
        ("0", "", "optional", "typed-secret\n", True, "typed-secret"),
        ("1", "", "", "", False, ""),
        ("0", "", "", "\n", False, ""),
    ],
)
def test_bootstrap_optional_secret_prompt(noninteractive, key, optional, stdin, success, expected):
    source = (ROOT / "scripts/bootstrap.sh").read_text()
    function = source[source.index("prompt_secret() {") : source.index("genbase64()")]
    result = bash(
        'die() { echo "$*" >&2; exit 1; }; ask() { echo "$*" >&2; }; '
        + function
        + '\nNON_INTERACTIVE="$1"; TEST_KEY="$2"; prompt_secret TEST_KEY "Test key" "$3"',
        noninteractive,
        key,
        optional,
        input=stdin,
    )
    assert (result.returncode == 0) is success
    if success:
        assert result.stdout == expected
    assert "preset-secret" not in result.stderr and "typed-secret" not in result.stderr


@pytest.mark.parametrize("existing", [False, True])
def test_bootstrap_typesafe_env_roundtrip_preserves_existing_key(tmp_path, existing):
    from _vendor.dotenv import dotenv_values

    source = (ROOT / "scripts/bootstrap.sh").read_text()
    prompts = source[source.index("prompt() {") : source.index("[[ $EUID")]
    config_block = source[
        source.index('if [[ -f "$ENV_FILE" ]]; then') : source.index("# ── Caddy / TLS intent")
    ]
    writer = source[
        source.index("umask 077") : source.index('chown "$APP_USER:$APP_USER" "$ENV_FILE"')
    ]
    app = tmp_path / "app"
    (app / ".venv/bin").mkdir(parents=True)
    python = app / ".venv/bin/python"
    python.write_text(f'#!/bin/sh\nexec {shlex.quote(sys.executable)} "$@"\n')
    python.chmod(0o755)
    # Bootstrap runs this step after the release tree is synced into APP_DIR.
    (app / "_vendor").symlink_to(ROOT / "_vendor", target_is_directory=True)
    env_file = app / ".env"
    if existing:
        env_file.write_text("TYPESAFE_API_KEY='existing-secret'\nOTHER_SETTING='preserved'\n")
    result = bash(
        'die() { echo "$*" >&2; exit 1; }; info() { :; }; ask() { exit 99; }; '
        + prompts
        + '\nAPP_DIR="$1"; ENV_FILE="$2"; NON_INTERACTIVE=1; '
        "AUTH_SIGNING_PUBKEY=test; OPENROUTER_API_KEY=test; LENS_SESSION_SECRET=test; LENS_AUTH_MODE=elcano; "
        "TYPESAFE_API_KEY=provided-secret\n" + config_block + writer,
        app,
        env_file,
    )
    assert result.returncode == 0, result.stderr
    saved = dotenv_values(env_file)
    assert saved["TYPESAFE_API_KEY"] == ("existing-secret" if existing else "provided-secret")
    if existing:
        assert saved["OTHER_SETTING"] == "preserved"
    assert "provided-secret" not in result.stdout + result.stderr
    assert "existing-secret" not in result.stdout + result.stderr


@pytest.mark.parametrize("script", ["bootstrap.sh", "update.sh"])
def test_service_user_commands_never_run_from_the_callers_cwd(script):
    # runuser keeps the cwd; from root's unreadable home, uv and podman fail.
    source = (ROOT / "scripts" / script).read_text()
    assert 0 <= source.index('cd "$SRC_DIR"') < source.index("runuser -u")


@pytest.mark.skipif(not shutil.which("rsync"), reason="deployment integration needs rsync")
def test_deploy_and_rollback_preserve_instance_data(tmp_path):
    live, stage, backup = [tmp_path / name for name in ("live", "stage", "backup")]
    for tree in (live, stage, backup):
        tree.mkdir()
    state = [
        ".env",
        ".env.shared",
        "config.json",
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


def test_deploy_git_needs_no_home_or_global_trust_config(tmp_path):
    repo = tmp_path / "source"
    subprocess.run(["git", "init", str(repo)], check=True, capture_output=True)
    env = {**os.environ, "GIT_TEST_ASSUME_DIFFERENT_OWNER": "1", "GIT_CONFIG_NOSYSTEM": "1"}
    env.pop("HOME", None)
    env.pop("XDG_CONFIG_HOME", None)
    result = subprocess.run(
        ["git", "-C", str(repo), "status", "--porcelain"], env=env, capture_output=True
    )
    assert result.returncode != 0
    result = bash('source "$1"; lens_git "$2" status --porcelain', LIB, repo, env=env)
    assert result.returncode == 0, result.stderr
    assert not (repo / ".gitconfig").exists()


def test_doctor_podman_probe_uses_service_home_not_root_cwd(tmp_path, monkeypatch):
    calls = []

    def probe(*args, **kwargs):
        if "podman" in args:
            calls.append((args, kwargs))
            return (0, "") if kwargs.get("cwd") == tmp_path else (125, "")
        return 127, ""

    monkeypatch.setattr(doctor, "run", probe)
    monkeypatch.setattr(doctor.shutil, "which", lambda name: name == "podman")
    monkeypatch.setattr(doctor.os, "geteuid", lambda: 0)
    monkeypatch.setattr(doctor.pwd, "getpwnam", lambda _: doctor.pwd.getpwuid(os.getuid()))
    checks = doctor.diagnose(tmp_path, tmp_path, "lens")
    assert next(c for c in checks if c["name"] == "podman")["status"] == "ok"
    assert calls[0][0][:4] == ("runuser", "-u", "lens", "--")


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


class _Account:
    def __init__(self, uid, gid):
        self.pw_uid = uid
        self.pw_gid = gid


class _Stat:
    def __init__(self, mode, uid, gid):
        self.st_mode = mode
        self.st_uid = uid
        self.st_gid = gid


def test_service_user_can_read_rejects_a_root_owned_private_env(tmp_path, monkeypatch):
    env = tmp_path / ".env"
    env.write_text("OPENROUTER_API_KEY=x\n")
    monkeypatch.setattr(doctor.pwd, "getpwnam", lambda name: _Account(987, 987))
    monkeypatch.setattr(doctor.os, "getgrouplist", lambda user, gid: [gid])

    monkeypatch.setattr(Path, "stat", lambda self, *args, **kwargs: _Stat(0o100600, 0, 0))
    assert doctor.service_user_can_read(env, "lens") is False

    monkeypatch.setattr(Path, "stat", lambda self, *args, **kwargs: _Stat(0o100600, 987, 987))
    assert doctor.service_user_can_read(env, "lens") is True

    monkeypatch.setattr(Path, "stat", lambda self, *args, **kwargs: _Stat(0o100640, 0, 987))
    assert doctor.service_user_can_read(env, "lens") is True


def test_doctor_flags_env_the_service_user_cannot_read(tmp_path, monkeypatch):
    env = tmp_path / ".env"
    env.write_text("OPENROUTER_API_KEY=x\n")
    env.chmod(0o600)
    monkeypatch.setattr(doctor, "run", lambda *args, **kwargs: (127, ""))
    monkeypatch.setattr(doctor.shutil, "which", lambda name: None)
    monkeypatch.setattr(doctor, "service_user_can_read", lambda path, user: False)
    checks = doctor.diagnose(tmp_path, tmp_path / "src", "lens")
    readable = next(c for c in checks if c["name"] == "env-readable")
    assert readable["status"] == "fail"
    assert f"chown lens:lens {env}" in readable["detail"]
    assert next(c for c in checks if c["name"] == "env-permissions")["status"] == "ok"


def test_unreadable_dotenv_does_not_abort_startup(monkeypatch):
    import config as config_module

    def boom(*args, **kwargs):
        raise PermissionError(13, "Permission denied", "/opt/lens/.env")

    monkeypatch.setattr(config_module, "load_dotenv", boom)
    config_module._load_dotenv()


def test_lens_env_edit_returns_ownership_to_the_service_user(tmp_path):
    app = tmp_path / "app"
    app.mkdir()
    env = app / ".env"
    env.write_text("OPENROUTER_API_KEY=present\n")
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    log = tmp_path / "sudo.log"
    editor = bin_dir / "ed"
    editor.write_text("#!/bin/sh\nprintf 'OPENROUTER_API_KEY=edited\\n' > \"$1\"\n")
    editor.chmod(0o755)
    sudo = bin_dir / "sudo"
    sudo.write_text(
        "#!/bin/sh\n"
        f"printf '%s\\n' \"$*\" >> {shlex.quote(str(log))}\n"
        "while [ $# -gt 0 ]; do\n"
        '  case "$1" in\n'
        "    --preserve-env=*) shift ;;\n"
        "    *) break ;;\n"
        "  esac\n"
        "done\n"
        'cmd="$1"; shift\n'
        'case "$cmd" in\n'
        "  chown|chmod) exit 0 ;;\n"
        "esac\n"
        'exec "$cmd" "$@"\n'
    )
    sudo.chmod(0o755)
    result = subprocess.run(
        ["bash", str(ROOT / "deploy/lens-cli"), "env", "edit"],
        env={
            **os.environ,
            "PATH": f"{bin_dir}:{os.environ['PATH']}",
            "LENS_APP_DIR": str(app),
            "EDITOR": str(editor),
            "LENS_APP_USER": "lens",
        },
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stderr
    logged = log.read_text()
    assert f"chown lens:lens {env}" in logged
    assert f"chmod 0600 {env}" in logged
    assert env.read_text() == "OPENROUTER_API_KEY=edited\n"


@pytest.mark.parametrize("keep, survivors", [("", 2), ("1", 1), ("3", 3), ("junk", 2), ("0", 2)])
def test_prune_rollbacks_keeps_the_newest(tmp_path, keep, survivors):
    app = tmp_path / "lens"
    app.mkdir()
    snapshots = []
    for n in range(5):
        snap = tmp_path / f"lens.rollback.{n:06d}"
        (snap / ".venv").mkdir(parents=True)
        os.utime(snap, (1_000_000 + n, 1_000_000 + n))
        snapshots.append(snap)
    (tmp_path / "lens-other").mkdir()  # not a rollback snapshot
    script = '. "$1"; lens_prune_rollbacks "$2" "$3"'
    result = bash(script, ROOT / "scripts/lib/deploy.sh", app, keep)
    assert result.returncode == 0, result.stderr
    remaining = sorted(p.name for p in tmp_path.glob("lens.rollback.*"))
    assert remaining == [s.name for s in snapshots[-survivors:]]
    assert app.exists() and (tmp_path / "lens-other").exists()


def test_prune_rollbacks_without_snapshots_is_quiet(tmp_path):
    result = bash(
        '. "$1"; lens_prune_rollbacks "$2" 2', ROOT / "scripts/lib/deploy.sh", tmp_path / "lens"
    )
    assert result.returncode == 0 and result.stdout == ""


VULTR_HOSTS = (
    "127.0.0.1   localhost localhost.localdomain\n"
    "::1         localhost localhost.localdomain\n"
    "127.0.0.1 vultr.guest vultr\n"
)
PRESERVE_CFG = (
    "# The hostname is set by hand; do not let cloud-init reset it from Vultr metadata.\n"
    "preserve_hostname: true\n"
    "manage_etc_hosts: false\n"
)


def set_hostname(box, fqdn, current="vultr.guest", hostnamectl=True, hosts=VULTR_HOSTS, **env):
    """Run lens_set_hostname on a scratch box; return (output, name, hosts, cloud cfg)."""
    bindir = box / "bin"
    bindir.mkdir()
    name = box / "hostname"
    hosts_file = box / "hosts"
    cloud = box / "cloud"
    cloud.mkdir()
    name.write_text(current + "\n")
    hosts_file.write_text(hosts)
    for tool in ("awk", "cat", "mkdir"):
        (bindir / tool).symlink_to(shutil.which(tool))
    if hostnamectl:
        (bindir / "hostnamectl").write_text(
            "#!/bin/sh\n"
            f"case \"$1\" in --static) cat '{name}' ;; "
            f"set-hostname) printf '%s\\n' \"$2\" > '{name}' ;; esac\n"
        )
        (bindir / "hostnamectl").chmod(0o755)
    result = subprocess.run(
        [
            shutil.which("bash"),
            "-c",
            'set -euo pipefail; . "$1"; info() { echo "INFO $*"; }; ok() { echo "OK $*"; }; '
            'warn() { echo "WARN $*"; }; lens_set_hostname "$2"; echo "RC=$?"',
            "test",
            str(ROOT / "scripts/lib/hostname.sh"),
            fqdn,
        ],
        env={
            "PATH": str(bindir),
            "LENS_HOSTS_FILE": str(hosts_file),
            "LENS_CLOUD_DIR": str(cloud),
            **env,
        },
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0 and "RC=0" in result.stdout, result.stdout + result.stderr
    cfg = cloud / "cloud.cfg.d/99-preserve-hostname.cfg"
    return (
        result.stdout,
        name.read_text().strip(),
        hosts_file.read_text(),
        cfg.read_text() if cfg.exists() else "",
    )


def test_set_hostname_renames_a_vultr_default_once(tmp_path):
    out, name, hosts, cfg = set_hostname(tmp_path, "Lens.Example.com")
    assert name == "lens.example.com", out
    assert cfg == PRESERVE_CFG
    assert hosts == (
        "127.0.0.1   localhost localhost.localdomain\n"
        "::1         localhost localhost.localdomain\n"
        "127.0.0.1 lens.example.com lens\n"
    )
    (tmp_path / "again").mkdir()
    out, name, again, _ = set_hostname(
        tmp_path / "again", "lens.example.com", current="lens.example.com", hosts=hosts
    )
    assert again == hosts
    assert "hostname already lens.example.com" in out


def test_set_hostname_never_rewrites_a_localhost_line(tmp_path):
    _, name, hosts, _ = set_hostname(
        tmp_path,
        "lens.example.com",
        current="localhost",
        hosts="127.0.0.1 localhost\n::1 localhost\n",
    )
    assert name == "lens.example.com"
    assert hosts == (
        "127.0.0.1 localhost\n::1 localhost\n"
        "127.0.0.1 lens.example.com lens\n::1 lens.example.com lens\n"
    )


@pytest.mark.parametrize("current", ["yoga", "fedora", "lens.client.example"])
def test_set_hostname_leaves_a_deliberate_name(tmp_path, current):
    out, name, hosts, cfg = set_hostname(tmp_path, "lens.example.com", current=current)
    assert (name, hosts, cfg) == (current, VULTR_HOSTS, "")
    assert "hostnamectl set-hostname lens.example.com" in out


@pytest.mark.parametrize(
    "fqdn,hostnamectl,env",
    [
        ("lens.example.com", True, {"LENS_SET_HOSTNAME": "0"}),
        ("localhost", True, {}),
        ("10.0.0.5", True, {}),
        ("lens.example.com", False, {}),
        ("lens.example.com", True, {"DRY_RUN": "1"}),
    ],
)
def test_set_hostname_skips(tmp_path, fqdn, hostnamectl, env):
    out, name, hosts, cfg = set_hostname(tmp_path, fqdn, hostnamectl=hostnamectl, **env)
    assert (name, hosts, cfg) == ("vultr.guest", VULTR_HOSTS, ""), out


def test_set_hostname_force_renames_a_deliberate_name(tmp_path):
    _, name, _, _ = set_hostname(
        tmp_path, "lens.example.com", current="yoga", LENS_SET_HOSTNAME="1"
    )
    assert name == "lens.example.com"


def test_only_bootstrap_sets_the_hostname():
    assert "lens_set_hostname" in ((ROOT / "scripts") / "bootstrap.sh").read_text()
    assert "set_hostname" not in ((ROOT / "scripts") / "update.sh").read_text()


def cockpit_uninstall(tmp_path, rpm=True, **env):
    """Run lens_cockpit_uninstall with stub rpm/dnf; return (output, calls)."""
    bindir = tmp_path / "bin"
    bindir.mkdir()
    log = tmp_path / "calls.log"
    for name, body in (
        ("rpm", '[ "${STUB_INSTALLED:-0}" = 1 ]'),
        ("dnf", '[ "${STUB_FAIL:-0}" != 1 ]'),
    ):
        if name == "rpm" and not rpm:
            continue
        (bindir / name).write_text(f'#!/bin/sh\necho "{name} $*" >> "$STUB_LOG"\n{body}\n')
        (bindir / name).chmod(0o755)
    result = subprocess.run(
        [
            shutil.which("bash"),
            "-c",
            'set -euo pipefail; . "$1"; info() { echo "INFO $*"; }; ok() { echo "OK $*"; }; '
            'warn() { echo "WARN $*"; }; lens_cockpit_uninstall; echo "RC=$?"',
            "test",
            str(LIB),
        ],
        env={"PATH": str(bindir), "STUB_LOG": str(log), **env},
        text=True,
        capture_output=True,
    )
    assert result.returncode == 0 and "RC=0" in result.stdout, result.stdout + result.stderr
    return result.stdout, log.read_text() if log.exists() else ""


def test_cockpit_uninstall_removes_an_installed_cockpit(tmp_path):
    out, calls = cockpit_uninstall(tmp_path, STUB_INSTALLED="1")
    assert "dnf -y remove cockpit*" in calls
    assert "OK removed Cockpit" in out


@pytest.mark.parametrize(
    "rpm,env",
    [
        (True, {}),
        (False, {"STUB_INSTALLED": "1"}),
        (True, {"STUB_INSTALLED": "1", "LENS_KEEP_COCKPIT": "1"}),
        (True, {"STUB_INSTALLED": "1", "DRY_RUN": "1"}),
    ],
)
def test_cockpit_uninstall_skips(tmp_path, rpm, env):
    out, calls = cockpit_uninstall(tmp_path, rpm=rpm, **env)
    assert "dnf" not in calls, out
    if env.get("DRY_RUN"):
        assert "[dry-run] dnf -y remove 'cockpit*'" in out


def test_cockpit_uninstall_failure_only_warns(tmp_path):
    out, _ = cockpit_uninstall(tmp_path, STUB_INSTALLED="1", STUB_FAIL="1")
    assert "WARN could not remove Cockpit" in out


def test_cockpit_is_removed_by_bootstrap_only():
    assert "\nlens_cockpit_uninstall\n" in (ROOT / "scripts/bootstrap.sh").read_text()
    assert "cockpit" not in (ROOT / "scripts/update.sh").read_text()


KEY_ONLY_CONF = (
    "# Key-only SSH: no passwords, root by key only. Sorts before cloud-init's\n"
    "# 50-cloud-init.conf so it wins (sshd keeps the first value it reads).\n"
    "PasswordAuthentication no\n"
    "KbdInteractiveAuthentication no\n"
    "PermitRootLogin prohibit-password\n"
)


def ssh_key_only(tmp_path, keys=1, **env):
    """Run lens_ssh_key_only against a stub sshd/systemctl under a fake root."""
    root = tmp_path / "root"
    (root / "etc/ssh/sshd_config.d").mkdir(parents=True)
    (root / "home/ops/.ssh").mkdir(parents=True)
    (root / "home/ops/.ssh/authorized_keys").write_text("ssh-ed25519 AAAA ops\n" * keys)
    bindir = tmp_path / "bin"
    bindir.mkdir()
    log = tmp_path / "calls.log"
    for name, body in (
        (
            "sshd",
            'case "$1" in -t) [ "${STUB_T_FAIL:-0}" != 1 ];; '
            '-T) echo "authorizedkeysfile .ssh/authorized_keys"; '
            'echo "passwordauthentication ${STUB_PA:-no}";; esac',
        ),
        ("systemctl", "true"),
    ):
        (bindir / name).write_text(f'#!/bin/sh\necho "{name} $*" >> "$STUB_LOG"\n{body}\n')
        (bindir / name).chmod(0o755)
    result = subprocess.run(
        [
            shutil.which("bash"),
            "-c",
            'set -euo pipefail; . "$1"; info() { echo "INFO $*"; }; ok() { echo "OK $*"; }; '
            'warn() { echo "WARN $*"; }; lens_ssh_key_only; echo "RC=$?"',
            "test",
            str(LIB),
        ],
        env={
            "PATH": f"{bindir}:/usr/bin:/bin",
            "STUB_LOG": str(log),
            "SSH_KEY_ONLY_ROOT": str(root),
            **env,
        },
        text=True,
        capture_output=True,
    )
    assert result.returncode == 0 and "RC=0" in result.stdout, result.stdout + result.stderr
    conf = root / "etc/ssh/sshd_config.d/01-no-password-login.conf"
    return result.stdout, log.read_text() if log.exists() else "", conf


def test_ssh_key_only_writes_validates_and_reloads(tmp_path):
    out, calls, conf = ssh_key_only(tmp_path)
    assert conf.read_text() == KEY_ONLY_CONF
    assert conf.stat().st_mode & 0o777 == 0o600
    assert "sshd -t" in calls and "systemctl reload sshd" in calls
    assert "OK SSH is key-only" in out and "WARN" not in out


def test_ssh_key_only_is_idempotent(tmp_path):
    _, _, conf = ssh_key_only(tmp_path)
    log = tmp_path / "calls.log"
    log.write_text("")
    result = subprocess.run(
        [shutil.which("bash"), "-c", '. "$1"; lens_ssh_key_only; echo "RC=$?"', "t", str(LIB)],
        env={
            "PATH": f"{tmp_path / 'bin'}:/usr/bin:/bin",
            "STUB_LOG": str(log),
            "SSH_KEY_ONLY_ROOT": str(tmp_path / "root"),
        },
        text=True,
        capture_output=True,
    )
    assert result.stdout.strip() == "RC=0", result.stdout + result.stderr
    assert log.read_text() == ""
    assert conf.read_text() == KEY_ONLY_CONF


@pytest.mark.parametrize(
    "keys,env,expect",
    [
        (0, {}, "WARN no SSH authorized keys found"),
        (1, {"LENS_KEEP_SSH_PASSWORDS": "1"}, "INFO LENS_KEEP_SSH_PASSWORDS=1"),
        (1, {"DRY_RUN": "1"}, "INFO [dry-run] would write"),
        (1, {"STUB_T_FAIL": "1"}, "WARN sshd -t rejected"),
    ],
)
def test_ssh_key_only_leaves_sshd_alone(tmp_path, keys, env, expect):
    out, calls, conf = ssh_key_only(tmp_path, keys=keys, **env)
    assert expect in out
    assert not conf.exists()
    assert "reload" not in calls


def test_ssh_key_only_warns_when_an_earlier_directive_wins(tmp_path):
    out, _, conf = ssh_key_only(tmp_path, STUB_PA="yes")
    assert conf.exists()
    assert "WARN sshd still accepts passwords" in out


def test_ssh_key_only_runs_from_bootstrap_only():
    assert "\nlens_ssh_key_only\n" in (ROOT / "scripts/bootstrap.sh").read_text()
    assert "ssh_key_only" not in (ROOT / "scripts/update.sh").read_text()


@pytest.mark.parametrize(
    "effective,status",
    [
        ("passwordauthentication no\npermitrootlogin prohibit-password", "ok"),
        ("passwordauthentication yes\npermitrootlogin prohibit-password", "warn"),
        ("passwordauthentication no\npermitrootlogin yes", "warn"),
    ],
)
def test_doctor_warns_when_sshd_accepts_passwords(tmp_path, monkeypatch, effective, status):
    monkeypatch.setattr(
        doctor, "run", lambda *a, **k: (0, effective) if a[0] == "sshd" else (127, "")
    )
    monkeypatch.setattr(doctor.shutil, "which", lambda name: name == "sshd")
    monkeypatch.setattr(doctor.os, "geteuid", lambda: 0)
    checks = {c["name"]: c for c in doctor.diagnose(tmp_path, tmp_path, "lens")}
    assert checks["sshd-passwords"]["status"] == status
    if status == "warn":
        assert "production-support README, Access: SSH keys" in checks["sshd-passwords"]["detail"]


def test_doctor_skips_sshd_check_when_not_root(tmp_path, monkeypatch):
    monkeypatch.setattr(doctor, "run", lambda *a, **k: (0, "passwordauthentication yes"))
    monkeypatch.setattr(doctor.shutil, "which", lambda name: name == "sshd")
    monkeypatch.setattr(doctor.os, "geteuid", lambda: 1000)
    names = {c["name"] for c in doctor.diagnose(tmp_path, tmp_path, "lens")}
    assert "sshd-passwords" not in names


def test_set_hostname_keeps_other_loopback_aliases(tmp_path):
    _, name, hosts, _ = set_hostname(
        tmp_path,
        "lens.example.com",
        hosts="127.0.0.1 localhost\n127.0.0.1 vultr.guest vultr myalias # note\n",
    )
    assert name == "lens.example.com"
    assert hosts == "127.0.0.1 localhost\n127.0.0.1 lens.example.com lens myalias # note\n"
