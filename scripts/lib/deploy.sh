#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

# Same exclusions for staging, deployment and rollback. These are instance data,
# never release files. The venv is copied separately for snapshots.
LENS_STATE_EXCLUDES=(
  --exclude='/.git' --exclude='/.venv' --exclude='/.venv.old'
  --exclude='/.env*' --exclude='/config.json'
  --exclude='/managed-files' --exclude='/data'
  --exclude='/.local' --exclude='/.config' --exclude='/.cache'
  --exclude='/input.csv' --exclude='/output.csv' --exclude='/progress.json'
  --exclude='/*.log' --exclude='/__pycache__' --exclude='/.pytest_cache'
)

lens_git() {
  local source="$1"; shift
  # Legacy installs have a service-owned checkout. Scope trust to this command
  # instead of writing root's global config (HOME may be unset under systemd).
  command git -c "safe.directory=$source" -C "$source" "$@"
}

lens_lock() {
  # flock's parent owns the lock across update.sh's self-exec; no inheritable
  # descriptor leaks into systemd or long-lived child processes.
  if [[ "${LENS_DEPLOY_LOCKED:-0}" != 1 ]]; then
    exec flock --nonblock --conflict-exit-code 75 --close /run/lock/lens-deploy.lock \
      env LENS_DEPLOY_LOCKED=1 bash "$0" "$@"
  fi
}

lens_health() {
  local _attempt
  for _attempt in {1..15}; do
    if [[ "$(curl -s --connect-timeout 2 --max-time 3 -o /dev/null -w '%{http_code}' \
        http://127.0.0.1:8808/health)" == 200 ]]; then return 0; fi
    sleep 1
  done
  return 1
}

lens_venv() {
  local tree="$1" version
  version="$(cat "$tree/.python-version")"
  # `install --upgrade` is unavailable in distro uv 0.7.22. The separate
  # upgrade subcommand works there and on current uv; install also covers a
  # fresh host. Patch availability comes from the installed uv's catalogue.
  runuser -u "$APP_USER" -- env "HOME=$APP_DIR" uv python install "$version" || return
  runuser -u "$APP_USER" -- env "HOME=$APP_DIR" uv python upgrade "$version" || return
  # A bootstrap re-run rebuilds over the existing venv. uv 0.7 replaced it
  # silently; current uv refuses without --clear, which 0.7 does not accept.
  # The environment variable works on both.
  runuser -u "$APP_USER" -- env "HOME=$APP_DIR" UV_VENV_CLEAR=1 uv venv --managed-python --python "$version" --relocatable "$tree/.venv" || return
  runuser -u "$APP_USER" -- env "HOME=$APP_DIR" uv pip install \
    --python "$tree/.venv/bin/python" -r "$tree/requirements.txt"
}

lens_restore_release() {
  local app="$1" backup="$2" unit="$3" cli="$4"
  rsync -a --checksum --delete "${LENS_STATE_EXCLUDES[@]}" \
    --exclude='/lens.service.previous' "$backup/" "$app/" || return
  rm -rf "$app/.venv" || return
  cp -a "$backup/.venv" "$app/.venv" || return
  install -m 0644 "$backup/lens.service.previous" "$unit" || return
  install -m 0755 "$app/deploy/lens-cli" "$cli"
}

lens_latest_fedora() {
  python3 -c 'import json,sys; print(max(int(r["version"]) for r in json.load(sys.stdin) if r.get("version", "").isascii() and r.get("version", "").isdigit()))'
}

# Keep the newest KEEP (default 2, minimum 1) <app_dir>.rollback.* snapshots
# and delete the rest. Older snapshots are never restore candidates.
lens_prune_rollbacks() {
  local app_dir="$1" keep="${2:-2}"
  [[ "$keep" =~ ^[0-9]+$ && "$keep" -ge 1 ]] || keep=2
  local old=()
  mapfile -t old < <(ls -1dt "${app_dir}".rollback.* 2>/dev/null | tail -n +"$((keep + 1))")
  (( ${#old[@]} )) || return 0
  rm -rf -- "${old[@]}"
  printf 'pruned %d older rollback snapshot(s); kept the newest %d\n' "${#old[@]}" "$keep"
}

# Keep the Cockpit web console off. The Vultr Fedora image ships cockpit, and
# cockpit.socket serves a root-capable web login on :9090 on a box whose only
# admin path is ssh. bootstrap.sh runs this on a new box and update.sh on every
# update, so an existing box stays healed (mirrors keel's step_cockpit).
#
# Safe on any host: does nothing (one line) unless systemd has a cockpit.socket
# unit; touches the firewall only when firewalld is running and a zone lists
# cockpit; never installs or removes packages, never touches ufw/iptables, and
# never fails the caller. Idempotent: a masked socket is left alone. With
# DRY_RUN=1 it prints the commands instead. Uses the caller's say/ok/warn.
_lens_cockpit_do() {
  if [[ "${DRY_RUN:-0}" == 1 ]]; then say "  [dry-run] $*"; else "$@"; fi
}
lens_cockpit_off() {
  local zone zones="" services fw_failed=0
  if ! command -v systemctl >/dev/null 2>&1 \
    || [[ -z "$(systemctl list-unit-files cockpit.socket --no-legend 2>/dev/null)" ]]; then
    say "  cockpit is not installed — no web console to turn off"
    return 0
  fi
  if [[ "$(systemctl is-enabled cockpit.socket 2>/dev/null || true)" == masked* ]]; then
    ok "cockpit.socket already masked — no web console on :9090"
  elif _lens_cockpit_do systemctl disable --now cockpit.socket \
    && _lens_cockpit_do systemctl mask cockpit.socket; then
    [[ "${DRY_RUN:-0}" == 1 ]] || ok "cockpit.socket disabled, stopped and masked — no web console on :9090"
  else
    warn "could not turn cockpit off — run: systemctl disable --now cockpit.socket && systemctl mask cockpit.socket"
  fi
  command -v firewall-cmd >/dev/null 2>&1 || return 0
  [[ "$(firewall-cmd --state 2>/dev/null || true)" == running ]] || return 0
  for zone in $(firewall-cmd --permanent --get-zones 2>/dev/null || true); do
    # Captured, not piped into grep -q: under pipefail an early grep exit can
    # SIGPIPE the writer and turn a match into a miss.
    services="$(firewall-cmd --permanent --zone="$zone" --list-services 2>/dev/null || true)"
    [[ " $services " == *" cockpit "* ]] || continue
    zones+="$zone "
    _lens_cockpit_do firewall-cmd -q --permanent --zone="$zone" --remove-service=cockpit || fw_failed=1
  done
  [[ -n "$zones" ]] || return 0
  _lens_cockpit_do firewall-cmd -q --reload || fw_failed=1
  if [[ "$fw_failed" == 1 ]]; then
    warn "could not remove cockpit from firewalld (zones: ${zones% }) — run: firewall-cmd --permanent --zone=<zone> --remove-service=cockpit && firewall-cmd --reload"
  elif [[ "${DRY_RUN:-0}" != 1 ]]; then
    ok "removed the cockpit service from firewalld (zones: ${zones% })"
  fi
  return 0
}
