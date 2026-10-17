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

# The Vultr Fedora image ships Cockpit's root-capable web login on :9090; our boxes are administered over ssh only.
# Bootstrap only: remove every cockpit package when cockpit-ws is installed.
# LENS_KEEP_COCKPIT=1 skips it; silent without rpm or cockpit; never touches
# firewalld; honours DRY_RUN=1; warns and continues on failure. Uses the
# caller's info/ok/warn.
lens_cockpit_uninstall() {
  if [[ "${LENS_KEEP_COCKPIT:-0}" == 1 ]]; then
    info "LENS_KEEP_COCKPIT=1 — leaving Cockpit installed"
    return 0
  fi
  command -v rpm >/dev/null 2>&1 && rpm -q cockpit-ws >/dev/null 2>&1 || return 0
  if [[ "${DRY_RUN:-0}" == 1 ]]; then
    info "[dry-run] dnf -y remove 'cockpit*'"
  elif dnf -y remove 'cockpit*' >/dev/null; then
    ok "removed Cockpit — no web console on :9090"
  else
    warn "could not remove Cockpit — run: dnf -y remove 'cockpit*'"
  fi
  return 0
}

# Vultr's cloud-init writes /etc/ssh/sshd_config.d/50-cloud-init.conf with
# `PasswordAuthentication yes`, so a fresh box takes root password guesses all
# day. The Vultr startup script "elcano-key-only-ssh" fixes that at first boot;
# this is the net for when nobody selected it. Same drop-in, same name.
SSH_KEY_ONLY_CONF="# Key-only SSH: no passwords, root by key only. Sorts before cloud-init's
# 50-cloud-init.conf so it wins (sshd keeps the first value it reads).
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password"

# lens_count_authorized_keys — how many public keys sshd would accept for root
# and /home/* users, honouring the effective AuthorizedKeysFile from `sshd -T`
# (keel's deploy/harden-host.sh count_authorized_keys). The lockout guard.
# SSH_KEY_ONLY_ROOT prefixes every path (tests only).
lens_count_authorized_keys() {
  local root="${SSH_KEY_ONLY_ROOT:-}" line home rel f path count=0
  local -a files=()
  line="$(sshd -T 2>/dev/null | grep -i '^authorizedkeysfile ' || true)"
  [[ -n "$line" ]] && read -ra files <<<"${line#* }"
  [[ ${#files[@]} -eq 0 ]] && files=(.ssh/authorized_keys .ssh/authorized_keys2)
  for home in "$root"/root "$root"/home/*; do
    [[ -d "$home" ]] || continue
    rel="${home#"$root"}"
    for f in "${files[@]}"; do
      [[ "$f" == none ]] && continue
      path="${f//%h/$rel}"
      path="${path//%u/${rel##*/}}"
      [[ "$path" != /* ]] && path="$rel/$path"
      path="$root$path"
      [[ -r "$path" ]] || continue
      count=$(( count + $(grep -cE '^[[:space:]]*(ssh-|ecdsa-|sk-)' "$path" || true) ))
    done
  done
  printf '%s' "$count"
}

# Bootstrap only: install /etc/ssh/sshd_config.d/01-no-password-login.conf
# (passwords off, root by key only), check it with `sshd -t`, reload sshd.
# LENS_KEEP_SSH_PASSWORDS=1 skips it. Silent without sshd or sshd_config.d;
# a no-op when the file already has this content; skips with a warning when
# no authorized key is installed (it would lock everyone out). Honours
# DRY_RUN; never fails the caller. Uses the caller's info/ok/warn.
lens_ssh_key_only() {
  if [[ "${LENS_KEEP_SSH_PASSWORDS:-0}" == 1 ]]; then
    info "LENS_KEEP_SSH_PASSWORDS=1 — leaving sshd's password settings alone"
    return 0
  fi
  local dir="${SSH_KEY_ONLY_ROOT:-}/etc/ssh/sshd_config.d"
  local conf="$dir/01-no-password-login.conf" keys tmp prev=""
  command -v sshd >/dev/null 2>&1 && [[ -d "$dir" ]] || return 0
  if [[ -f "$conf" ]] && printf '%s\n' "$SSH_KEY_ONLY_CONF" | cmp -s - "$conf"; then
    return 0
  fi
  keys="$(lens_count_authorized_keys)"
  if [[ "${keys:-0}" -lt 1 ]]; then
    warn "no SSH authorized keys found — leaving password login on so nobody is locked out; add a key, then re-run bootstrap"
    return 0
  fi
  # A key only helps if sshd will take it: with public-key login off, turning
  # passwords off locks everyone out too.
  if ! sshd -T 2>/dev/null | grep -qix 'pubkeyauthentication yes'; then
    warn "sshd does not accept public keys (PubkeyAuthentication) — leaving password login on so nobody is locked out"
    return 0
  fi
  if [[ "${DRY_RUN:-0}" == 1 ]]; then
    info "[dry-run] would write $conf (key-only SSH), check it with sshd -t and reload sshd"
    return 0
  fi
  tmp="$(mktemp)" || { warn "could not write $conf — SSH still accepts passwords"; return 0; }
  # Keep a differing hand-made copy so a failed sshd -t puts it back.
  if [[ -f "$conf" ]]; then prev="$(mktemp)" && cp -p "$conf" "$prev" || prev=""; fi
  printf '%s\n' "$SSH_KEY_ONLY_CONF" >"$tmp"
  if ! install -m 0600 "$tmp" "$conf"; then
    rm -f "$tmp" ${prev:+"$prev"}
    warn "could not write $conf — SSH still accepts passwords"
    return 0
  fi
  rm -f "$tmp"
  if ! sshd -t >/dev/null 2>&1; then
    if [[ -n "$prev" ]]; then mv -f "$prev" "$conf"; else rm -f "$conf"; fi
    warn "sshd -t rejected the key-only drop-in — reverted $conf; sshd left as it was"
    return 0
  fi
  [[ -n "$prev" ]] && rm -f "$prev"
  if systemctl reload sshd >/dev/null 2>&1 || systemctl reload ssh >/dev/null 2>&1; then
    ok "SSH is key-only: passwords off, root by key ($conf)"
  else
    warn "wrote $conf but could not reload sshd — run: systemctl reload sshd"
  fi
  if sshd -T 2>/dev/null | grep -qiE '^(passwordauthentication|permitrootlogin) yes$'; then
    warn "sshd still accepts passwords: an earlier directive wins (e.g. 'PermitRootLogin yes' above the Include line in /etc/ssh/sshd_config) — fix it by hand"
  fi
  return 0
}
