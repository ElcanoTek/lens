#!/usr/bin/env bash
# stack-check.sh — enforce the Elcano frontend standard (see the
# "Elcano frontend standard" block in AGENTS.md). Shared verbatim by every
# Elcano repo; change it in all of them at once (bump STACK_CHECK_VERSION).
#
# Reads stack.conf at the repo root:
#   tier=A|B                 A: Go templates + plain JS; B: React/Vite SPA embedded in Go
#   web=<dir> [<dir> ...]    directories holding served HTML/CSS/JS (scanned for CDN use)
#   npm=<dir> [<dir> ...]    directories holding a package.json ("." for the root)
#   backend=go|<other>       anything but go must be explained in AGENTS.md
#
# Checks:
#   1. AGENTS.md carries the shared standard block, byte-identical (hash pinned below).
#   2. flag.lock is honest (scripts/flag-sync.sh check).
#   3. No served file loads a script, stylesheet or font from another host.
#   4. npm trees: no server frameworks (next, express, ...); Tier A has no runtime deps.
set -euo pipefail

STACK_CHECK_VERSION=2
STANDARD_SHA=686c0e22ef25ce4783a4d7da70c43ac80899f529cddebdf6b6efc7bd91bd75d8
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONF="$ROOT/stack.conf"
fail=0
err() { echo "stack-check: $*" >&2; fail=1; }

sha() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum | cut -d' ' -f1
  else shasum -a 256 | cut -d' ' -f1; fi
}
conf() { sed -n "s/^$1=//p" "$CONF" | sed 's/[[:space:]]*#.*$//' | tail -1; }

[ -f "$CONF" ] || { echo "stack-check: missing stack.conf" >&2; exit 2; }
tier="$(conf tier)"; web="$(conf web)"; npm="$(conf npm)"; backend="$(conf backend)"
case "$tier" in A|B) ;; *) err "stack.conf: tier must be A or B (got '$tier')";; esac

# 1. The standard block.
if [ ! -f "$ROOT/AGENTS.md" ]; then err "missing AGENTS.md"
else
  got="$(awk '/^<!-- BEGIN:elcano-frontend-standard/{f=1;next}/^<!-- END:elcano-frontend-standard/{f=0}f' "$ROOT/AGENTS.md" | sha)"
  [ "$got" = "$STANDARD_SHA" ] || err "AGENTS.md: the elcano-frontend-standard block is missing or edited (sha $got). It is shared verbatim across repos; change it everywhere and bump STANDARD_SHA in every scripts/stack-check.sh."
  # A non-Go backend needs its own AGENTS.md heading naming the exception and
  # the backend (the shared block mentions exceptions, so it does not count).
  if [ "${backend:-go}" != go ] && ! awk '/^<!-- BEGIN:elcano-frontend-standard/{f=1} !f{print} /^<!-- END:elcano-frontend-standard/{f=0}' "$ROOT/AGENTS.md" \
       | grep -iE '^#{1,6} .*exception' | grep -qi "$backend"; then
    err "stack.conf: backend=$backend needs an AGENTS.md heading like '## Exception: $backend backend' explaining why"
  fi
fi

# 2. Vendored Flag files.
if [ -f "$ROOT/flag.lock" ]; then
  "$ROOT/scripts/flag-sync.sh" check || fail=1
else
  err "missing flag.lock (vendor Flag with scripts/flag-sync.sh add)"
fi

# 3. Third-party hosts in served files. Links (<a href>, rel=canonical) are
# fine; loading a script, stylesheet, font, icon or import from elsewhere is not.
host='[[:space:]]*["'"'"'(]?[[:space:]]*(https?:)?//'
for d in $web; do
  [ -d "$ROOT/$d" ] || { err "stack.conf: web dir $d does not exist"; continue; }
  while IFS= read -r hit; do
    err "remote asset in served file: ${hit#"$ROOT"/}"
  done < <(grep -rEnoi --include='*.html' --include='*.tmpl' --include='*.gohtml' --include='*.css' --include='*.js' --include='*.mjs' --include='*.tsx' --include='*.ts' --include='*.jinja' --include='*.j2' \
           --exclude-dir=node_modules --exclude-dir=dist --exclude-dir=test --exclude-dir=tests --exclude-dir=e2e --exclude-dir=testdata \
           "<script[^>]*[[:space:]]src=$host|<link[^>]*>|@import$host|url\($host|\bimport[[:space:]]*\($host|\bfrom$host" "$ROOT/$d" \
         | grep -Ei "<script|@import|url\(|\bimport[[:space:]]*\(|\bfrom[[:space:]]*[\"']|rel=[\"']?(stylesheet|preload|modulepreload|icon|shortcut icon|apple-touch-icon|manifest)" \
         | grep -Ei "(src|href)=[\"']?(https?:)?//|@import|url\(|\bimport[[:space:]]*\(|\bfrom[[:space:]]*[\"']" \
         | grep -Ei "(https?:)?//" \
         | grep -viE '//(localhost|127\.0\.0\.1)' || true)
done

# 4. npm trees.
servers='"(next|express|koa|fastify|@hapi/hapi|@remix-run/[a-z-]+|nuxt|@sveltejs/kit|astro|hono)"[[:space:]]*:'
for d in $npm; do
  pj="$ROOT/$d/package.json"
  [ -f "$pj" ] || { err "stack.conf: no package.json in $d"; continue; }
  if grep -Eq "$servers" "$pj"; then err "$d/package.json: depends on a JavaScript server framework; the server is Go"; fi
  if [ "$tier" = A ] && awk '/"dependencies"[[:space:]]*:/{f=1} f&&/}/{print; exit} f' "$pj" | grep -Eq '"[^"]+"[[:space:]]*:[[:space:]]*"'; then
    err "$d/package.json: Tier A allows devDependencies only (browser tests), no runtime dependencies"
  fi
done

if [ "$fail" -ne 0 ]; then echo "stack-check: FAILED (frontend standard v1, tier $tier)" >&2; exit 1; fi
echo "stack-check v$STACK_CHECK_VERSION: ok (frontend standard v1, tier $tier, backend ${backend:-go})"
