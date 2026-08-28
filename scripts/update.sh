#!/usr/bin/env bash
# scripts/update.sh — staged update for a Lens install.

set -euo pipefail

SRC_DIR="${SRC_DIR:-/opt/lens-src}"
APP_DIR="${APP_DIR:-/opt/lens}"
APP_USER="${APP_USER:-lens}"
CLI_TARGET="/usr/local/bin/lens"
SERVICE="lens.service"

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

step "1/4  Fetching latest"
cd "$SRC_DIR"
git config --global --add safe.directory "$SRC_DIR" 2>/dev/null || true
before_sha="$(git rev-parse HEAD)"

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

  if [[ "$before_sha" == "$after_sha" ]]; then
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
  if ! git diff --quiet "$before_sha" "$after_sha" -- scripts/update.sh; then
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
# image was never pre-seeded. `podman system migrate` also kills any stale
# pause process so new mappings and the (possibly changed) unit sandbox
# apply. Skippable with LENS_UPDATE_SKIP_DEEP=1.
ensure_rootless_podman() {
  [[ "${LENS_UPDATE_SKIP_DEEP:-0}" == "1" ]] && return 0
  if ! command -v podman >/dev/null 2>&1; then
    dnf install -y podman >/dev/null 2>&1 || { warn "podman unavailable — deep scrapes disabled"; return 0; }
  fi
  local app_uid
  app_uid="$(id -u "$APP_USER")" || return 0
  if ! grep -q "^${APP_USER}:" /etc/subuid 2>/dev/null; then
    usermod --add-subuids 200000-265535 "$APP_USER" && ok "added subuid range for $APP_USER"
  fi
  if ! grep -q "^${APP_USER}:" /etc/subgid 2>/dev/null; then
    usermod --add-subgids 200000-265535 "$APP_USER" && ok "added subgid range for $APP_USER"
  fi
  loginctl enable-linger "$APP_USER" 2>/dev/null || true
  runuser -u "$APP_USER" -- env "XDG_RUNTIME_DIR=/run/user/$app_uid" "HOME=$APP_DIR" \
    podman system migrate 2>/dev/null || true
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
rsync -a --exclude='/.git' --exclude='/.venv' "$SRC_DIR/" "$STAGING/"
chown -R "$APP_USER:$APP_USER" "$STAGING"
runuser -u "$APP_USER" -- uv venv "$STAGING/.venv" >/dev/null
runuser -u "$APP_USER" -- uv pip install --python "$STAGING/.venv/bin/python" \
  --reinstall -r "$STAGING/requirements.txt" \
  || die "uv pip install failed — live install untouched"
ok "staging venv ready"

step "3/4  Swapping + restarting"
systemctl stop "$SERVICE" || true
# Keep runtime state out of --delete's reach: uploaded inputs/outputs
# (managed-files) and rootless podman's storage/config under the service
# user's home (.local/.config/.cache hold the pre-seeded Chrome image).
rsync -a --delete \
  --exclude='/.git' --exclude='/.venv' --exclude='/.env' \
  --exclude='/managed-files' \
  --exclude='/.local' --exclude='/.config' --exclude='/.cache' \
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
for i in 1 2 3 4 5 6 7 8 9 10; do
  code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8808/health 2>/dev/null || echo 000)
  if [[ "$code" == "200" ]]; then
    ok "lens /health → 200"
    rm -rf "$APP_DIR/.venv.old" 2>/dev/null || true
    break
  fi
  sleep 1
  if [[ "$i" == "10" ]]; then
    warn "lens didn't return 200 on /health within 10s"
    warn "  Rollback: systemctl stop $SERVICE && rm -rf $APP_DIR/.venv && mv $APP_DIR/.venv.old $APP_DIR/.venv && systemctl start $SERVICE"
    exit 1
  fi
done

say
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
printf '%s ✓ Updated %s → %s%s\n' "$c_bold" "${before_sha:0:12}" "${after_sha:0:12}" "$c_reset"
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
say
say "  Logs:      ${c_dim}lens logs${c_reset}"
say "  Roll back: ${c_dim}cd $SRC_DIR && sudo git checkout ${before_sha:0:12} && sudo lens rebuild${c_reset}"
say "             ${c_dim}(use rebuild, not update — update would fast-forward straight back)${c_reset}"
