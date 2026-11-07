#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# scripts/release-newer-published.sh TAG — print the release tags NEWER than
# TAG that are successfully PUBLISHED: a non-draft GitHub release that carries
# SHA256SUMS (what a complete publish uploads, and what a box verifies first).
# Newest last; nothing printed means TAG is the newest published release.
set -euo pipefail

[[ $# -eq 1 ]] || { echo "usage: $0 TAG" >&2; exit 2; }
tag="$1"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[[ -n "$(printf '%s\n' "$tag" | "$here/version.sh" filter)" ]] || { echo "not a release tag: $tag" >&2; exit 2; }

if ! listed="$(gh release list --limit 200 --exclude-drafts --json tagName --jq '.[].tagName')"; then
  echo "could not list the releases" >&2
  exit 2
fi
newer="$(printf '%s\n%s\n' "$listed" "$tag" | awk 'NF && !seen[$0]++' | "$here/version.sh" filter \
  | awk -v t="$tag" 'found { print } $0 == t { found = 1 }')"
[[ -n "$newer" ]] || exit 0
while IFS= read -r t; do
  if ! complete="$(gh release view "$t" --json isDraft,assets --jq '(.isDraft | not) and ((["SHA256SUMS","SHA256SUMS.sigstore.json","release.json","lens-linux-amd64","lens-linux-amd64.sigstore.json","lens-linux-arm64","lens-linux-arm64.sigstore.json"] - [.assets[].name]) | length == 0)')"; then
    echo "could not read release $t" >&2
    exit 2
  fi
  [[ "$complete" == "true" ]] && printf '%s\n' "$t"
done <<<"$newer"
exit 0
