#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# scripts/update.sh — staged update for a Lens install.
#
# Fetch, build bin/lens in a staging tree beside $APP_DIR, snapshot the live
# release, stop, migrate the access DB, swap, restart and health-check; any
# failure after the stop restores the snapshot. The first run on a box still
# on the Python release arrives here by re-exec from the OLD update.sh
# (LENS_UPDATE_NO_PULL=1 LENS_UPDATE_YES=1 LENS_UPDATE_BASE_SHA=<old HEAD>,
# deploy lock already held), so this script must stand on its own: it
# installs the Go toolchain itself, snapshots the Python venv with the code so
# a failed health check brings the Python release back, and retires the venv
# only once the Go release has answered /health.

set -euo pipefail

SRC_DIR="${SRC_DIR:-${LENS_SRC_DIR:-/opt/lens-src}}"
APP_DIR="${APP_DIR:-${LENS_APP_DIR:-/opt/lens}}"
APP_USER="${APP_USER:-lens}"
CLI_TARGET="${LENS_CLI_TARGET:-/usr/local/bin/lens}"
# Host locations. The overrides exist for internal/deploytest, which runs
# this script end to end against a scratch tree with stub tools.
UNIT_DIR="${LENS_UNIT_DIR:-/etc/systemd/system}"
STATE_DIR="${LENS_STATE_DIR:-/var/lib/lens}"
MOTD_FILE="${LENS_MOTD_FILE:-/etc/motd}"
# Owner of bin/ and of the CLI copy: root, never the service user. Root runs
# /usr/local/bin/lens for every operator command (lens update, doctor, env
# edit, ...), so a binary the service account can rewrite would hand a
# compromised service root. The service only needs to execute bin/lens.
BIN_OWNER="${LENS_BIN_OWNER:-root:root}"
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

[[ $EUID -eq 0 || "${LENS_UPDATE_TEST_NONROOT:-0}" == 1 ]] || die "run as root: sudo lens update"
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
      [[ -f "$APP_DIR/.env" ]] || install -o "$APP_USER" -g "$APP_USER" -m 0600 /dev/null "$APP_DIR/.env"
      printf 'AUTH_SIGNING_PUBKEY="%s"\n' "$pubkey_in" >> "$APP_DIR/.env"
      chown "$APP_USER:$APP_USER" "$APP_DIR/.env"; chmod 0600 "$APP_DIR/.env"
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
# migration stops every service-user container.
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

step "2/4  Building staging release"
# go + make from dnf when missing (a first Go migration of a Python box has
# neither). GOTOOLCHAIN=auto then fetches go.mod's pinned release into the
# root-owned build cache (scripts/lib/deploy.sh).
lens_ensure_go || die "could not install the Go toolchain (dnf install golang make) — live install untouched"
# Stage beside $APP_DIR, not in /tmp: a tree built under /tmp keeps its
# SELinux tmp_t label across mv, and systemd refuses to exec tmp_t
# (203/EXEC Permission denied). Same filesystem also makes the swap cheap.
# /opt has no tmp reaper, so sweep leftovers from any previous run that died
# before its EXIT trap.
rm -rf "${APP_DIR}".staging.* "${APP_DIR}".deploybin.* 2>/dev/null || true
STAGING="$(mktemp -d "${APP_DIR}.staging.XXXXXX")"
# Root-only (mktemp -d is 0700, beside $APP_DIR under root-owned /opt): the
# copy of the new binary that root executes and installs as the CLI.
PRIV_DIR="$(mktemp -d "${APP_DIR}.deploybin.XXXXXX")"
trap 'rm -rf "$STAGING" "$PRIV_DIR"' EXIT
rsync -a "${LENS_STATE_EXCLUDES[@]}" "$SRC_DIR/" "$STAGING/"
describe="$(git describe --tags --always --dirty 2>/dev/null || printf '%s' "${after_sha:0:12}")"
lens_build "$STAGING" "$after_sha" "$describe" || die "build failed — live install untouched"
# Refuse a binary that cannot even report its own identity: nothing has been
# stopped yet, so this is the last cheap exit.
built="$("$STAGING/bin/lens" --version 2>/dev/null || true)"
[[ "$built" == *"$after_sha"* ]] || die "staged bin/lens does not report ${after_sha:0:12} (got: ${built:-nothing}) — live install untouched"
# Take root's copy while the staging tree is still root-owned: from the chown
# below the service user owns $STAGING and could swap its bin/ entry, so
# nothing root executes may come from there.
install -m 0755 "$STAGING/bin/lens" "$PRIV_DIR/lens"
# Likewise every file root installs into the system: the unit (its
# directives run as root) and the motd.
install -d -m 0755 "$PRIV_DIR/deploy"
install -m 0644 "$STAGING/deploy/lens.service" "$STAGING/deploy/motd" "$PRIV_DIR/deploy/"
chown -R "$BIN_OWNER" "$PRIV_DIR"
chown -R "$APP_USER:$APP_USER" "$STAGING"
chown -R "$BIN_OWNER" "$STAGING/bin"
ok "staging release ready: $built"

step "3/4  Swapping + restarting"
BACKUP="$(mktemp -d "${APP_DIR}.rollback.XXXXXX")"
rsync -a "${LENS_STATE_EXCLUDES[@]}" "$APP_DIR/" "$BACKUP/"
# A Python release (first migration) also needs its interpreter back on
# rollback; a Go release has no venv and the binary is already in bin/.
if [[ -d "$APP_DIR/.venv" ]]; then cp -a "$APP_DIR/.venv" "$BACKUP/.venv"; fi
install -m 0644 "$UNIT_DIR/lens.service" "$BACKUP/lens.service.previous"
# The CLI as it is: a copied bash script (Python) or a symlink (Go).
if [[ -e "$CLI_TARGET" || -L "$CLI_TARGET" ]]; then cp -a "$CLI_TARGET" "$BACKUP/lens-cli.previous"; fi
# Any failure after stopping restores the actual deployed code AND runtime.
rollback() {
  local status=$?
  trap - EXIT
  if [[ "$status" != 0 ]]; then
    warn "deploy failed — restoring $BACKUP"
    systemctl stop "$SERVICE" || true
    if lens_restore_release "$APP_DIR" "$BACKUP" "$UNIT_DIR/lens.service" "$CLI_TARGET"; then
      if command -v restorecon >/dev/null 2>&1; then
        for d in "$APP_DIR/bin" "$APP_DIR/.venv"; do
          if [[ -d "$d" ]]; then restorecon -RF "$d" >/dev/null 2>&1 || true; fi
        done
      fi
      systemctl daemon-reload
      if systemctl start "$SERVICE" && lens_health; then
        ok "previous Lens release restored"
      else
        warn "rollback needs attention — lens logs; snapshot: $BACKUP"
      fi
    else
      warn "rollback needs attention — retained snapshot: $BACKUP"
    fi
  fi
  rm -rf "$STAGING" "$PRIV_DIR"
  exit "$status"
}
trap rollback EXIT
systemctl stop "$SERVICE"
# Persistent auth state lives in $STATE_DIR (/var/lib/lens), outside the release tree.
# Stopped first so no session or allowlist write lands between the online
# backup and the restart on the new database.
if ! "$PRIV_DIR/lens" migrate-access-db \
  --legacy "$APP_DIR/data/access.db" \
  --target "$STATE_DIR/access.db" \
  --env-file "$APP_DIR/.env" \
  --owner "$APP_USER"; then
  systemctl start "$SERVICE" 2>/dev/null || true
  die "could not prepare persistent Lens access database; previous service restarted"
fi
# Keep runtime state out of --delete's reach: uploaded inputs/outputs
# (managed-files), the legacy auth DB retained for operator verification
# (data), rootless podman's storage/config under the service user's home
# (.local/.config/.cache hold the pre-seeded Chrome image) and a Python-era
# .venv, which stays until the new release is healthy.
rsync -a --delete "${LENS_STATE_EXCLUDES[@]}" \
  "$STAGING/" "$APP_DIR/"
chown -R "$BIN_OWNER" "$APP_DIR/bin"
if command -v restorecon >/dev/null 2>&1; then
  restorecon -RF "$APP_DIR/bin" || warn "restorecon failed for $APP_DIR/bin"
fi

install -m 0644 "$PRIV_DIR/deploy/lens.service" "$UNIT_DIR/"
# The operator CLI is the binary itself, as a root-owned copy outside the
# service-owned tree (see lens_install_cli); it replaces the copied bash
# script a Python release installed here.
lens_install_cli "$PRIV_DIR/lens" "$CLI_TARGET" "$BIN_OWNER"

# ── Remove the retired Firecrawl stack ───────────────────────────────────
# Firecrawl was removed from Lens (2026-10): it rescued few sites and slowed
# every run. Hosts installed before then still run firecrawl.service and hold
# its images (many GB, plus an orphaned copy from every past update). Clear
# the unit and exactly what its compose project (elcano-firecrawl) made:
#   - its containers: those carrying the project's compose label AND named
#     elcano-firecrawl_*/-*, removed with `rm -v` so the anonymous volumes
#     they declared go with them;
#   - its one named volume and its networks, by exact name;
#   - its images, without -f: an image another container still uses (redis
#     and rabbitmq are common) stays;
#   - dangling images (untagged, used by no container): re-pullable.
# Nothing else of this user's podman storage is touched (no prune of
# volumes: an unused volume may hold data). A step that fails is reported.
# Idempotent: does nothing once the stack is gone.
FIRECRAWL_VOLUMES="elcano-firecrawl_nuq-postgres-data"
FIRECRAWL_NETWORKS="elcano-firecrawl_backend elcano-firecrawl_default"
remove_firecrawl() {
  local unit="$UNIT_DIR/firecrawl.service" removed=0 failed=0
  if [[ -f "$unit" ]]; then
    systemctl disable --now firecrawl.service >/dev/null 2>&1 || true
    rm -f "$unit"
    systemctl daemon-reload
    removed=1
  fi
  if command -v podman >/dev/null 2>&1 && id -u "$APP_USER" >/dev/null 2>&1; then
    local app_uid; app_uid="$(id -u "$APP_USER")"
    local pod=(runuser -u "$APP_USER" -- env "XDG_RUNTIME_DIR=/run/user/$app_uid" "HOME=$APP_DIR" podman)
    local ids="" images volumes networks label item
    for label in io.podman.compose.project com.docker.compose.project; do
      ids+="$(cd / && "${pod[@]}" ps -a --filter "label=$label=elcano-firecrawl" --format '{{.ID}} {{.Names}}' 2>/dev/null \
        | awk '$2 ~ /^elcano-firecrawl[_-]/ { print $1 }' || true)"$'\n'
    done
    ids="$(printf '%s' "$ids" | sed '/^$/d' | sort -u)"
    images="$(cd / && "${pod[@]}" images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null \
      | grep -E '^ghcr\.io/firecrawl/|^docker\.io/library/(redis:alpine|rabbitmq:3-alpine)$' || true)"
    volumes="$(cd / && "${pod[@]}" volume ls -q 2>/dev/null | grep -xF -f <(tr ' ' '\n' <<<"$FIRECRAWL_VOLUMES") || true)"
    networks="$(cd / && "${pod[@]}" network ls --format '{{.Name}}' 2>/dev/null | grep -xF -f <(tr ' ' '\n' <<<"$FIRECRAWL_NETWORKS") || true)"
    if [[ -n "$ids$images$volumes$networks" ]]; then
      removed=1
      for item in $ids; do (cd / && "${pod[@]}" rm -f -v "$item") >/dev/null 2>&1 || failed=1; done
      for item in $volumes; do (cd / && "${pod[@]}" volume rm "$item") >/dev/null 2>&1 || failed=1; done
      for item in $networks; do (cd / && "${pod[@]}" network rm "$item") >/dev/null 2>&1 || failed=1; done
      for item in $images; do (cd / && "${pod[@]}" rmi "$item") >/dev/null 2>&1 || failed=1; done
      (cd / && "${pod[@]}" image prune -f) >/dev/null 2>&1 || failed=1
    fi
  fi
  rm -rf "$APP_DIR/firecrawl"
  if [[ "$failed" == "1" ]]; then
    warn "could not remove every Firecrawl container, volume, network or image (an image another container uses is kept); see 'podman ps -a', 'podman volume ls' and 'podman images' as $APP_USER"
  elif [[ "$removed" == "1" ]]; then
    ok "removed the retired Firecrawl stack (service, containers, its volumes, images)"
  fi
  return 0
}
remove_firecrawl
# Keep /etc/motd in sync with deploy/motd — boxes bootstrapped before the
# banner existed never got one, and this heals drift on every update.
if [[ -f "$PRIV_DIR/deploy/motd" ]] && ! cmp -s "$PRIV_DIR/deploy/motd" "$MOTD_FILE"; then
  install -m 0644 "$PRIV_DIR/deploy/motd" "$MOTD_FILE"
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
# The Go release is healthy, so the Python interpreter it replaced is retired
# now — never earlier, because the rollback above restores it from the
# snapshot and a failed first migration must come back up on Python. The
# snapshot keeps its copy until lens_prune_rollbacks ages it out.
if [[ -d "$APP_DIR/.venv" || -d "$APP_DIR/.venv.old" ]]; then
  rm -rf "$APP_DIR/.venv" "$APP_DIR/.venv.old"
  ok "retired the Python virtualenv"
fi
ok "lens /health → 200; previous release retained at $BACKUP"

# Every update leaves a full code snapshot (the first Go migration's also
# holds the ~250 MB Python venv); keep only the newest few so a box updated
# weekly doesn't fill its disk.
lens_prune_rollbacks "$APP_DIR" "${LENS_KEEP_ROLLBACKS:-2}"

say
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
printf '%s ✓ Updated %s → %s%s\n' "$c_bold" "${before_sha:0:12}" "${after_sha:0:12}" "$c_reset"
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
say
say "  Logs:      ${c_dim}lens logs${c_reset}"
say "  Previous:  ${c_dim}$BACKUP (restore instructions in docs/DEPLOYMENT.md)${c_reset}"
say "  Diagnose:  ${c_dim}lens doctor${c_reset}"
