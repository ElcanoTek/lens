#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

# Same exclusions for staging, deployment and rollback. These are instance data,
# never release files. A Python-era .venv is excluded too, so --delete never
# removes the interpreter a first Go migration may still have to roll back to;
# update.sh snapshots it separately and retires it only after a healthy start.
LENS_STATE_EXCLUDES=(
  --exclude='/.git' --exclude='/.venv' --exclude='/.venv.old'
  --exclude='/.env*' --exclude='/config.json'
  --exclude='/managed-files' --exclude='/data'
  --exclude='/.local' --exclude='/.config' --exclude='/.cache'
  --exclude='/input.csv' --exclude='/output.csv' --exclude='/progress.json'
  --exclude='/*.log'
)

# Root-owned Go caches for every deploy build: GOPATH, the module cache, the
# build cache and any toolchain GOTOOLCHAIN=auto downloads. Keeping them out
# of $HOME means builds neither litter /root nor depend on HOME being set
# (systemd and sudo -i differ), and reruns reuse the downloaded modules.
LENS_BUILD_CACHE="${LENS_BUILD_CACHE:-/var/cache/lens-build}"

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
  local _attempt attempts="${LENS_HEALTH_ATTEMPTS:-15}"
  for (( _attempt = 0; _attempt < attempts; _attempt++ )); do
    if [[ "$(curl -s --connect-timeout 2 --max-time 3 -o /dev/null -w '%{http_code}' \
        http://127.0.0.1:8808/health)" == 200 ]]; then return 0; fi
    sleep 1
  done
  return 1
}

# lens_build TREE [COMMIT] [VERSION] — `make build` in TREE, emitting
# TREE/bin/lens. Staging trees carry no .git, so the caller passes the
# revision and describe string the Makefile would otherwise ask git for.
# GOTOOLCHAIN=auto (also the Makefile default) lets a lagging distro golang
# fetch go.mod's pinned toolchain; GOENV=off ignores any per-user go env file
# and the module proxy/sumdb are the upstream defaults unless the caller set
# them (Fedora's go.env says GOPROXY=direct, which is slower and needs every
# origin reachable).
lens_build() {
  local tree="$1" commit="${2:-}" version="${3:-}" cache="$LENS_BUILD_CACHE"
  local -a vars=()
  [[ -n "$commit" ]] && vars+=("COMMIT=$commit")
  [[ -n "$version" ]] && vars+=("VERSION=$version")
  install -d -m 0700 "$cache" || return
  env GOTOOLCHAIN="${GOTOOLCHAIN:-auto}" GOENV=off \
    GOPATH="$cache/gopath" GOMODCACHE="$cache/gomod" GOCACHE="$cache/gocache" \
    GOPROXY="${GOPROXY:-https://proxy.golang.org,direct}" GOSUMDB="${GOSUMDB:-sum.golang.org}" \
    HOME="$cache" make -C "$tree" --no-print-directory build "${vars[@]}" || return
  [[ -x "$tree/bin/lens" ]] || { echo "make build did not emit $tree/bin/lens" >&2; return 1; }
}

# lens_ensure_go — install the distro Go toolchain (plus make) when missing.
# Any Go >= 1.21 is enough: GOTOOLCHAIN=auto downloads the pinned release.
lens_ensure_go() {
  local -a missing=()
  command -v go >/dev/null 2>&1 || missing+=(golang)
  command -v make >/dev/null 2>&1 || missing+=(make)
  (( ${#missing[@]} )) || return 0
  dnf install -y "${missing[@]}" >/dev/null || return
  command -v go >/dev/null 2>&1 && command -v make >/dev/null 2>&1
}

# lens_restore_release APP BACKUP UNIT CLI — put a snapshot taken by
# update.sh back in place: code, the Python venv when the snapshot has one (a
# failed first Go migration restores the Python release), the systemd unit and
# the operator CLI. The CLI is a copy of deploy/lens-cli on Python releases and
# a symlink to bin/lens on Go releases; lens-cli.previous holds whichever it
# was. Snapshots written by the Python-era update.sh have no lens-cli.previous,
# so fall back to what the restored tree ships.
lens_restore_release() {
  local app="$1" backup="$2" unit="$3" cli="$4"
  rsync -a --checksum --delete "${LENS_STATE_EXCLUDES[@]}" \
    --exclude='/lens.service.previous' --exclude='/lens-cli.previous' \
    "$backup/" "$app/" || return
  rm -rf "$app/.venv" || return
  if [[ -d "$backup/.venv" ]]; then
    cp -a "$backup/.venv" "$app/.venv" || return
  fi
  install -m 0644 "$backup/lens.service.previous" "$unit" || return
  # rm first: install/cp onto a symlink would write through it into bin/lens.
  rm -f "$cli" || return
  if [[ -e "$backup/lens-cli.previous" || -L "$backup/lens-cli.previous" ]]; then
    cp -a "$backup/lens-cli.previous" "$cli"
  elif [[ -x "$app/bin/lens" ]]; then
    ln -s "$app/bin/lens" "$cli"
  else
    install -m 0755 "$app/deploy/lens-cli" "$cli"
  fi
}

# lens_latest_fedora — read Fedora's releases.json on stdin and print the
# newest stable release number. Betas ("45 Beta") and rawhide are not
# numbers, so they never qualify; no numeric release at all fails closed.
lens_latest_fedora() {
  jq -er '[.[]? | .version? | strings | select(test("^[0-9]+$")) | tonumber] | max'
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
  local conf="$dir/01-no-password-login.conf" keys tmp prev="" eff
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
  # Capture sshd -T first: under pipefail, grep -q exiting early can SIGPIPE
  # sshd and the pipeline then reads as "no match".
  eff="$(sshd -T 2>/dev/null || true)"
  if ! grep -qix 'pubkeyauthentication yes' <<<"$eff"; then
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
    # cp onto the new inode, not mv: mv would carry /tmp's SELinux label along.
    if [[ -n "$prev" ]]; then cp "$prev" "$conf"; rm -f "$prev"; else rm -f "$conf"; fi
    warn "sshd -t rejected the key-only drop-in — reverted $conf; sshd left as it was"
    return 0
  fi
  [[ -n "$prev" ]] && rm -f "$prev"
  if systemctl reload sshd >/dev/null 2>&1 || systemctl reload ssh >/dev/null 2>&1; then
    ok "SSH is key-only: passwords off, root by key ($conf)"
  else
    warn "wrote $conf but could not reload sshd — run: systemctl reload sshd"
  fi
  eff="$(sshd -T 2>/dev/null || true)"
  if grep -qiE '^(passwordauthentication|permitrootlogin) yes$' <<<"$eff"; then
    warn "sshd still accepts passwords: an earlier directive wins (e.g. 'PermitRootLogin yes' above the Include line in /etc/ssh/sshd_config) — fix it by hand"
  fi
  return 0
}
