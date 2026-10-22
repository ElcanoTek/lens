# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
#
# Lens build, test and lint targets. Every build path funnels through here:
# scripts/bootstrap.sh and scripts/update.sh run `make build`, and CI runs the
# same targets, so a fix to how Lens is built lands in one place.

.PHONY: all build compile test test-race test-cover vet lint fmt fmt-check govulncheck shellcheck shellcheck-install stack-check version ci clean help

# GOTOOLCHAIN=auto: the operator does NOT have to hand-install the Go release
# go.mod pins. That pin moves with every Go security release (govulncheck
# reports stdlib CVEs against whatever toolchain built the binary), distro Go
# lags it by days to weeks, and Fedora's `golang` ships GOTOOLCHAIN=local in
# its go.env, which turns the lag into a hard failure ("go.mod requires
# go >= 1.27.2 (running go 1.26.x; GOTOOLCHAIN=local)") instead of the
# download Go would otherwise do. The environment variable outranks go.env.
#
# `?=` so a deliberate choice (an air-gapped box pinned to `local`, or a
# specific `go1.27.2`) is kept.
GOTOOLCHAIN ?= auto
export GOTOOLCHAIN

GO ?= go
BIN := bin/lens

# Build identity, stamped into internal/version via -ldflags -X. Immediate
# (`:=`) so git runs once per make invocation. update.sh builds in a staging
# copy that has no .git, so it passes VERSION and COMMIT on the command line
# (command-line variables override these assignments).
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT := $(shell git rev-parse HEAD 2>/dev/null)
VERSION_PKG := github.com/ElcanoTek/lens/internal/version
LDFLAGS := -s -w -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT)

# The shell scripts ShellCheck lints (CI's shell job runs this target): the
# deploy scripts, the shared frontend-standard tooling and the cloud-session hook.
SHELL_SCRIPTS := install.sh scripts/bootstrap.sh scripts/update.sh scripts/host.sh \
	scripts/bsl-change-date.sh scripts/lib/deploy.sh scripts/lib/caddy-site.sh scripts/lib/hostname.sh \
	scripts/flag-sync.sh scripts/stack-check.sh .claude/hooks/session-start.sh

all: build

help:
	@echo "lens — build/test/lint targets"
	@echo "  make build        emit $(BIN) (stamped with VERSION/COMMIT from git)"
	@echo "  make compile      go build ./... (compile-check every package; no artifact)"
	@echo "  make test         go test ./..."
	@echo "  make test-race    go test -race ./..."
	@echo "  make test-cover   go test with coverage.out"
	@echo "  make vet          go vet ./..."
	@echo "  make lint         golangci-lint run (.golangci.yml)"
	@echo "  make fmt          gofmt -w the tree"
	@echo "  make fmt-check    fail when any file needs gofmt"
	@echo "  make govulncheck  known-vulnerability scan (CI's go job)"
	@echo "  make shellcheck   bash -n + shellcheck the deploy scripts"
	@echo "  make shellcheck-install  pinned ShellCheck release into TOOLS_BIN (CI, cloud hook)"
	@echo "  make stack-check  frontend standard: AGENTS.md block, flag.lock, no CDNs"
	@echo "  make version      print what a build from this tree calls itself"
	@echo "  make ci           everything CI's go + shell jobs run"

# -trimpath keeps the builder's paths (/var/cache/lens-build, a runner's
# workspace) out of the binary. CGO_ENABLED=0: the SQLite driver is pure Go,
# so the binary is static and survives a distro libc upgrade untouched.
build:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/lens

compile:
	$(GO) build ./...

version:
	@echo "$(VERSION) ($(COMMIT))"

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

test-cover:
	$(GO) test -coverprofile=coverage.out -covermode=atomic ./...
	@$(GO) tool cover -func=coverage.out | tail -1

vet:
	$(GO) vet ./...

lint:
	golangci-lint run

fmt:
	gofmt -w $$(git ls-files '*.go' 2>/dev/null || find . -name '*.go' -not -path './.git/*')

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# The scanner stays @latest on purpose (upstream's documented invocation): its
# advisory database is live anyway, and a pin nobody bumps silently loses
# reachability-analysis improvements.
#
# The scanner must be built with at least go.mod's Go: one built with an
# older toolchain refuses every package ("package requires newer Go version").
# Under GOTOOLCHAIN=auto, `go run pkg@latest` picks the newer of the local Go
# and the scanner's own minimum — not go.mod's — so a box whose Go is older
# than the pin (a cloud session, a distro Go) failed here while CI passed.
# `<go.mod version>+auto` sets that floor; an explicit GOTOOLCHAIN is kept.
GO_MOD_TOOLCHAIN := go$(shell sed -n 's/^go //p' go.mod)
govulncheck:
	GOTOOLCHAIN=$(if $(filter auto,$(GOTOOLCHAIN)),$(GO_MOD_TOOLCHAIN)+auto,$(GOTOOLCHAIN)) \
		$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

shellcheck:
	@for s in $(SHELL_SCRIPTS); do bash -n "$$s" || exit 1; done
	shellcheck -x -S warning $(SHELL_SCRIPTS)

# The ShellCheck release CI and the cloud-session hook install, so both lint
# with the same rules (distro packages lag and differ). Bump the version and
# the release tarball's sha256 together. Idempotent: a matching binary in
# TOOLS_BIN is left alone. Linux x86_64 only (CI runners, cloud sessions).
SHELLCHECK_VERSION := 0.11.0
SHELLCHECK_SHA256 := 8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198
TOOLS_BIN ?= $(HOME)/.local/bin
shellcheck-install:
	@set -eu; \
	if "$(TOOLS_BIN)/shellcheck" --version 2>/dev/null | grep -qx 'version: $(SHELLCHECK_VERSION)'; then \
		echo "shellcheck $(SHELLCHECK_VERSION) already in $(TOOLS_BIN)"; exit 0; fi; \
	[ "$$(uname -sm)" = "Linux x86_64" ] || { echo "shellcheck-install: Linux x86_64 only" >&2; exit 1; }; \
	tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	tarball="shellcheck-v$(SHELLCHECK_VERSION).linux.x86_64.tar.xz"; \
	curl -fsSL -o "$$tmp/$$tarball" "https://github.com/koalaman/shellcheck/releases/download/v$(SHELLCHECK_VERSION)/$$tarball"; \
	echo "$(SHELLCHECK_SHA256)  $$tmp/$$tarball" | sha256sum -c --quiet -; \
	tar -xJf "$$tmp/$$tarball" -C "$$tmp"; \
	mkdir -p "$(TOOLS_BIN)"; \
	install -m 0755 "$$tmp/shellcheck-v$(SHELLCHECK_VERSION)/shellcheck" "$(TOOLS_BIN)/shellcheck"; \
	"$(TOOLS_BIN)/shellcheck" --version | grep -qx 'version: $(SHELLCHECK_VERSION)'; \
	echo "installed shellcheck $(SHELLCHECK_VERSION) into $(TOOLS_BIN)"

# The Elcano frontend standard (AGENTS.md): the shared block is intact,
# static/design-system matches flag.lock, and nothing loads from a CDN.
# Offline; no flag checkout needed. scripts/flag-sync.sh and
# scripts/stack-check.sh are shared verbatim across Elcano repos.
stack-check:
	scripts/stack-check.sh

ci: stack-check fmt-check build vet lint test-race govulncheck shellcheck

clean:
	rm -rf bin coverage.out
