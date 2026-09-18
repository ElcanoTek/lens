#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# scripts/bootstrap.sh — interactive installer for Elcano Lens.
#
# What this does:
#   1. Installs system deps via dnf (python3, uv, selenium runtime libs)
#   2. Creates 'lens' system user + /opt/lens
#   3. Syncs source to /opt/lens-src and /opt/lens
#   4. Builds venv via uv and installs Python deps
#   5. Writes /opt/lens/.env and migrates persistent auth state to /var/lib/lens
#   6. Installs the lens systemd unit + operator CLI
#   7. Enables and starts the service; health-checks /health
#
# Usage:
#   sudo bash scripts/bootstrap.sh
#
# Re-run safe. Scripted: LENS_BOOTSTRAP_NON_INTERACTIVE=1
# plus AUTH_SIGNING_PUBKEY (from `auth pubkey`).

set -euo pipefail

if [[ ! -t 0 && -t 1 ]]; then exec </dev/tty; fi

APP_DIR="${APP_DIR:-/opt/lens}"
APP_USER="${APP_USER:-lens}"
SRC_DIR="${SRC_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
INSTALL_SRC_DIR="${LENS_SRC_DIR:-/opt/lens-src}"
# shellcheck source=lib/deploy.sh
. "$SRC_DIR/scripts/lib/deploy.sh"
ENV_FILE="$APP_DIR/.env"
CLI_TARGET="/usr/local/bin/lens"

if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
  c_reset=$'\033[0m' c_dim=$'\033[2m' c_red=$'\033[0;31m'
  c_green=$'\033[0;32m' c_yellow=$'\033[0;33m' c_cyan=$'\033[0;36m' c_bold=$'\033[1m'
else
  c_reset='' c_dim='' c_red='' c_green='' c_yellow='' c_cyan='' c_bold=''
fi
say()  { printf '%s\n' "$*"; }
info() { printf '%s» %s%s\n' "$c_dim" "$*" "$c_reset"; }
step() { printf '\n%s▸ %s%s\n' "$c_bold" "$*" "$c_reset"; }
ok()   { printf '%s✓ %s%s\n' "$c_green" "$*" "$c_reset"; }
warn() { printf '%s! %s%s\n' "$c_yellow" "$*" "$c_reset" >&2; }
die()  { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_reset" >&2; exit 1; }
ask()  { printf '%s?%s %s ' "$c_cyan" "$c_reset" "$*" >&2; }

NON_INTERACTIVE="${LENS_BOOTSTRAP_NON_INTERACTIVE:-0}"
# Firecrawl rides on the same rootless-podman plumbing as the deep scraper,
# so skipping deep implies skipping Firecrawl unless explicitly overridden —
# hardened hosts that pinned SKIP_DEEP=1 keep their no-podman guarantee.
LENS_BOOTSTRAP_SKIP_FIRECRAWL="${LENS_BOOTSTRAP_SKIP_FIRECRAWL:-${LENS_BOOTSTRAP_SKIP_DEEP:-0}}"
prompt() {
  local varname="$1" label="$2" default="${3:-}" answer=""
  if [[ -n "${!varname:-}" ]]; then printf '%s' "${!varname}"; return; fi
  if [[ "$NON_INTERACTIVE" == "1" ]]; then
    [[ -n "$default" || "${4:-}" == optional ]] || die "non-interactive + missing: set $varname"
    printf '%s' "$default"; return
  fi
  if [[ -n "$default" ]]; then ask "$label ${c_dim}[$default]${c_reset}:"
  else ask "$label:"; fi
  read -r answer
  [[ -z "$answer" ]] && answer="$default"
  printf '%s' "$answer"
}
prompt_secret() {
  local varname="$1" label="$2" answer=""
  if [[ -n "${!varname:-}" ]]; then printf '%s' "${!varname}"; return; fi
  if [[ "$NON_INTERACTIVE" == "1" ]]; then die "non-interactive + missing: set $varname"; fi
  ask "$label (input hidden):"
  read -r -s answer
  printf '\n' >&2
  [[ -n "$answer" ]] || die "$varname is required"
  printf '%s' "$answer"
}
genbase64() { openssl rand -base64 "$1" | tr -d '=\n' | tr '/+' '_-'; }
genhex()    { openssl rand -hex "$1"; }

[[ $EUID -eq 0 ]] || die "run as root: sudo bash scripts/bootstrap.sh"
[[ -f "$SRC_DIR/web_service.py" ]] || die "not a Lens checkout at $SRC_DIR"
lens_lock "$@"

cat <<EOF
${c_bold}Elcano Lens — bootstrap${c_reset}
${c_dim}Fedora / RHEL 9+  •  systemd  •  FastAPI scraper UI${c_reset}

Safe to re-run: existing .env is preserved; only missing values are prompted.

EOF

step "1/7  Installing system dependencies via dnf"
# chromium + chromedriver are needed by the selenium-based scrapers.
# If the operator runs scrapers headless-only, they can skip those
# packages by exporting LENS_BOOTSTRAP_SKIP_CHROME=1.
PKGS=(git curl jq python3 python3-devel gcc uv rsync openssl util-linux ca-certificates)
[[ "${LENS_BOOTSTRAP_SKIP_CHROME:-0}" == "1" ]] || PKGS+=(chromium chromedriver)
# podman drives the deep-scrape headless-Chrome container (auto mode's
# fallback for sites that block the fast crawler) and the Firecrawl stack.
[[ "${LENS_BOOTSTRAP_SKIP_DEEP:-0}" == "1" && "$LENS_BOOTSTRAP_SKIP_FIRECRAWL" == "1" ]] || PKGS+=(podman)
dnf install -y "${PKGS[@]}" >/dev/null
command -v uv >/dev/null 2>&1 || die "uv missing after dnf install"
ok "installed: ${PKGS[*]}"
# podman-compose runs the local Firecrawl stack (auto mode's JS-capable
# retry pass; see firecrawl/docker-compose.yaml). Installed separately so a
# repo without it (RHEL sans EPEL) degrades to no-Firecrawl instead of
# aborting the whole bootstrap.
if [[ "$LENS_BOOTSTRAP_SKIP_FIRECRAWL" != "1" ]] && ! command -v podman-compose >/dev/null 2>&1; then
  dnf install -y podman-compose >/dev/null 2>&1 \
    || warn "podman-compose unavailable (EPEL needed on RHEL) — Firecrawl disabled"
fi

step "2/7  Preparing $APP_DIR + '$APP_USER' user"
if ! id -u "$APP_USER" >/dev/null 2>&1; then
  useradd --system --home-dir "$APP_DIR" --shell /usr/sbin/nologin "$APP_USER"
fi
mkdir -p "$APP_DIR"
chown -R "$APP_USER:$APP_USER" "$APP_DIR"

# ── rootless podman for the deep-scrape + Firecrawl containers ──────────
# System users get no subuid/subgid ranges, so rootless podman can't unpack
# multi-uid images ("potentially insufficient UIDs or GIDs available").
# Lingering keeps /run/user/<uid> (podman's runtime root) alive for the
# systemd services, and pre-pulling images means first use doesn't download
# gigabytes mid-job.
NEED_PODMAN=0
[[ "${LENS_BOOTSTRAP_SKIP_DEEP:-0}" == "1" && "$LENS_BOOTSTRAP_SKIP_FIRECRAWL" == "1" ]] || NEED_PODMAN=1
if [[ "$NEED_PODMAN" == "1" ]] && command -v podman >/dev/null 2>&1; then
  app_uid="$(id -u "$APP_USER")"
  if ! grep -q "^${APP_USER}:" /etc/subuid 2>/dev/null; then
    usermod --add-subuids 200000-265535 "$APP_USER"
  fi
  if ! grep -q "^${APP_USER}:" /etc/subgid 2>/dev/null; then
    usermod --add-subgids 200000-265535 "$APP_USER"
  fi
  loginctl enable-linger "$APP_USER" 2>/dev/null || warn "could not enable lingering for $APP_USER"
  # migrate kills any stale pause process so new subuid mappings apply
  runuser -u "$APP_USER" -- env "XDG_RUNTIME_DIR=/run/user/$app_uid" "HOME=$APP_DIR" \
    podman system migrate 2>/dev/null || true

  pull_as_app_user() {
    runuser -u "$APP_USER" -- env "XDG_RUNTIME_DIR=/run/user/$app_uid" "HOME=$APP_DIR" \
      podman pull -q "$1" >/dev/null 2>&1
  }

  if [[ "${LENS_BOOTSTRAP_SKIP_DEEP:-0}" != "1" ]]; then
    CHROME_IMAGE="${LENS_DEEP_SCRAPE_IMAGE:-docker.io/selenium/standalone-chrome:latest}"
    info "pre-pulling $CHROME_IMAGE for deep scrapes (skippable: LENS_BOOTSTRAP_SKIP_DEEP=1)"
    if pull_as_app_user "$CHROME_IMAGE"; then
      ok "deep-scrape image pre-seeded"
    else
      warn "could not pre-pull $CHROME_IMAGE — deep scrapes will pull on first use"
    fi
  fi

  if [[ "$LENS_BOOTSTRAP_SKIP_FIRECRAWL" != "1" ]] && command -v podman-compose >/dev/null 2>&1; then
    # `firecrawl.sh pull` reads the image list straight from the compose
    # file, so there is exactly one place that defines tags.
    info "pre-pulling Firecrawl images (~3 GB; skippable: LENS_BOOTSTRAP_SKIP_FIRECRAWL=1)"
    if runuser -u "$APP_USER" -- env "XDG_RUNTIME_DIR=/run/user/$app_uid" "HOME=$APP_DIR" \
      bash "$SRC_DIR/scripts/firecrawl.sh" pull >/dev/null 2>&1; then
      ok "Firecrawl images pre-seeded"
    else
      warn "could not pre-pull Firecrawl images — first start will download them"
    fi
  fi
fi

if [[ ! -d "$INSTALL_SRC_DIR/.git" ]]; then
  # Seed WITH /.git so `lens update` can git fetch/merge later.
  # /.venv stays excluded — we rebuild it into APP_DIR below.
  [[ -d "$SRC_DIR/.git" ]] || die "bootstrap must run from a git checkout (no .git at $SRC_DIR)"
  mkdir -p "$INSTALL_SRC_DIR"
  rsync -a "${LENS_STATE_EXCLUDES[@]}" "$SRC_DIR/" "$INSTALL_SRC_DIR/"
  # .git is needed for subsequent updates, but local instance data is not.
  rsync -a "$SRC_DIR/.git/" "$INSTALL_SRC_DIR/.git/"
fi
# Keep runtime state out of --delete's reach: uploaded inputs/outputs
# (managed-files), the legacy auth DB during its one-time migration (data),
# and rootless podman's storage/config under the service user's home
# (.local/.config/.cache hold the pre-seeded Chrome image).
rsync -a --delete "${LENS_STATE_EXCLUDES[@]}" \
  "$INSTALL_SRC_DIR/" "$APP_DIR/"
chown -R "$APP_USER:$APP_USER" "$APP_DIR"

step "3/7  Building venv via uv"
lens_venv "$APP_DIR" || die "venv build failed"
ok "venv ready"

step "4/7  Configuring the instance"
if [[ -f "$ENV_FILE" ]]; then
  info "found existing $ENV_FILE — re-using values"
  while IFS= read -r -d '' key && IFS= read -r -d '' value; do
    [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
    # Load known configuration only, never execute .env as a root shell script.
    case "$key" in AUTH_*|LENS_*|OPENROUTER_API_KEY) export "$key=$value" ;; esac
  done < <("$APP_DIR/.venv/bin/python" - "$ENV_FILE" <<'PY'
import sys
from dotenv import dotenv_values
for key, value in dotenv_values(sys.argv[1], interpolate=False).items():
    if value is not None:
        sys.stdout.write(key + '\0' + value + '\0')
PY
  )
fi

# Unified auth: Lens verifies the elcano_auth cookie minted by the auth
# service (auth.elcanotek.com) using that service's Ed25519 PUBLIC key. Get it
# from the auth host with `auth pubkey`. Safe to paste — verify-only, can't
# mint sessions. Without it the app redirects every request to auth.
AUTH_SIGNING_PUBKEY="$(prompt AUTH_SIGNING_PUBKEY "auth service AUTH_SIGNING_PUBKEY (run 'auth pubkey' on the auth host)" "${AUTH_SIGNING_PUBKEY:-}")"
[[ -n "$AUTH_SIGNING_PUBKEY" ]] || die "AUTH_SIGNING_PUBKEY is required"
LENS_AUTH_MODE="$(prompt LENS_AUTH_MODE "Authentication mode: elcano (legacy magic link) or central" "${LENS_AUTH_MODE:-elcano}")"
case "$LENS_AUTH_MODE" in
  elcano) ;;
  central)
    AUTH_ISSUER_URL="$(prompt AUTH_ISSUER_URL "Central Auth issuer origin" "${AUTH_ISSUER_URL:-}")"
    LENS_PUBLIC_URL="$(prompt LENS_PUBLIC_URL "Public Lens origin" "${LENS_PUBLIC_URL:-}")"
    AUTH_CLIENT_ID="$(prompt AUTH_CLIENT_ID "Central Auth client ID" "${AUTH_CLIENT_ID:-lens}")"
    AUTH_CLIENT_SECRET="$(prompt_secret AUTH_CLIENT_SECRET "Central Auth client secret")"
    ;;
  *) die "LENS_AUTH_MODE must be elcano or central" ;;
esac
LENS_SESSION_SECRET="${LENS_SESSION_SECRET:-$(genbase64 32)}"
OPENROUTER_API_KEY="$(prompt_secret OPENROUTER_API_KEY "OpenRouter API key")"

# ── Caddy / TLS intent (install happens in step 7) ────────
HOSTNAME_FOR_TLS="$(prompt LENS_BOOTSTRAP_HOSTNAME "Public hostname for TLS (e.g. lens.example.com, blank to skip Caddy)" "${LENS_BOOTSTRAP_HOSTNAME:-}" optional)"
SETUP_CADDY="n"; USE_LETSENCRYPT="n"; LE_EMAIL=""
if [[ -n "$HOSTNAME_FOR_TLS" && "$HOSTNAME_FOR_TLS" != "localhost" ]]; then
  SETUP_CADDY_ANS="$(prompt LENS_BOOTSTRAP_SETUP_CADDY "Set up Caddy + auto-TLS for $HOSTNAME_FOR_TLS? (Y/n)" Y)"
  case "${SETUP_CADDY_ANS,,}" in y|yes) SETUP_CADDY="y" ;; esac
  if [[ "$SETUP_CADDY" == "y" ]]; then
    LE_ANS="$(prompt LENS_BOOTSTRAP_USE_LETSENCRYPT "Use Let's Encrypt (needs public 80/443)? (Y/n)" Y)"
    case "${LE_ANS,,}" in y|yes) USE_LETSENCRYPT="y" ;; esac
    if [[ "$USE_LETSENCRYPT" == "y" ]]; then
      LE_EMAIL="$(prompt LENS_BOOTSTRAP_LE_EMAIL "LE contact email (blank to skip)" "${LENS_BOOTSTRAP_LE_EMAIL:-}" optional)"
    fi
  fi
fi

umask 077
LENS_ACCESS_DB="${LENS_ACCESS_DB:-/var/lib/lens/access.db}"
export AUTH_SIGNING_PUBKEY LENS_AUTH_MODE LENS_SESSION_SECRET LENS_ACCESS_DB OPENROUTER_API_KEY
export AUTH_ISSUER_URL="${AUTH_ISSUER_URL:-}" LENS_PUBLIC_URL="${LENS_PUBLIC_URL:-}"
export AUTH_CLIENT_ID="${AUTH_CLIENT_ID:-}" AUTH_CLIENT_SECRET="${AUTH_CLIENT_SECRET:-}"
# Merge only the configured keys; retain custom settings and comments on rerun.
"$APP_DIR/.venv/bin/python" - "$ENV_FILE" <<'PY'
import os, sys
from dotenv import set_key
for key in ('AUTH_SIGNING_PUBKEY', 'LENS_AUTH_MODE', 'LENS_SESSION_SECRET',
            'LENS_ACCESS_DB', 'OPENROUTER_API_KEY', 'AUTH_ISSUER_URL',
            'LENS_PUBLIC_URL', 'AUTH_CLIENT_ID', 'AUTH_CLIENT_SECRET'):
    set_key(sys.argv[1], key, os.environ[key])
PY
chown "$APP_USER:$APP_USER" "$ENV_FILE"
# Owner-only: the file carries the session secret, the central-auth client
# secret, and the OpenRouter key.
chmod 0600 "$ENV_FILE"
ok "env seeded"

# Persistent auth state must not live beneath APP_DIR: release syncs replace
# that tree. Stop an existing service so no session or allowlist write can land
# between SQLite's online backup and the restart on the new database.
systemctl stop lens.service 2>/dev/null || true
"$APP_DIR/.venv/bin/python" "$APP_DIR/scripts/migrate_access_db.py" \
  --legacy "$APP_DIR/data/access.db" \
  --target /var/lib/lens/access.db \
  --env-file "$ENV_FILE" \
  --owner "$APP_USER" \
  || die "could not prepare persistent Lens access database"

step "5/7  Installing systemd units + CLI"
install -m 0644 "$APP_DIR/deploy/lens.service" /etc/systemd/system/
install -m 0755 "$APP_DIR/deploy/lens-cli" "$CLI_TARGET"
if [[ "$LENS_BOOTSTRAP_SKIP_FIRECRAWL" != "1" ]] && command -v podman-compose >/dev/null 2>&1; then
  install -m 0644 "$APP_DIR/deploy/firecrawl.service" /etc/systemd/system/
fi
systemctl daemon-reload
ok "units + CLI installed"

step "6/7  Starting lens"
if [[ "$LENS_BOOTSTRAP_SKIP_FIRECRAWL" != "1" ]] && command -v podman-compose >/dev/null 2>&1; then
  systemctl enable firecrawl.service >/dev/null 2>&1 || true
  if systemctl restart firecrawl.service; then
    ok "firecrawl stack running (127.0.0.1:3002)"
  else
    warn "firecrawl stack failed to start — auto mode continues without it (journalctl -u firecrawl)"
  fi
fi
systemctl enable lens.service
systemctl restart lens.service
healthy=0
for _ in $(seq 1 15); do
  code=$(curl -s --connect-timeout 2 --max-time 3 -o /dev/null -w '%{http_code}' http://127.0.0.1:8808/health 2>/dev/null || echo 000)
  if [[ "$code" == "200" ]]; then healthy=1; break; fi
  sleep 1
done
if [[ "$healthy" == "1" ]]; then
  ok "health check /health → 200"
else
  systemctl stop lens.service 2>/dev/null || true
  die "lens did not become ready within 15s — check: lens logs"
fi

# ── step 7: Caddy / TLS ─────────────────────────────────────────────────
step "7/7  Reverse proxy / TLS"
if [[ "$SETUP_CADDY" == "y" ]]; then
  if command -v dig >/dev/null 2>&1; then resolved_ip=$(dig +short "$HOSTNAME_FOR_TLS" A 2>/dev/null | tail -n1 || true); else resolved_ip=""; fi
  public_ip=$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)
  if [[ -n "$resolved_ip" && -n "$public_ip" && "$resolved_ip" != "$public_ip" ]]; then
    warn "DNS mismatch: $HOSTNAME_FOR_TLS → $resolved_ip, this box → $public_ip"
  elif [[ -z "$resolved_ip" ]]; then
    warn "$HOSTNAME_FOR_TLS doesn't resolve yet."
  elif [[ "$resolved_ip" == "$public_ip" ]]; then
    ok "DNS resolves correctly ($resolved_ip)"
  fi

  dnf install -y caddy >/dev/null
  install -d /etc/caddy/conf.d
  if [[ ! -f /etc/caddy/Caddyfile ]] || ! grep -qE '^[[:space:]]*import[[:space:]]' /etc/caddy/Caddyfile; then
    { echo ""; echo "# Managed by Elcano service bootstraps"; echo "import conf.d/*.caddy"; } >> /etc/caddy/Caddyfile
  fi
  if [[ -n "$LE_EMAIL" ]] && ! grep -qE '^[[:space:]]*email[[:space:]]' /etc/caddy/Caddyfile; then
    tmp=$(mktemp); printf '{\n\temail %s\n}\n\n' "$LE_EMAIL" > "$tmp"
    cat /etc/caddy/Caddyfile >> "$tmp"; install -m 0644 "$tmp" /etc/caddy/Caddyfile; rm -f "$tmp"
  fi

  tmp=$(mktemp)
  sed "s/{{HOSTNAME}}/$HOSTNAME_FOR_TLS/g" "$APP_DIR/deploy/lens.caddy" > "$tmp"
  if [[ "$USE_LETSENCRYPT" != "y" ]]; then
    awk -v host="$HOSTNAME_FOR_TLS" '$0 == host " {" { print; print "\ttls internal"; next } { print }' "$tmp" > "$tmp.2" && mv "$tmp.2" "$tmp"
  fi
  install -m 0644 "$tmp" /etc/caddy/conf.d/lens.caddy; rm -f "$tmp"

  if systemctl is-active --quiet firewalld 2>/dev/null; then
    firewall-cmd --add-service=http --permanent >/dev/null 2>&1 || true
    firewall-cmd --add-service=https --permanent >/dev/null 2>&1 || true
    firewall-cmd --reload >/dev/null 2>&1 || true
    ok "firewalld: http + https opened"
  fi

  systemctl enable caddy >/dev/null 2>&1 || true
  if systemctl is-active --quiet caddy; then systemctl reload caddy || die "caddy reload failed"
  else systemctl start caddy || die "caddy failed to start"; fi
  ok "Caddy running — auto-renews ~30 days before expiry, no cron needed"

  if [[ "$USE_LETSENCRYPT" == "y" ]]; then
    info "waiting for TLS at https://${HOSTNAME_FOR_TLS} (up to 45s)"
    tls_ok=0
    for _ in $(seq 1 45); do
      if curl -fsS --max-time 5 "https://${HOSTNAME_FOR_TLS}/health" -o /dev/null 2>/dev/null; then tls_ok=1; break; fi
      sleep 1
    done
    if [[ "$tls_ok" == "1" ]]; then
      expiry=$(echo | openssl s_client -servername "$HOSTNAME_FOR_TLS" -connect "${HOSTNAME_FOR_TLS}:443" 2>/dev/null | openssl x509 -noout -enddate 2>/dev/null | cut -d= -f2)
      ok "TLS live — cert valid until ${expiry:-unknown}"
    else
      warn "https://${HOSTNAME_FOR_TLS} didn't come up in 45s — check: journalctl -u caddy"
    fi
  fi
else
  info "skipping Caddy — reach lens at http://127.0.0.1:8808 (or via your own proxy)"
fi

# ── motd ─────────────────────────────────────────────────────────────────
# Single source of truth is deploy/motd; update.sh keeps it in sync on
# existing boxes.
install -m 0644 "$APP_DIR/deploy/motd" /etc/motd
lens_git "$INSTALL_SRC_DIR" rev-parse HEAD > "$APP_DIR/.deployed-revision"

say
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
printf '%s ✓ Lens installed%s\n' "$c_bold" "$c_reset"
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
say
if [[ "$SETUP_CADDY" == "y" ]]; then
  say "  URL         ${c_bold}https://$HOSTNAME_FOR_TLS${c_reset}"
else
  say "  URL         ${c_bold}http://127.0.0.1:8808${c_reset}  (front with your reverse proxy for HTTPS)"
fi
say "  Logs        ${c_dim}lens logs${c_reset}"
say "  Diagnose    ${c_dim}lens doctor${c_reset}"
say "  Maintain    ${c_dim}lens update --yes; lens host --help${c_reset}"
say
say "  Sign-in     ${c_dim}via the auth service (auth.elcanotek.com) — no local password${c_reset}"
if [[ -z "$AUTH_SIGNING_PUBKEY" ]]; then
  printf '  %s! AUTH_SIGNING_PUBKEY is unset — every request will redirect to auth.%s\n' "$c_yellow" "$c_reset"
  printf '  %s  Set it with: lens env edit  (paste the output of `auth pubkey`), then: lens restart%s\n' "$c_yellow" "$c_reset"
  say
fi
