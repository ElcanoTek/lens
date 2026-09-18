#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

# Same exclusions for staging, deployment and rollback. These are instance data,
# never release files. The venv is copied separately for snapshots.
LENS_STATE_EXCLUDES=(
  --exclude='/.git' --exclude='/.venv' --exclude='/.venv.old'
  --exclude='/.env*' --exclude='/config.json' --exclude='/firecrawl/.env'
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
  runuser -u "$APP_USER" -- env "HOME=$APP_DIR" uv venv --managed-python --python "$version" --relocatable "$tree/.venv" || return
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
