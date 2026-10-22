# Contributing to Lens

Thanks for looking. Bug reports, small fixes and well-scoped features are all
welcome.

Before anything else, read [`AGENTS.md`](AGENTS.md). It is the architecture map
— package responsibilities, the auto-mode ladder, and the non-obvious gotchas
that will otherwise cost you an afternoon (the CTV news-routing rule, why the
direct pass reads the body to EOF, why the deep pass is single-session).

## Licensing of contributions

Lens is source-available under the **Business Source License 1.1** with
ElcanoTek, Inc. as Licensor. **Contributions are accepted under those same
terms.** Opening a pull request means you have the right to contribute the code
and you agree to it being released under BSL 1.1, converting to MIT on the
Change Date like the rest of the file. There is no separate CLA to sign.

Every new first-party source file starts with:

```go
// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
```

Before the package doc comment, separated from it by a blank line. Shell
scripts and the Makefile use the same two lines as `#` comments, after any
shebang. See [`docs/LICENSING.md`](docs/LICENSING.md) for what the licence does
and does not allow — note that it is non-production-only, so please don't open
a PR that assumes otherwise.

## Setup

Go, version 1.21 or newer, and `make`. `go.mod` pins the exact toolchain
(currently 1.27.3); the Makefile exports `GOTOOLCHAIN=auto`, so an older local
Go downloads the pinned release on first use instead of failing. CI builds with
the pinned release, and the deploy-build job also proves an older distro Go
(1.24) still gets there.

```bash
git clone https://github.com/ElcanoTek/lens.git
cd lens
make build                # → bin/lens, stamped with git describe / HEAD
./bin/lens --version
```

You do **not** need an OpenRouter key to run the tests. You do need one to run
the pipeline for real:

```bash
cp .env.example .env      # then fill in OPENROUTER_API_KEY
./bin/lens run --input-csv test-input/url_master.csv --scrape-mode direct
```

Remember that real runs cost money. `test-input/` has small synthetic fixtures
for cheap smoke tests.

## Running the tests

```bash
make test                                   # go test ./..., ~15s
make test-race                              # what CI runs; the gate for the job queue and cost meters
go test ./internal/detect/                  # one package
go test ./internal/openrouter/ -run CTVNewsRouting -v
make test-cover                             # coverage.out plus a total
```

Two things to know about the suite:

- It is **hermetic**. No API key, no network: OpenRouter, the stores, the
  WebDriver endpoint and Auth are `httptest` servers, and tests that touch
  auth mint their own throwaway Ed25519 keypair. Nothing reads a key at
  package load — `config.Load` never fails for a missing
  `OPENROUTER_API_KEY`; only the commands that need it call `RequireAPIKey`.
- It **never touches real paths**. Every test that writes builds its tree
  under `t.TempDir()`, hands it over explicitly (`web.Options{Home: …}`,
  `jobs.NewManager(home)`) or with `t.Setenv("LENS_HOME", …)` / `t.Chdir`, so
  it cannot write into a developer's `managed-files/`, `.env` or
  `config.json`. Keep new tests that way; don't read the working directory.

`internal/deploytest` runs the bash deploy scripts end to end against a scratch
tree with stub tools on `PATH` (needs `bash`, `rsync`, `jq`; skips without
them). One test — the Python-era `update.sh` re-execing into the Go one — only
runs as root; CI runs it separately under `sudo`.

## Linting and checks

```bash
make fmt          # gofmt -w the tree
make fmt-check    # fail when any file needs gofmt
make vet          # go vet ./...
make lint         # golangci-lint run (.golangci.yml)
make govulncheck  # known vulnerabilities reachable from Lens
make shellcheck   # bash -n + shellcheck the deploy scripts
make stack-check  # Elcano frontend standard: AGENTS.md block, flag.lock, no CDNs
make ci           # everything CI's go + shell jobs run
```

CI **enforces** all of them, so a finding fails the build.

`make lint` needs **golangci-lint v2.14.0**, the version CI pins. Install that
release, built with Go 1.27 or newer (golangci-lint refuses to run when the Go
it was built with is older than the one `go.mod` targets, and go1.27.2 changed
the export-data format v2.13.x cannot read). From source, for example:

```bash
GOTOOLCHAIN=auto go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
```

Bumping `go.mod`'s Go line can therefore mean bumping the pin in
`.github/workflows/tests.yml` too — see the comment in `.golangci.yml`. The lint
is a full gate (`only-new-issues: false`): fix a finding or silence it with
`//nolint:<linter> // <reason>`; `nolintlint` rejects a suppression with no
reason or one that no longer suppresses anything.

`govulncheck` runs against a live advisory database, so it can turn red with no
commit involved — usually a Go security release the `go` line in `go.mod`
needs to move to.

Keep unrelated refactors out of functional PRs. The test suite is the gate on
behaviour; gofmt and golangci-lint are the gate on style.

## Dependencies

There are three direct Go modules (`golang.org/x/net`, `golang.org/x/text`,
`modernc.org/sqlite`); `go.mod` and `go.sum` pin them and everything they pull
in, so every build — CI, a laptop, a production deploy — compiles the same
code. To change one, `go get module@version`, then `go mod tidy`, and commit
both files. Never hand-edit `go.sum`.

Dependabot proposes updates (daily, cooldown 7 days (3 for patch releases),
minor/patch grouped); a human merges one only when the bump earns it (a
security fix whose code path Lens uses, or an upstream commit we need). Before
adding a dependency, check whether the standard library or a small package of
our own would do — see "Dependencies and in-housed code" in
[`AGENTS.md`](AGENTS.md) for the policy and what is already in-house.

Test-only and tooling code (golangci-lint, govulncheck) is never a module
dependency. Production builds nothing but `bin/lens`; the binary carries its
assets (templates, static files, taxonomy, TLD list), so nothing else of the
tree is needed at run time.

## Branches and pull requests

Work on a branch off `main` and open a PR — `main` is protected and CI runs on
pull requests, alongside CodeQL (`.github/workflows/codeql.yml`), which also
scans weekly. The issue and pull-request templates under `.github/` ask for
exactly what a reviewer needs. The convention in this repo's history is a
`<type>/<short-description>` slug:

| Prefix | For |
|---|---|
| `feat/` | New capability |
| `fix/` | Bug fix |
| `ci/` | Workflow and pipeline changes |
| `chore/` | Dependencies, housekeeping, model-pin bumps |
| `docs/` | Documentation only |

Commit subjects are imperative and describe the *effect*, not the mechanism —
"Skip the deep crawler for sites the direct pass proved are blocked"
rather than "update internal/scrape". Conventional-commit prefixes
(`chore(ci):`, `fix:`) appear in the history too and are fine.

A good PR:

- Does one thing.
- Adds or updates tests for behaviour it changes. This codebase is mocked
  heavily precisely so that scraper and LLM behaviour is testable offline.
- Updates the docs it invalidates — `AGENTS.md` for architecture and gotchas,
  `docs/DEPLOYMENT.md` for anything operational, `config.json.example` and
  `.env.example` for any new setting (the examples are meant to document
  *every* key the code reads).
- Leaves `make stack-check fmt-check vet lint test-race` green (and `make shellcheck`
  when it touches the deploy scripts).

### Things that need extra care

- **`internal/config` defaults** — a new key belongs in `config.json.example`
  too, with a comment; `internal/config/config_test.go` fails when the example
  and `Default()` disagree.
- **Scrape-ladder behaviour** — read the anti-bot section of `AGENTS.md`
  first. "Try harder" changes (more retries, stealth escalation) increase ban
  risk without helping, and have been deliberately removed before.
- **`deploy/` and `scripts/`** — these run as root on real hosts. Keep them
  re-runnable, and update `docs/DEPLOYMENT.md` in the same PR.
- **`static/design-system/`** — vendored from the Flag design system and
  locked by hash in `flag.lock`. Never edit it in place; update it with
  `scripts/flag-sync.sh sync` against a flag checkout (see "Frontend" in
  [`AGENTS.md`](AGENTS.md)). Product styling goes in `static/styles.css`.
- **`content_taxonomy.tsv`** — third-party reference data (IAB Tech Lab).
  Don't hand-edit rows; replace the file from upstream.
- **Never commit** `config.json`, `output.csv`, `progress.json`, `.env`,
  `managed-files/`, or any real inventory list. All are git-ignored; keep it
  that way. Fixtures in `test-input/` must stay synthetic — see
  [`test-input/README.md`](test-input/README.md).

## Reporting bugs

Open an issue with the failing input shape (a couple of synthetic rows, not
your real list), the command or dashboard action, and the relevant log lines.
Scrub API keys and anything commercially sensitive first.

For anything security-related, do **not** open an issue — see
[`SECURITY.md`](SECURITY.md).

## Code of conduct

Participation is covered by [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).
