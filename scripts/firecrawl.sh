#!/usr/bin/env bash
# scripts/firecrawl.sh — manage the local self-hosted Firecrawl stack.
#
# Lens uses Firecrawl (when running) as a fast JS-capable retry pass in auto
# scrape mode, and as the backend for --scrape-mode firecrawl. The stack is
# fully local: nothing leaves this box except the page fetches themselves.
#
# Usage: scripts/firecrawl.sh {up|down|status|logs|pull|reset}
#
# Versioning: the stack tracks `latest` and is refreshed on every
# `lens update`. Firecrawl's database is disposable queue state (all real
# output lives in Lens's CSVs), so `up` self-heals a broken upgrade by
# wiping that state and retrying; `reset` does the same on demand. Pin
# FIRECRAWL_TAG in firecrawl/.env if a deployment ever needs to.

set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$REPO_DIR/firecrawl/docker-compose.yaml"
API_URL="${FIRECRAWL_URL:-http://127.0.0.1:3002}"

# Rootless podman needs the user runtime dir; systemd services with User=
# don't get it set (lingering must be enabled — bootstrap handles that).
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"

if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
  c_reset=$'\033[0m' c_red=$'\033[0;31m' c_green=$'\033[0;32m'
  c_yellow=$'\033[0;33m' c_bold=$'\033[1m'
else
  c_reset='' c_red='' c_green='' c_yellow='' c_bold=''
fi
say()  { printf '%s\n' "$*"; }
step() { printf '\n%s▸ %s%s\n' "$c_bold" "$*" "$c_reset"; }
ok()   { printf '%s✓ %s%s\n' "$c_green" "$*" "$c_reset"; }
warn() { printf '%s! %s%s\n' "$c_yellow" "$*" "$c_reset" >&2; }
die()  { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_reset" >&2; exit 1; }

command -v podman-compose >/dev/null 2>&1 \
  || die "podman-compose is required (dnf install podman-compose)"
[[ -f "$COMPOSE_FILE" ]] || die "compose file not found: $COMPOSE_FILE"

# Run from the compose file's directory so podman-compose resolves the
# optional firecrawl/.env the same way regardless of the caller's cwd
# (systemd runs this with WorkingDirectory=/opt/lens).
compose() { (cd "$(dirname "$COMPOSE_FILE")" && podman-compose -f "$(basename "$COMPOSE_FILE")" "$@"); }

wait_for_api() {
  local deadline=$((SECONDS + 180))
  while ((SECONDS < deadline)); do
    if curl -sf -m 3 -o /dev/null "$API_URL/"; then
      return 0
    fi
    sleep 2
  done
  return 1
}

start_fresh() {
  # Queue state is disposable; a wipe recovers schema-mismatch upgrades.
  compose down -v
  compose up -d
  wait_for_api
}

case "${1:-}" in
  up)
    step "Starting Firecrawl stack"
    compose up -d
    step "Waiting for API at $API_URL"
    if wait_for_api; then
      ok "Firecrawl is ready at $API_URL"
    else
      warn "API not ready — rebuilding disposable queue state and retrying"
      if start_fresh; then
        ok "Firecrawl is ready at $API_URL (after state rebuild)"
      else
        die "Firecrawl API still not ready (try: $0 logs api)"
      fi
    fi
    ;;
  down)
    step "Stopping Firecrawl stack"
    compose down
    ok "Stopped"
    ;;
  status)
    if curl -sf -m 3 -o /dev/null "$API_URL/"; then
      ok "Firecrawl API is up at $API_URL"
    else
      warn "Firecrawl API is not responding at $API_URL"
    fi
    podman ps --filter "name=elcano-firecrawl" \
      --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"
    ;;
  logs)
    shift || true
    compose logs --tail 100 "$@"
    ;;
  pull)
    step "Pulling Firecrawl images"
    compose pull
    ok "Images updated (restart with: $0 up)"
    ;;
  reset)
    step "Wiping Firecrawl queue state and starting fresh"
    if start_fresh; then
      ok "Firecrawl is ready at $API_URL"
    else
      die "Firecrawl API did not become ready after reset (try: $0 logs api)"
    fi
    ;;
  *)
    say "Usage: $0 {up|down|status|logs [service]|pull|reset}"
    exit 1
    ;;
esac
