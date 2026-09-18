#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/deploy.sh
. "$SCRIPT_DIR/lib/deploy.sh"
cmd="${1:---help}"; shift || true
YES=() DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --yes|-y) YES=(-y) ;;
    --dry-run) DRY_RUN=1 ;;
    *) echo "Unknown argument: $arg" >&2; exit 2 ;;
  esac
done
case "$cmd" in
  --help|-h|help)
    cat <<'EOF'
lens host check                     refresh DNF metadata; list package updates
lens host update [--yes] [--dry-run] upgrade all packages in enabled repositories
lens host tools [--yes] [--dry-run]  install/upgrade distro Node + npm + Go
lens host upgrade [--yes] [--dry-run] download latest stable Fedora (max two releases)

Updates never reboot automatically. After an OS upgrade: lens rebuild; lens doctor.
tools is optional: Lens itself uses Python/uv, with browsers running in Podman.
--dry-run prints the plan; upgrade also reads Fedora's release feed.
EOF
    exit 0 ;;
  check|update|tools|upgrade) ;;
  *) echo "Unknown host command: $cmd" >&2; exit 2 ;;
esac
command -v dnf >/dev/null || { echo 'This command requires dnf' >&2; exit 1; }
if [[ "$DRY_RUN" == 0 && "$cmd" != check ]]; then
  [[ $EUID == 0 ]] || { echo 'Run through sudo lens host' >&2; exit 1; }
  lens_lock "$cmd" "$@"
fi
do_run() {
  printf ' >'; printf ' %q' "$@"; printf '\n'
  [[ "$DRY_RUN" == 1 ]] || "$@"
}
case "$cmd" in
  check)
    rc=0
    do_run dnf --refresh check-update || rc=$?
    # DNF uses 100 for available updates; reserve failure for actual errors.
    [[ "$rc" == 0 || "$rc" == 100 ]] || exit "$rc"
    ;;
  update)
    do_run dnf --refresh upgrade "${YES[@]}"
    echo 'Packages refreshed. Reboot for kernel/runtime changes, then: lens rebuild; lens doctor'
    ;;
  tools)
    do_run dnf --refresh install "${YES[@]}" nodejs npm golang
    do_run dnf --refresh upgrade "${YES[@]}" nodejs npm golang
    if [[ "$DRY_RUN" == 0 ]]; then node --version; npm --version; go version; fi
    ;;
  upgrade)
    # /etc/os-release is OS-owned, not application configuration.
    . /etc/os-release
    [[ "$ID" == fedora && "$VERSION_ID" =~ ^[1-9][0-9]*$ ]] \
      || { echo 'Release upgrades require Fedora; use vendor tooling for RHEL' >&2; exit 1; }
    latest="$(curl -fsS --connect-timeout 5 --max-time 30 https://fedoraproject.org/releases.json \
      | lens_latest_fedora)"
    [[ "$latest" =~ ^[1-9][0-9]*$ ]] || { echo 'No stable Fedora release found' >&2; exit 1; }
    if (( latest <= VERSION_ID )); then echo "Already on Fedora $VERSION_ID (latest stable: $latest)"; exit 0; fi
    (( latest - VERSION_ID <= 2 )) || { echo "Fedora $VERSION_ID → $latest needs intermediate upgrades (maximum two releases per hop)" >&2; exit 1; }
    echo "Fedora $VERSION_ID → $latest stable. Take a provider snapshot and finish active Lens jobs before the offline reboot."
    if command -v dnf5 >/dev/null; then
      # system-upgrade is built into DNF5; DNF4 needs the plugin.
      do_run dnf5 --refresh upgrade "${YES[@]}"
      do_run dnf5 system-upgrade download "${YES[@]}" "--releasever=$latest"
    else
      do_run dnf --refresh upgrade "${YES[@]}"
      do_run dnf install "${YES[@]}" dnf-plugin-system-upgrade
      do_run dnf system-upgrade download "${YES[@]}" "--releasever=$latest"
    fi
    if command -v dnf5 >/dev/null; then
      echo 'Download complete. Start the offline upgrade when ready: sudo dnf5 offline reboot'
    else
      echo 'Download complete. Start the offline upgrade when ready: sudo dnf system-upgrade reboot'
    fi
    echo 'After reconnecting: lens rebuild; lens doctor --strict'
    ;;
esac
