#!/usr/bin/env bash

# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

# scripts/lib/hostname.sh — name a new box after its service FQDN.
# Sourced by bootstrap.sh only: update never renames a box.
#
# Vultr boxes come up as "vultr.guest" (or a short label), so every prompt
# read root@vultr. lens_set_hostname renames the box to the public hostname
# bootstrap configures, but only when the current static hostname is exactly
# one of the unset cloud-image defaults: vultr.guest, localhost or
# localhost.localdomain. Any other name, short ones included, is a deliberate
# name (a laptop called "yoga") and is left alone with a one-line hint.
# LENS_SET_HOSTNAME=1 forces the rename whatever the current name;
# LENS_SET_HOSTNAME=0 disables the step. Skipped when hostnamectl is missing
# or the target is not a dotted FQDN.
#
# When it renames: hostnamectl set-hostname; when /etc/cloud exists,
# cloud.cfg.d/99-preserve-hostname.cfg so cloud-init does not reset the name
# from Vultr metadata; and /etc/hosts, where the 127.0.0.1 and ::1 lines
# naming the old hostname become "<addr> <fqdn> <short>" (a line that also
# names localhost is never rewritten) and, when no line names the FQDN, the
# two loopback lines are appended. Idempotent, honours DRY_RUN=1, never fails
# the caller. Uses the caller's info/ok/warn. The paths are overridable for
# the tests only.
LENS_HOSTS_FILE="${LENS_HOSTS_FILE:-/etc/hosts}"
LENS_CLOUD_DIR="${LENS_CLOUD_DIR:-/etc/cloud}"
LENS_PRESERVE_HOSTNAME_CFG='# The hostname is set by hand; do not let cloud-init reset it from Vultr metadata.
preserve_hostname: true
manage_etc_hosts: false'

_lens_hn_do() {
  if [[ "${DRY_RUN:-0}" == "1" ]]; then info "[dry-run] $*"; else "$@"; fi
}

# lens_hostname_is_default NAME — true only for an unset cloud-image default.
lens_hostname_is_default() {
  case "$1" in
    vultr.guest | localhost | localhost.localdomain) return 0 ;;
    *) return 1 ;;
  esac
}

# lens_hosts_rewrite FILE OLD FQDN — print FILE with the 127.0.0.1/::1 lines
# naming OLD (and not localhost) replaced by "<addr> FQDN <short>", plus both
# loopback lines appended when no line names FQDN. An empty OLD replaces
# nothing. Read-only.
lens_hosts_rewrite() {
  awk -v old="$2" -v fqdn="$3" -v short="${3%%.*}" '
    function names(n,   i) {
      for (i = 2; i <= NF; i++) { if ($i ~ /^#/) break; if ($i == n) return 1 }
      return 0
    }
    ($1 == "127.0.0.1" || $1 == "::1") && old != "" && names(old) && !names("localhost") {
      print $1 " " fqdn " " short; seen = 1; next
    }
    { if ($1 !~ /^#/ && names(fqdn)) seen = 1; print }
    END { if (!seen) { print "127.0.0.1 " fqdn " " short; print "::1 " fqdn " " short } }
  ' "$1"
}

# lens_set_hostname FQDN
lens_set_hostname() {
  local fqdn="${1,,}" current old="" cfg hosts
  if [[ "${LENS_SET_HOSTNAME:-1}" == "0" ]]; then
    info "LENS_SET_HOSTNAME=0 — leaving the hostname alone"
    return 0
  fi
  command -v hostnamectl >/dev/null 2>&1 || return 0
  if [[ ! "$fqdn" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ \
    || "$fqdn" =~ ^[0-9.]+$ ]]; then
    info "hostname: '${1}' is not a dotted FQDN — leaving the hostname alone"
    return 0
  fi
  current="$(hostnamectl --static 2>/dev/null || true)"
  if [[ "$current" == "$fqdn" ]]; then
    ok "hostname already ${fqdn}"
  elif [[ "${LENS_SET_HOSTNAME:-}" != "1" ]] && ! lens_hostname_is_default "$current"; then
    info "hostname is ${current:-<unset>}, not ${fqdn} — left as is; to rename: hostnamectl set-hostname ${fqdn} (or LENS_SET_HOSTNAME=1)"
    return 0
  elif _lens_hn_do hostnamectl set-hostname "$fqdn"; then
    old="$current"
    [[ "${DRY_RUN:-0}" == "1" ]] || ok "hostname ${current:-<unset>} → ${fqdn}"
  else
    warn "could not set the hostname — run: hostnamectl set-hostname ${fqdn}"
    return 0
  fi

  if [[ -d "$LENS_CLOUD_DIR" ]]; then
    cfg="$LENS_CLOUD_DIR/cloud.cfg.d/99-preserve-hostname.cfg"
    if [[ "$(cat "$cfg" 2>/dev/null || true)" != "$LENS_PRESERVE_HOSTNAME_CFG" ]]; then
      if [[ "${DRY_RUN:-0}" == "1" ]]; then
        info "[dry-run] would write ${cfg} (preserve_hostname: true, manage_etc_hosts: false)"
      elif mkdir -p "${cfg%/*}" && printf '%s\n' "$LENS_PRESERVE_HOSTNAME_CFG" > "$cfg"; then
        ok "cloud-init keeps the hostname (${cfg})"
      else
        warn "could not write ${cfg} — cloud-init may reset the hostname on reboot"
      fi
    fi
  fi

  [[ -f "$LENS_HOSTS_FILE" ]] || return 0
  if ! hosts="$(lens_hosts_rewrite "$LENS_HOSTS_FILE" "$old" "$fqdn")"; then
    warn "could not read ${LENS_HOSTS_FILE} — add: 127.0.0.1 ${fqdn} ${fqdn%%.*}"
    return 0
  fi
  [[ "$hosts" != "$(cat "$LENS_HOSTS_FILE")" ]] || return 0
  if [[ "${DRY_RUN:-0}" == "1" ]]; then
    info "[dry-run] would point ${LENS_HOSTS_FILE}'s loopback lines at ${fqdn}"
  elif printf '%s\n' "$hosts" > "$LENS_HOSTS_FILE"; then
    ok "${LENS_HOSTS_FILE} names ${fqdn} on 127.0.0.1 and ::1"
  else
    warn "could not update ${LENS_HOSTS_FILE} — add: 127.0.0.1 ${fqdn} ${fqdn%%.*}"
  fi
  return 0
}
