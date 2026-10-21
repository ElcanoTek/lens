# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
#
# Lens build, test and lint targets. Every build path funnels through here:
# scripts/bootstrap.sh and scripts/update.sh run `make build`, and CI runs the
# same targets, so a fix to how Lens is built lands in one place.

.PHONY: all build compile test test-race test-cover vet lint fmt fmt-check govulncheck shellcheck version ci clean help

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

# The deploy scripts ShellCheck lints (CI's shell job runs this target).
SHELL_SCRIPTS := install.sh scripts/bootstrap.sh scripts/update.sh scripts/host.sh \
	scripts/lib/deploy.sh scripts/lib/caddy-site.sh scripts/lib/hostname.sh

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
govulncheck:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

shellcheck:
	@for s in $(SHELL_SCRIPTS); do bash -n "$$s" || exit 1; done
	shellcheck -x -S warning $(SHELL_SCRIPTS)

ci: fmt-check build vet lint test-race govulncheck shellcheck

clean:
	rm -rf bin coverage.out
