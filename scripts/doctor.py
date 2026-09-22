#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""Read-only, bounded deployment diagnostics; never import the Lens pipeline."""

import argparse
import datetime
import json
import os
import platform
import pwd
import shutil
import subprocess
import sys
from pathlib import Path


def run(*args, timeout=15, cwd=None):
    try:
        result = subprocess.run(args, capture_output=True, text=True, timeout=timeout, cwd=cwd)
        return result.returncode, result.stdout.strip()
    except (OSError, subprocess.TimeoutExpired):
        return 127, ""


def service_user_can_read(path: Path, user: str) -> bool:
    """Whether `user` can open `path`.

    Doctor runs as root, so a mode of 0600 is not enough. systemd reads
    EnvironmentFile as root and the dashboard stays up, while each job
    subprocess runs as the service user and opens `.env` itself.
    """
    try:
        stat = path.stat()
        account = pwd.getpwnam(user)
    except (OSError, KeyError):
        return False
    mode = stat.st_mode
    if stat.st_uid == account.pw_uid:
        return bool(mode & 0o400)
    groups = {account.pw_gid}
    try:
        groups.update(os.getgrouplist(user, account.pw_gid))
    except OSError:
        pass
    if stat.st_gid in groups:
        return bool(mode & 0o040)
    return bool(mode & 0o004)


def inspect_env(app):
    # Parse with the application's dotenv implementation, never source secrets
    # as shell commands. Return key names and booleans, never secret values.
    return run(
        str(app / ".venv/bin/python"),
        "-c",
        """
import base64, json, sys
from dotenv import dotenv_values
v = dotenv_values(sys.argv[1], interpolate=False)
required = ['OPENROUTER_API_KEY', 'AUTH_SIGNING_PUBKEY']
mode = v.get('LENS_AUTH_MODE', 'elcano')
if mode == 'central':
    required += ['AUTH_ISSUER_URL', 'LENS_PUBLIC_URL', 'AUTH_CLIENT_ID',
                 'AUTH_CLIENT_SECRET', 'LENS_SESSION_SECRET']
missing = [k for k in required if not v.get(k)]
try:
    key_ok = len(base64.b64decode(v.get('AUTH_SIGNING_PUBKEY', ''), validate=True)) == 32
except (ValueError, TypeError):
    key_ok = False
print(json.dumps({'missing': missing, 'key_ok': key_ok,
                  'mode_ok': mode in ('elcano', 'central'),
                  'session_ok': mode != 'central' or len(v.get('LENS_SESSION_SECRET') or '') >= 32}))
""",
        str(app / ".env"),
    )


def diagnose(app, src, user, public_url=None):
    checks = []

    def add(name, good, detail, remedy, warning=False):
        checks.append(
            {
                "name": name,
                "status": "ok" if good else "warn" if warning else "fail",
                "detail": detail if good else remedy,
            }
        )

    for name in ("git", "curl", "rsync", "uv", "dnf", "systemctl"):
        add(name, shutil.which(name), "installed", "Run bootstrap to install " + name)
    code, version = run(str(app / ".venv/bin/python"), "--version")
    try:
        wanted = (src / ".python-version").read_text().strip()
    except OSError:
        wanted = "3.12"
    add(
        "python",
        code == 0 and version.startswith(f"Python {wanted}."),
        version,
        "Run lens rebuild to install the supported Python/venv",
    )
    code, _ = run("uv", "pip", "check", "--python", str(app / ".venv/bin/python"))
    add(
        "dependencies",
        code == 0,
        "locked dependencies consistent",
        "Run lens rebuild; Python dependencies are broken",
    )
    env_file = app / ".env"
    try:
        mode = env_file.stat().st_mode & 0o777
        add("env-permissions", mode in (0o600, 0o640), "private", "chmod 600 /opt/lens/.env")
        # Mode 0600 owned by root still looks "private" and still crashes every
        # job: the web process gets the variables from EnvironmentFile, and the
        # analyzer opens the file as the service user.
        add(
            "env-readable",
            service_user_can_read(env_file, user),
            f"{user} can read .env",
            f"chown {user}:{user} {env_file} && chmod 600 {env_file}. "
            "Jobs run as the service user and load .env themselves. "
            "A root-owned 0600 file passes the mode check and crashes every run.",
        )
        code, data = inspect_env(app)
        env = json.loads(data) if code == 0 else {}
        good = env and not env["missing"] and env["key_ok"] and env["mode_ok"] and env["session_ok"]
        missing = ", ".join(env.get("missing", []))
        add(
            "configuration",
            good,
            "auth and API credentials configured",
            "lens env edit: check auth mode, signing key, session secret and required credentials"
            + (f"; missing: {missing}" if missing else ""),
        )
    except (OSError, ValueError):
        add(
            "configuration",
            False,
            "",
            "Cannot read configuration; run doctor with sudo or restore .env",
        )
    code, _ = run("systemctl", "is-active", "--quiet", "lens.service")
    add("service", code == 0, "lens.service active", "Inspect lens logs; then lens restart")
    code, status = run(
        "curl",
        "-sS",
        "--connect-timeout",
        "2",
        "--max-time",
        "5",
        "-o",
        "/dev/null",
        "-w",
        "%{http_code}",
        "http://127.0.0.1:8808/health",
    )
    add(
        "readiness",
        code == 0 and status == "200",
        "/health returns 200",
        "Readiness failed; inspect lens logs",
    )
    code, pid = run("systemctl", "show", "--property=MainPID", "--value", "lens.service")
    if code == 0 and pid.isdigit() and int(pid):
        try:
            same = os.path.samefile(f"/proc/{pid}/exe", app / ".venv/bin/python")
        except OSError:
            same = False
        add(
            "running-python",
            same,
            "running interpreter matches installed interpreter",
            "Run lens restart; interpreter is stale or inaccessible",
        )
    git = ("git", "-c", f"safe.directory={src}", "-C", str(src))
    code, head = run(*git, "rev-parse", "HEAD")
    try:
        deployed = (app / ".deployed-revision").read_text().strip()
    except OSError:
        deployed = ""
    add(
        "release",
        code == 0 and head == deployed,
        f"deployed {head[:12]}",
        "Run lens rebuild; source and deployed revision differ or stamp is missing",
    )
    code, dirty = run(*git, "status", "--porcelain")
    add(
        "checkout",
        code == 0 and not dirty,
        "clean",
        "Resolve local source changes before lens update",
    )
    try:
        free = shutil.disk_usage(app).free
        add(
            "disk",
            free >= 2 * 1024**3,
            f"{free / 1024**3:.1f} GiB available",
            "Less than 2 GiB free; inspect run outputs and Podman storage",
        )
    except OSError:
        add("disk", False, "", "Application directory is missing or inaccessible")
    try:
        release = platform.freedesktop_os_release()
        end = release.get("SUPPORT_END")
        if end:
            days = (datetime.date.fromisoformat(end) - datetime.date.today()).days
            add(
                "os-support",
                days >= 30,
                f"support ends {end}",
                f"OS support ends {end}; plan lens host upgrade",
                warning=True,
            )
        else:
            add(
                "os-support",
                False,
                "",
                "OS does not publish SUPPORT_END; verify vendor lifecycle",
                warning=True,
            )
    except (OSError, ValueError, AttributeError):
        add("os-support", False, "", "Cannot read OS lifecycle metadata", warning=True)
    if shutil.which("podman"):
        try:
            uid = pwd.getpwnam(user).pw_uid
            prefix = ["runuser", "-u", user, "--"] if os.geteuid() == 0 else []
            code, _ = run(
                *prefix,
                "env",
                f"HOME={app}",
                f"XDG_RUNTIME_DIR=/run/user/{uid}",
                "podman",
                "info",
                timeout=20,
                cwd=app,
            )
            add(
                "podman",
                code == 0,
                "service-user rootless Podman works",
                "Check subuid/subgid, lingering and lens user's Podman; rerun bootstrap",
                warning=True,
            )
        except KeyError:
            add("podman", False, "", "Lens service account missing; rerun bootstrap")
    else:
        add(
            "podman",
            False,
            "",
            "Optional container scraping unavailable; install Podman with bootstrap",
            warning=True,
        )
    code, load = run("systemctl", "show", "--property=LoadState", "--value", "firecrawl.service")
    if code == 0 and load == "loaded":
        code, _ = run(
            "curl", "-fsS", "--connect-timeout", "2", "--max-time", "5", "http://127.0.0.1:3002/"
        )
        add("firecrawl", code == 0, "API responds", "Inspect journalctl -u firecrawl", warning=True)
    if public_url:
        code, status = run(
            "curl",
            "-fsS",
            "--connect-timeout",
            "3",
            "--max-time",
            "10",
            "-o",
            "/dev/null",
            "-w",
            "%{http_code}",
            public_url.rstrip("/") + "/health",
        )
        add(
            "public-tls",
            code == 0 and status == "200",
            "HTTPS readiness and certificate valid",
            "Check DNS, Caddy and public TLS",
        )
    for name, args in (("node", ("--version",)), ("npm", ("--version",)), ("go", ("version",))):
        if shutil.which(name):
            code, version = run(name, *args)
            add(
                name,
                code == 0,
                version,
                f"Optional {name} is broken; lens host tools",
                warning=True,
            )
    return checks


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--json", action="store_true", help="machine-readable checks")
    parser.add_argument("--check", action="store_true", help="read-only (the default)")
    parser.add_argument("--strict", action="store_true", help="warnings also exit 1")
    parser.add_argument("--public-url", help="also check HTTPS /health")
    args = parser.parse_args()
    if args.public_url and not args.public_url.startswith("https://"):
        parser.error("--public-url must use https://")
    checks = diagnose(
        Path(os.environ.get("LENS_APP_DIR", "/opt/lens")),
        Path(os.environ.get("LENS_SRC_DIR", "/opt/lens-src")),
        os.environ.get("APP_USER", "lens"),
        args.public_url,
    )
    failed = any(c["status"] == "fail" or (args.strict and c["status"] == "warn") for c in checks)
    if args.json:
        print(json.dumps({"ok": not failed, "checks": checks}, indent=2))
    else:
        for check in checks:
            print(f"{check['status'].upper():4} {check['name']}: {check['detail']}")
        print("\nHost packages: lens host check | lens host update. Doctor changes nothing.")
    return int(failed)


if __name__ == "__main__":
    sys.exit(main())
