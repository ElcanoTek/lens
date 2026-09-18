#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# scripts/update.sh — staged update for a Lens install.

set -euo pipefail

SRC_DIR="${SRC_DIR:-${LENS_SRC_DIR:-/opt/lens-src}}"
APP_DIR="${APP_DIR:-${LENS_APP_DIR:-/opt/lens}}"
APP_USER="${APP_USER:-lens}"
CLI_TARGET="/usr/local/bin/lens"
SERVICE="lens.service"
for arg in "$@"; do
  case "$arg" in
    --yes|-y) export LENS_UPDATE_YES=1 ;;
    --help|-h) echo 'Usage: lens update [--yes] (or lens rebuild to deploy the current checkout)'; exit 0 ;;
    *) echo "Unknown argument: $arg" >&2; exit 2 ;;
  esac
done
# shellcheck source=lib/deploy.sh
. "$SRC_DIR/scripts/lib/deploy.sh"

if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
  c_reset=$'\033[0m' c_dim=$'\033[2m' c_red=$'\033[0;31m'
  c_green=$'\033[0;32m' c_yellow=$'\033[0;33m' c_cyan=$'\033[0;36m' c_bold=$'\033[1m'
else
  c_reset='' c_dim='' c_red='' c_green='' c_yellow='' c_cyan='' c_bold=''
fi
say()  { printf '%s\n' "$*"; }
step() { printf '\n%s▸ %s%s\n' "$c_bold" "$*" "$c_reset"; }
ok()   { printf '%s✓ %s%s\n' "$c_green" "$*" "$c_reset"; }
warn() { printf '%s! %s%s\n' "$c_yellow" "$*" "$c_reset" >&2; }
die()  { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_reset" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root: sudo lens update"
[[ -d "$SRC_DIR/.git" ]] || die "no source checkout at $SRC_DIR"
[[ -d "$APP_DIR" ]]      || die "no existing install at $APP_DIR"
lens_lock "$@"

step "1/4  Fetching latest"
cd "$SRC_DIR"
git() { lens_git "$SRC_DIR" "$@"; }
before_sha="$(git rev-parse HEAD)"
[[ -z "$(git status --porcelain)" ]] || die "source checkout has local changes — commit or stash them first"

if [[ "${LENS_UPDATE_NO_PULL:-0}" == "1" ]]; then
  after_sha="$before_sha"
  # Set when a pull re-execs the fresh script (below) so the final
  # summary still shows the real old → new range.
  before_sha="${LENS_UPDATE_BASE_SHA:-$before_sha}"
  ok "rebuild-only mode — skipping fetch, building ${after_sha:0:12}"
  say
else
  git fetch --quiet origin

  # Resolve target branch.  If HEAD is attached we simply follow it.
  # If detached, try to recover from a local branch at this commit;
  # otherwise fall back to the repo's default branch (origin/HEAD).
  current_branch="$(git rev-parse --abbrev-ref HEAD)"
  if [[ -n "${LENS_UPDATE_BRANCH:-}" ]]; then
    target_branch="$LENS_UPDATE_BRANCH"
  elif [[ "$current_branch" != "HEAD" ]]; then
    target_branch="$current_branch"
  else
    mapfile -t matching < <(git branch --points-at HEAD --format='%(refname:short)')
    if [[ ${#matching[@]} -eq 1 ]]; then
      target_branch="${matching[0]}"
      warn "HEAD is detached — recovering tracked branch '$target_branch'"
    elif [[ ${#matching[@]} -gt 1 ]]; then
      target_branch="${matching[0]}"
      warn "HEAD is detached — multiple local branches match; using '$target_branch'"
    else
      target_branch="$(git rev-parse --abbrev-ref origin/HEAD | sed 's|^origin/||')"
      warn "HEAD is detached — defaulting to '$target_branch'"
      warn "  (set LENS_UPDATE_BRANCH to override)"
    fi
  fi
  target_ref="origin/$target_branch"
  after_sha="$(git rev-parse "$target_ref")"

  if [[ "$before_sha" == "$after_sha" && "$(cat "$APP_DIR/.deployed-revision" 2>/dev/null || true)" == "$after_sha" ]]; then
    ok "already on ${after_sha:0:12} — nothing to update"; exit 0
  fi
  say; printf '%s  incoming commits:%s\n' "$c_dim" "$c_reset"
  git --no-pager log --oneline --no-decorate "${before_sha}..${after_sha}" | sed 's/^/    /'; say

  if [[ "${LENS_UPDATE_YES:-0}" != "1" ]]; then
    count="$(git rev-list --count "${before_sha}..${after_sha}")"
    printf '%s?%s Apply %s%d%s commits — %s..%s? %s(y/N)%s ' \
      "$c_cyan" "$c_reset" "$c_bold" "$count" "$c_reset" \
      "${before_sha:0:12}" "${after_sha:0:12}" "$c_dim" "$c_reset"
    read -r answer
    case "${answer,,}" in y|yes) ;; *) warn "cancelled"; exit 1 ;; esac
  fi

  # Stay on a local branch — never detach HEAD.  If the branch already
  # exists, fast-forward it; otherwise create it from the fetched ref.
  if git show-ref --quiet --verify "refs/heads/$target_branch"; then
    git checkout --quiet "$target_branch"
    git merge --ff-only "$target_ref" || die "$target_branch has diverged from $target_ref — resolve manually"
  else
    git checkout --quiet -b "$target_branch" "$target_ref"
  fi

  # The shell running this script read the PRE-update file (bash holds the
  # old inode across the checkout above), so a fix to update.sh itself
  # would otherwise only take effect on the NEXT update. If this update
  # changed update.sh, re-exec the fresh copy in rebuild-only mode.
  if ! git diff --quiet "$before_sha" "$after_sha" -- scripts/update.sh scripts/lib/deploy.sh; then
    warn "update.sh changed in this update — re-executing the new version"
    exec env LENS_UPDATE_NO_PULL=1 LENS_UPDATE_YES=1 \
      LENS_UPDATE_BASE_SHA="$before_sha" bash "$SRC_DIR/scripts/update.sh"
  fi
fi

# ── 1b. unified-auth key ─────────────────────────────────────────────────
# Lens verifies the elcano_auth cookie with the auth service's PUBLIC key
# (AUTH_SIGNING_PUBKEY). On a box first updated across the auth migration the
# .env won't have it yet — and without it every request bounces to auth in a
# redirect loop. Make sure it's set before restarting.
ensure_auth_pubkey() {
  local found="" f v
  for f in "$APP_DIR/.env.shared" "$APP_DIR/.env"; do
    [[ -f "$f" ]] || continue
    v="$(sed -n 's/^[[:space:]]*AUTH_SIGNING_PUBKEY[[:space:]]*=[[:space:]]*//p' "$f" | tail -n1)"
    v="${v%[\"\']}"; v="${v#[\"\']}"
    [[ -n "$v" ]] && found="$v"
  done
  [[ -n "$found" ]] && return 0

  warn "AUTH_SIGNING_PUBKEY is not set — Lens can't verify the elcano_auth"
  warn "cookie, so every request will redirect to auth in a loop until it is."
  if [[ -t 0 ]]; then
    printf '%s?%s Paste it now (run `auth pubkey` on the auth host; blank to skip): ' "$c_cyan" "$c_reset"
    local pubkey_in; read -r pubkey_in
    if [[ -n "$pubkey_in" ]]; then
      [[ -f "$APP_DIR/.env" ]] || install -o "$APP_USER" -g "$APP_USER" -m 0640 /dev/null "$APP_DIR/.env"
      printf 'AUTH_SIGNING_PUBKEY="%s"\n' "$pubkey_in" >> "$APP_DIR/.env"
      chown "$APP_USER:$APP_USER" "$APP_DIR/.env"; chmod 0640 "$APP_DIR/.env"
      ok "AUTH_SIGNING_PUBKEY written to $APP_DIR/.env"
    else
      warn "skipped — set it later with: lens env edit   (then: lens restart)"
    fi
  else
    warn "non-interactive — set it with: lens env edit   (then: lens restart)"
  fi
}
ensure_auth_pubkey

# ── 1c. rootless podman for deep scrapes ─────────────────────────────────
# Heals boxes bootstrapped before deep-scrape support: system users get no
# subuid/subgid ranges (image unpack fails with "insufficient UIDs or
# GIDs"), services get no /run/user/<uid> without lingering, and the Chrome
# image was never pre-seeded. Migrate only after changing mappings: Podman
# migration stops every service-user container, including live Firecrawl.
# Skippable with LENS_UPDATE_SKIP_DEEP=1.
ensure_rootless_podman() {
  [[ "${LENS_UPDATE_SKIP_DEEP:-0}" == "1" ]] && return 0
  if ! command -v podman >/dev/null 2>&1; then
    dnf install -y podman >/dev/null 2>&1 || { warn "podman unavailable — deep scrapes disabled"; return 0; }
  fi
  local app_uid mappings_changed=0
  app_uid="$(id -u "$APP_USER")" || return 0
  if ! grep -q "^${APP_USER}:" /etc/subuid 2>/dev/null; then
    usermod --add-subuids 200000-265535 "$APP_USER" && ok "added subuid range for $APP_USER"
    mappings_changed=1
  fi
  if ! grep -q "^${APP_USER}:" /etc/subgid 2>/dev/null; then
    usermod --add-subgids 200000-265535 "$APP_USER" && ok "added subgid range for $APP_USER"
    mappings_changed=1
  fi
  loginctl enable-linger "$APP_USER" 2>/dev/null || true
  if [[ "$mappings_changed" == 1 ]]; then
    runuser -u "$APP_USER" -- env "XDG_RUNTIME_DIR=/run/user/$app_uid" "HOME=$APP_DIR" \
      podman system migrate
  fi
  local image="${LENS_DEEP_SCRAPE_IMAGE:-docker.io/selenium/standalone-chrome:latest}"
  if ! runuser -u "$APP_USER" -- env "XDG_RUNTIME_DIR=/run/user/$app_uid" "HOME=$APP_DIR" \
    podman image exists "$image" 2>/dev/null; then
    say "  pre-pulling $image for deep scrapes (one-time, ~1 GB)…"
    if runuser -u "$APP_USER" -- env "XDG_RUNTIME_DIR=/run/user/$app_uid" "HOME=$APP_DIR" \
      podman pull -q "$image" >/dev/null 2>&1; then
      ok "deep-scrape image pre-seeded"
    else
      warn "could not pre-pull $image — deep scrapes will pull on first use"
    fi
  fi
}
ensure_rootless_podman

step "2/4  Building staging venv"
# Stage beside $APP_DIR, not in /tmp: a venv built under /tmp keeps its
# SELinux tmp_t label across mv, and systemd refuses to exec tmp_t
# (203/EXEC Permission denied). Same filesystem also makes the final mv
# atomic and lets uv hardlink from its cache. /opt has no tmp reaper, so
# sweep leftovers from any previous run that died before its EXIT trap.
rm -rf "${APP_DIR}".staging.* 2>/dev/null || true
STAGING="$(mktemp -d "${APP_DIR}.staging.XXXXXX")"
trap 'rm -rf "$STAGING"' EXIT
rsync -a "${LENS_STATE_EXCLUDES[@]}" "$SRC_DIR/" "$STAGING/"
chown -R "$APP_USER:$APP_USER" "$STAGING"
lens_venv "$STAGING" || die "venv build failed — live install untouched"
ok "staging venv ready"

step "3/4  Swapping + restarting"
BACKUP="$(mktemp -d "${APP_DIR}.rollback.XXXXXX")"
rsync -a "${LENS_STATE_EXCLUDES[@]}" "$APP_DIR/" "$BACKUP/"
cp -a "$APP_DIR/.venv" "$BACKUP/.venv"
install -m 0644 /etc/systemd/system/lens.service "$BACKUP/lens.service.previous"
# Any failure after stopping restores the actual deployed code AND interpreter.
rollback() {
  local status=$?
  trap - EXIT
  if [[ "$status" != 0 ]]; then
    warn "deploy failed — restoring $BACKUP"
    systemctl stop "$SERVICE" || true
    if lens_restore_release "$APP_DIR" "$BACKUP" /etc/systemd/system/lens.service "$CLI_TARGET"; then
      command -v restorecon >/dev/null && restorecon -RF "$APP_DIR/.venv" || true
      systemctl daemon-reload
      systemctl start "$SERVICE" && lens_health && ok "previous Lens release restored" \
        || warn "rollback needs attention — lens logs; snapshot: $BACKUP"
    else
      warn "rollback needs attention — retained snapshot: $BACKUP"
    fi
  fi
  rm -rf "$STAGING"
  exit "$status"
}
trap rollback EXIT
systemctl stop "$SERVICE"
if ! "$STAGING/.venv/bin/python" "$STAGING/scripts/migrate_access_db.py" \
  --legacy "$APP_DIR/data/access.db" \
  --target /var/lib/lens/access.db \
  --env-file "$APP_DIR/.env" \
  --owner "$APP_USER"; then
  systemctl start "$SERVICE" 2>/dev/null || true
  die "could not prepare persistent Lens access database; previous service restarted"
fi
# Keep runtime state out of --delete's reach: uploaded inputs/outputs
# (managed-files), the legacy auth DB retained for operator verification
# (data), and rootless podman's storage/config under the service user's home
# (.local/.config/.cache hold the pre-seeded Chrome image).
rsync -a --delete "${LENS_STATE_EXCLUDES[@]}" \
  "$STAGING/" "$APP_DIR/"
if [[ -d "$APP_DIR/.venv" ]]; then
  rm -rf "$APP_DIR/.venv.old"; mv "$APP_DIR/.venv" "$APP_DIR/.venv.old"
fi
mv "$STAGING/.venv" "$APP_DIR/.venv"
chown -R "$APP_USER:$APP_USER" "$APP_DIR/.venv"
if command -v restorecon >/dev/null 2>&1; then
  restorecon -RF "$APP_DIR/.venv" || warn "restorecon failed for $APP_DIR/.venv"
fi

install -m 0644 "$APP_DIR/deploy/lens.service" /etc/systemd/system/
install -m 0755 "$APP_DIR/deploy/lens-cli" "$CLI_TARGET"

# ── Firecrawl local scraping stack ───────────────────────────────────────
# Installs or refreshes the Firecrawl stack (auto mode's JS-capable retry
# pass) and heals boxes bootstrapped before Firecrawl support existed.
# Runs after the swap so it uses the fresh deploy/ + firecrawl/ files.
# Skippable with LENS_UPDATE_SKIP_FIRECRAWL=1 (defaults to
# LENS_UPDATE_SKIP_DEEP so podman-free hosts stay podman-free).
ensure_firecrawl() {
  [[ "${LENS_UPDATE_SKIP_FIRECRAWL:-${LENS_UPDATE_SKIP_DEEP:-0}}" == "1" ]] && return 0
  if ! command -v podman >/dev/null 2>&1; then
    warn "podman unavailable — Firecrawl stays disabled"; return 0
  fi
  if ! command -v podman-compose >/dev/null 2>&1; then
    dnf install -y podman-compose >/dev/null 2>&1 \
      || { warn "podman-compose unavailable (EPEL needed on RHEL) — Firecrawl stays disabled"; return 0; }
  fi
  local app_uid; app_uid="$(id -u "$APP_USER")" || return 0

  # The stack tracks `latest`: every update pulls fresh images and restarts.
  # This briefly tears the stack down, so don't run `lens update` while a
  # scrape job is mid-flight. A broken upgrade self-heals — firecrawl.sh up
  # rebuilds the disposable queue state — and if the stack still won't come
  # up, Lens just runs without the Firecrawl pass until it's fixed.
  say "  refreshing Firecrawl images (tracks latest)…"
  runuser -u "$APP_USER" -- env "XDG_RUNTIME_DIR=/run/user/$app_uid" "HOME=$APP_DIR" \
    bash "$APP_DIR/scripts/firecrawl.sh" pull >/dev/null 2>&1 \
    || warn "could not pull Firecrawl images — restarting on the current ones"
  install -m 0644 "$APP_DIR/deploy/firecrawl.service" /etc/systemd/system/
  systemctl daemon-reload
  systemctl enable firecrawl.service >/dev/null 2>&1 || true
  if systemctl restart firecrawl.service; then
    ok "firecrawl stack running (127.0.0.1:3002)"
  else
    warn "firecrawl stack failed to start — auto mode continues without it (journalctl -u firecrawl)"
  fi
}
ensure_firecrawl
# Keep /etc/motd in sync with deploy/motd — boxes bootstrapped before the
# banner existed never got one, and this heals drift on every update.
if [[ -f "$APP_DIR/deploy/motd" ]] && ! cmp -s "$APP_DIR/deploy/motd" /etc/motd; then
  install -m 0644 "$APP_DIR/deploy/motd" /etc/motd
  ok "motd installed/refreshed"
fi
systemctl daemon-reload
systemctl enable "$SERVICE"
systemctl start "$SERVICE"
ok "service restarted"

step "4/4  Health check"
# Lens owns no /login route anymore — login is the unified elcano_auth cookie.
# Probe the public /health endpoint instead (/login would 404).
lens_health || die "Lens readiness failed — restoring the previous release"
printf '%s\n' "$after_sha" > "$APP_DIR/.deployed-revision"
rm -rf "$APP_DIR/.venv.old"
ok "lens /health → 200; previous release retained at $BACKUP"

say
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
printf '%s ✓ Updated %s → %s%s\n' "$c_bold" "${before_sha:0:12}" "${after_sha:0:12}" "$c_reset"
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
say
say "  Logs:      ${c_dim}lens logs${c_reset}"
say "  Previous:  ${c_dim}$BACKUP (code + venv; restore instructions in docs/DEPLOYMENT.md)${c_reset}"
say "  Diagnose:  ${c_dim}lens doctor${c_reset}"
