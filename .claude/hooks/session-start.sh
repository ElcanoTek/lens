#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
#
# SessionStart hook for Claude Code cloud sessions: make `make test`,
# `make lint`, `make shellcheck` and `make stack-check` runnable. Synchronous,
# idempotent and non-interactive; a no-op outside the cloud.
set -euo pipefail

[ "${CLAUDE_CODE_REMOTE:-}" = "true" ] || exit 0

cd "${CLAUDE_PROJECT_DIR:-$(cd "$(dirname "$0")/../.." && pwd)}"

# go.mod pins a newer toolchain than the image may carry; let Go fetch it
# (the Makefile exports the same), and fetch the modules once.
export GOTOOLCHAIN=auto
go mod download
go build ./... >/dev/null

# golangci-lint at the version CI pins (.github/workflows/tests.yml), built
# with go.mod's own Go: a golangci-lint built with an older Go cannot read
# go1.27.x export data (see .golangci.yml), and the module alone would let
# `go install` pick an older toolchain.
want=2.14.0
toolchain="$(sed -n 's/^toolchain //p' go.mod)"
toolchain="${toolchain:-go$(sed -n 's/^go //p' go.mod)}"
gobin="$(go env GOPATH)/bin"
if ! "$gobin/golangci-lint" version 2>/dev/null | grep -q "version $want built with $toolchain "; then
  GOTOOLCHAIN="$toolchain" go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$want"
fi
if [ -n "${CLAUDE_ENV_FILE:-}" ] && ! grep -qsF "$gobin" "$CLAUDE_ENV_FILE"; then
  echo "export PATH=\"$gobin:\$PATH\"" >> "$CLAUDE_ENV_FILE"
fi

# ShellCheck at the release CI pins (Makefile SHELLCHECK_VERSION, sha256
# verified). Under set -e a failed download or checksum fails the hook rather
# than leaving `make shellcheck` to break later.
toolsbin="$HOME/.local/bin"
make -s shellcheck-install TOOLS_BIN="$toolsbin"
if [ -n "${CLAUDE_ENV_FILE:-}" ] && ! grep -qsF "$toolsbin" "$CLAUDE_ENV_FILE"; then
  echo "export PATH=\"$toolsbin:\$PATH\"" >> "$CLAUDE_ENV_FILE"
fi
