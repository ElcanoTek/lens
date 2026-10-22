# AGENTS.md — Elcano Lens

Orientation for anyone (human or agent) making changes here. Companion docs: [`CONTRIBUTING.md`](CONTRIBUTING.md) for workflow, [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) for operations, and [`docs/LICENSING.md`](docs/LICENSING.md) for what the BSL 1.1 licence allows. New source files carry an SPDX header (`#` comments in shell scripts and the Makefile):

```go
// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
```

<!-- BEGIN:elcano-frontend-standard v2 (shared verbatim by every Elcano repo; scripts/stack-check.sh pins its hash; change it everywhere or nowhere) -->
## Elcano frontend standard (v2)

Every Elcano web product follows one of two tiers. `stack.conf` at the repo
root declares which, and `scripts/stack-check.sh` (run by CI) enforces it.

**The server is Go and serves everything.** The browser talks to one origin:
the Go binary serves the HTML, the static assets and the JSON API, and it owns
sign-in (the `elcano_auth` cookie from the auth service), sessions, CSRF and
security headers. There is no Node, Python or other JavaScript server in
production. Static files ship inside the binary with `go:embed`; Caddy only
terminates TLS and reverse-proxies to Go.

**Tier A — server-rendered (the default).** Go `html/template` pages, plain
CSS that reads Flag tokens, and small plain-JavaScript modules (no framework,
no bundler, no transpiler, no npm runtime dependencies). npm may appear only
as a dev dependency for browser tests (Playwright, axe). Pick Tier A unless the
page is a long-lived, state-heavy application. Components use Flag's CSS
component classes, which mirror the names and states of Flag's shadcn/ui
components so both tiers look and behave alike.

**Tier B — interactive app.** React + TypeScript built by Vite into a static
bundle that the Go server embeds and serves (same origin, so no CORS). No
server-side rendering, no Next.js, no runtime server code in TypeScript; data
comes from the Go JSON API. Base UI components come from Flag's shadcn/ui
registry (React + Radix + Tailwind, themed by Flag tokens; registry URL in
Flag's README): `npx shadcn add` copies them into the app, where they live and
are reviewed like our own code. Build a component by hand only when the
registry lacks it, then upstream it to Flag. Keep other runtime npm
dependencies few and justified in AGENTS.md. Use Tier B only when Tier A would
mean re-implementing a client-side state framework by hand (streaming chat,
multi-step builders).

**Both tiers:**

- Design comes from Flag (`ElcanoTek/flag`). Vendored Flag files are listed in
  `flag.lock` with their hashes; `scripts/flag-sync.sh check` fails CI on any
  hand edit. Update with `scripts/flag-sync.sh sync`, never by editing in place.
  Product styling lives outside the vendored directories and reads tokens.
- No third-party CDNs: every script, stylesheet and font is served by our own
  binary. Fonts are Flag's Nebula Sans and Hack only.
- A strict Content-Security-Policy (no `unsafe-inline` scripts, no remote
  script hosts), `X-Content-Type-Options: nosniff`, and `frame-ancestors`.
- Hashed or versioned asset URLs with long-lived caching; HTML is `no-cache`.
- Accessibility is tested (Playwright + axe where a browser suite exists).
- Exceptions (a non-Go backend, an extra runtime dependency) are written down
  in this repo's AGENTS.md with the reason, not left implicit.
<!-- END:elcano-frontend-standard -->

## Project Overview

Elcano Lens is a concurrent content analysis pipeline that classifies **websites**, **iOS apps**, **Android apps**, and **CTV (Connected TV) apps** into advertising quality tiers using LLM classification via OpenRouter. It outputs CSV with IAB content taxonomy categories, quality ratings, and metadata.

It is one Go binary, `lens` (`./cmd/lens`, built with `make build` → `bin/lens`), with two runtime modes sharing the same pipeline: the **CLI** (`lens run`) and the **web dashboard** (`lens serve`), plus the operator commands (`lens update`, `lens doctor`, `lens token`, …). The release before this one was Python; on-disk formats are unchanged across the rewrite (see [Gotchas](#on-disk-formats-are-the-python-releases)).

## Essential Commands

```bash
make build                    # bin/lens, version-stamped from git
make test                     # go test ./...  (make test-race is what CI runs)
make lint                     # golangci-lint v2.14.0
make stack-check              # frontend standard: AGENTS.md block, flag.lock, no CDNs (offline)
make ci                       # stack-check fmt-check build vet lint test-race govulncheck shellcheck

# Vendored Flag design system (needs a flag checkout; ../flag by default)
scripts/flag-sync.sh status   # is static/design-system behind flag?
scripts/flag-sync.sh sync     # re-copy every locked file from flag, rewrite flag.lock

# Run website analysis (auto mode, input.csv → output.csv)
./bin/lens run

# Direct HTTP scraping only (no browser), quiet
./bin/lens run --scrape-mode direct --quiet

# CTV workflow
./bin/lens run --ctv

# Deep scrape only (headless Chrome in a Podman container)
./bin/lens run --deep-scrape

# JSONL logging and custom I/O
./bin/lens run --jsonl run.jsonl --input-csv my_input.csv --output-csv my_output.csv

# One package's tests
go test ./internal/detect/

# Web service
./bin/lens serve --addr 127.0.0.1:8808

# Management utility
./bin/lens manage setup          # Scaffold .env and config.json
./bin/lens manage validate       # Validate config
./bin/lens manage progress       # Show progress summary
./bin/lens manage reset          # Reset all progress
./bin/lens manage reset-failed   # Reset only failed items
./bin/lens manage stats          # Show statistics

./bin/lens --help                # every command
```

## Architecture and Data Flow

```
Input CSV/xlsx
    │  internal/tabular (CSV + in-house XLSX reader → string table)
    ▼
internal/pipeline (Orchestrator)
    │
    ├─ internal/detect → ContentType (WEBSITE, IOS_APP, ANDROID_APP, CTV_APP) per row
    │
    ├─ WEBSITE path:
    │   internal/scrape (direct, then deep) → pipeline/domain.go → internal/openrouter (ClassifyWebsite)
    │   └─ still failed → openrouter.ResearchWebsite → classify the research
    │
    ├─ IOS_APP path:
    │   internal/stores (iTunes Search API) → pipeline/app.go → internal/openrouter (classify app)
    │
    ├─ ANDROID_APP path:
    │   internal/stores (Play Store page, internal/htmldom) → pipeline/app.go → internal/openrouter
    │
    └─ CTV_APP path (two-step pipeline):
        openrouter.ResearchCTVApp (web-search model)
        → classify the research (fast model) → news routing
        → pipeline/ctv.go (post-processing)
    │
    ▼
Output CSV (appended) + progress.json (saved per item, resumable)
```

The web service (`lens serve`) runs each job as a `lens run` subprocess of its own executable (`internal/jobs`) — it does **not** call the pipeline in-process.

## Code Organization

| Path | Responsibility |
|---|---|
| `cmd/lens` | The single binary: dispatches `run`, `serve`, `manage`, `token`, `access`, `submit`, `migrate-access-db` and the operator commands |
| `assets.go` (package `lens`) | `go:embed` of `static/`, `templates/`, `content_taxonomy.tsv` and `iana_tlds.txt` — the binary carries all of them |
| `static/`, `templates/` | The dashboard frontend (Tier A): product CSS/JS at the top of `static/`, vendored Flag files in `static/design-system/` (see `flag.lock`) |
| `scripts/flag-sync.sh`, `scripts/stack-check.sh`, `stack.conf`, `flag.lock` | The Elcano frontend standard's shared tooling and this repo's declaration; see [Frontend](#frontend-tier-a-and-the-vendored-flag-design-system) |
| `internal/pipeline` | `lens run` flags (`cli.go`) and `lens manage` (`manage.go`); the orchestrator (input loading, type detection, concurrent processing, auto-mode passes, signal handling, de-duplication); the website, app and CTV processors behind small interfaces so tests drive them with fakes |
| `internal/config` | Defaults → `config.json` → environment; legacy-key and dead-model upgrades; `LoadDotenv` |
| `internal/dotenv` | `.env` reading/writing, a port of python-dotenv 1.2.3 (BSD-3, `internal/dotenv/LICENSE`) |
| `internal/item` | `ContentType`, `WorkItem`, `CTVWorkItem` |
| `internal/detect` | Identifier-column detection, TLD/Android/iOS rules, CTV list detection, headerless lists, the dashboard's per-type breakdown |
| `internal/tabular` | CSV and XLSX loading into a string table (every cell kept as text), pandas' header/NaN/duplicate-name rules where detection depends on them |
| `internal/scrape` | Direct HTTP fetch (browser-like headers, retries, capped full-body read, redirect policy, charset decoding) and the deep backend (rootless Podman `selenium/standalone-chrome`); result validation and block-page detection |
| `internal/scrape/webdriver` | Minimal W3C WebDriver client (`net/http`) for the deep pass |
| `internal/htmldom` | Small HTML tree (find / find-all / text / title) the extractors parse with: a port of CPython 3.12's `html.parser` tokenizer building bs4 4.15.0's `html.parser` tree |
| `internal/openrouter` | OpenRouter chat-completions client over `net/http`: function-calling schemas, taxonomy prompt, retries with backoff, fallback model, truncated-response retries, website/CTV research, CTV news routing |
| `internal/stores` | iTunes Search API and Google Play clients; shared rate limits, bounded concurrency, retries |
| `internal/categories` | Custom-category definitions (`Spec`), validation, and the OpenRouter Decisions API client with the TypeSafe fallback; `pyjson` writes JSON byte-for-byte as Python's `json` did |
| `internal/cost` | Per-run and per-item OpenRouter spend meters on the context |
| `internal/progress` | Progress-file persistence: atomic write + rename after every item; resume skips successes |
| `internal/report` | Plain progress/metrics lines and the optional JSONL attempt log |
| `internal/logx` | `log/slog` with the Python release's line format (the dashboard's log view and operators' greps expect it) |
| `internal/web` | `lens serve`: dashboard pages and assets, eagerly validated legacy/central auth provider, public `/health` and `/signed-out`, uploads/downloads, the `managed-files/spool/` watcher for `lens submit`; mounts `/api/v1` and `/mcp` |
| `internal/jobs` | Job queue (`_jobs.json`), `LENS_MAX_PARALLEL_JOBS` workers running `lens run` subprocesses, cancellation, the input library |
| `internal/catalog` | Model dropdowns from OpenRouter's `/models`, cached 1 h, price-capped |
| `internal/api` | `/api/v1` REST routes and the job/result/download operations MCP shares; bearer-token auth only, never the browser session (see `docs/API.md`) |
| `internal/mcp` | `POST /mcp`: stateless Streamable-HTTP MCP server (JSON responses, no sessions) whose tools adapt `internal/api`; tools filtered and re-checked by token scope |
| `internal/apitoken` | Scoped API tokens (`jobs:read`, `jobs:run`) stored as SHA-256 in `/var/lib/lens/api_tokens.db`, signed 15-minute download links, and `lens token` |
| `internal/auth` | Legacy `elcano_auth` cookie verification and central Auth (code + PKCE, `id_token`, local SQLite sessions/allowlist, back-channel logout); `lens access`, `lens migrate-access-db` |
| `internal/submit` | `lens submit`: root half writes the spool entry, service-user half validates and waits for the job id |
| `internal/opscli` | Operator commands (`start`, `stop`, `restart`, `status`, `logs`, `update`, `rebuild`, `host`, `env`, `tls`), sudo/runuser re-exec, `lens --help` text |
| `internal/doctor` | `lens doctor`: read-only, bounded deployment diagnostics |
| `internal/version` | Build identity stamped by the Makefile; `lens --version` |
| `internal/deploytest` | Hermetic tests for the bash deploy scripts (stub tools on `PATH`); no non-test code |
| `content_taxonomy.tsv` | IAB content taxonomy, the classifier's permitted vocabulary; embedded in the binary |

## Configuration

Three-layer config: **defaults** → `config.json` → **environment variables / CLI flags** (highest priority).

- `config.json` — all scrape mode defaults, model names, concurrency limits, iOS/Android/CTV-specific settings (see `config.json.example`). `lens run` reads it from its working directory; `lens serve` from the Lens home (`LENS_HOME`, else the working directory — `/opt/lens` under systemd), which is also where jobs run.
- `.env` — secrets and per-host settings: `OPENROUTER_API_KEY` (required to run the pipeline), `AUTH_SIGNING_PUBKEY` (required for web sign-in — the auth service's Ed25519 public key; the dashboard verifies the `elcano_auth` cookie with it), `AUTH_LOGIN_URL` / `AUTH_COOKIE_NAME` (optional overrides), `SCRAPER_VERBOSE` (optional). See `.env.example`. Every command loads the nearest `.env` (working directory or a parent) without overriding variables already set; the service-user commands (`lens token`, `lens access`) read the instance's `.env` (`LENS_HOME`, else `LENS_APP_DIR`).
- `config.Config` is a plain struct whose JSON names are the `config.json` keys; it is passed explicitly, there is no global.

### Optional custom categories

`internal/categories` validates per-run definitions and batches independent
questions against original source evidence using OpenRouter's Decisions API
(`/api/alpha/decisions`, `OPENROUTER_API_KEY`). `decision_model`
(default `~typesafe/jev-latest`), `--decision-model` or the dashboard dropdown
picks the model. When the model is Jev and `TYPESAFE_API_KEY` is set, a failed
OpenRouter request is retried once against TypeSafe's own endpoint. Enable with
`--custom-categories JSON_FILE` or the dashboard editor. Processors append
custom columns after standard success; decision errors never fail the primary
result or become negative labels. Resumed CSVs rename the old `TypeSafe_*`
columns to `Decision_*`. Definitions are snapshotted in jobs, progress and a CSV sidecar;
changed definitions require new output/progress paths. See
[`docs/CUSTOM_CATEGORIES.md`](docs/CUSTOM_CATEGORIES.md).

## Gotchas and Non-Obvious Patterns

### A missing API key fails the run, not the load

`config.Load` never fails for a missing `OPENROUTER_API_KEY` (the Python release's config singleton crashed on import). The commands that need the key call `RequireAPIKey`: `lens run` fails after logging is up, so the reason lands in the run's log. `lens serve` starts without it — but every job it runs fails, and `lens doctor` reports the missing key.

### CTV news routing

The IAB taxonomy lacks a Tier-1 "News" category. CTV news apps are deterministically routed post-classification to `Politics > Civic Affairs` (or `Elections`/`Weather`/`Crime`/`Disasters` based on content signals). This logic lives in `internal/openrouter` (`ApplyCTVRouting`) and is tested in `internal/openrouter/ctv_test.go`.

### Content type detection edge cases

`internal/detect` handles ambiguous cases where domains start with TLD-like prefixes (e.g., `io.example.com` should be a website, not an Android app). The rules, in order:
- Clear URL/website indicators win (scheme, URL punctuation, `www.`, ports, IPs, hyphens, well-known TLD endings)
- Numeric-only → iOS app ID
- A structurally valid package name → Android when it starts with a known reverse-DNS prefix, its leaf label is not a real IANA TLD, or it nests five or more labels deep
- Otherwise → website (domain)

CTV items are detected by column headers or the `--ctv` flag, not by the identifier format.

### Deep scrape forces single concurrency

The deep backend drives the Podman Chrome container (`selenium/standalone-chrome`) over WebDriver, and the session pool is forced to one because the image serves one browser session (`internal/scrape`, with a warning when more were asked for). Each run uses its own `<deep_scrape_container_name>-<pid>` on free ports, so parallel dashboard jobs never share a browser.

### Auto-mode ladder, anti-bot, and ban avoidance

Auto mode runs `direct → deep → research`. `direct` is a plain HTTP fetch (no JS); the deep pass (headless Chrome in a rootless Podman container, single session) rescues JS-rendered sites and is skipped when Podman is unavailable; and the **research fallback** classifies whatever still failed from a web-search research summary (`ResearchWebsite` in `internal/openrouter`, default model `perplexity/sonar`; Sonar Pro is still selectable) instead of failing it. Research rows carry `Scrape_Mode="research"`; domains the research model finds nothing about stay Failed rather than being guessed at. (Firecrawl used to sit between direct and deep; it was removed. A legacy `scrape_mode: "firecrawl"` in `config.json` maps to `auto` with a warning, dashboard jobs persisted with mode `firecrawl` run as `auto`, and other `firecrawl_*` keys are ignored.)

What no scrape backend can do is beat commercial anti-bot (Cloudflare/DataDome/PerimeterX) from this host's datacenter IP. Those sites (reuters, bloomberg, wsj, economist, politico, barrons, marketwatch, mansionglobal, …) block the IP outright. The setup is deliberately tuned to fail those **fast and quietly** rather than fight them, because repeated hammering from one IP is what escalates a soft block into a permanent ban — the research fallback then classifies them without ever touching the site from this host:

- `skip_deep_retry_on_block` (default on): the deep pass **skips** domains the direct pass proved are anti-bot blocked (401/403/429 or a block page). The deep crawler renders from the same IP, so it would fail identically while adding load. It still runs for non-block failures (timeouts, too-short/JS-only pages). Blocked domains go straight to the research fallback.
- `retry_follow_redirects` (default on; the legacy `firecrawl_follow_redirects` key is honoured when it is unset): the deep retry pass follows cross-host redirects (the `direct` pass still rejects them, except same-site moves between a domain and its subdomains such as `example.com → home.example.com`). Publishers that redirect a brand domain to a parent (`foxnewsdigital.com → foxnews.com`, `abcnews.go.com → abcnews.com`) classify on the destination's content instead of failing.
- `deep_retry_enabled` (default on): master switch for the deep retry pass; also togglable per job from the web UI ("Retry with headless Chrome"), `lens submit --no-deep` and `--deep-retry off`. The deep crawler runs one browser per job, so it dominates run time on large lists.
- `research_fallback_enabled` (default on): master switch for the research pass; also togglable per job from the web UI ("Research unscrapeable sites"), `lens submit --no-research-fallback` and per run with `--research-fallback on|off`.

Scraping the hardened sites through a residential proxy is not currently supported. Do not raise retry counts to "try harder" — that increases ban risk without helping.

### Direct-pass body reads must drain the stream

The direct pass reads the body to EOF under a size cap (`readBody` in `internal/scrape`: `io.ReadAll(io.LimitReader(...))`). Never replace that with a single `Read` call: a single read returns whatever is buffered (the Python release's `aiohttp` `read(n)` returned the first 128 KiB chunk), which silently truncated modern pages to their `<head>` and made real sites fail validation as "content too short".

### Frontend: Tier A, and the vendored Flag design system

Lens is **Tier A** of the Elcano frontend standard above (`stack.conf`: `tier=A`, `web=static templates internal/web` — `internal/web` builds and serves the HTML, so stack-check scans its Go too — `backend=go`): two `html/template` pages (`templates/index.html`, `templates/signed_out.html`), plain CSS (`static/styles.css`) and two plain-JS files (`static/main.js`, `static/custom-categories.js`). There is no npm tree, bundler or framework. `internal/web` serves it all from the embedded tree: every asset URL goes through the `static_url` template helper, which appends `?v=<content hash of static/>` (`StaticVersion`), and `headerPolicy` (`internal/web/middleware.go`) sets the security headers and cache policy on every response — pages get a per-response nonce CSP (`script-src 'self' 'nonce-…'`, `style-src 'self'`, `frame-ancestors 'none'`; the one inline script is the nonce'd theme bootstrap, and there are no inline handlers or `style` attributes), everything else the closed `default-src 'none'` policy; `nosniff` and `X-Frame-Options: DENY` everywhere; versioned assets `immutable` for a year, unversioned ones `no-cache`, HTML and JSON `no-store`. `TestEveryResponseCarriesTheFrontendStandardHeaders` pins that.

`static/design-system/` is a **vendored copy** of files from `ElcanoTek/flag`'s `design-system/` directory, not a package. `flag.lock` lists every vendored file with its sha256 and the flag commit it came from: whole `fonts/`, `tokens/`, `theme/` and `icons/` directories, plus the two logos Lens uses (`logos/lens-favicon.svg`, `logos/elcano-mark-primary.svg`), and Flag's MIT licence (`design-system/LICENSE`, vendored as `static/design-system/FLAG-LICENSE` and listed in `NOTICE`, so the licence travels with the copies). `scripts/flag-sync.sh check` (part of `make stack-check`, run in CI) fails on any hand edit or on an unlocked file inside a locked directory. **Never edit a file under `static/design-system/`**: to update, check out flag next to this repo (`../flag`, or pass its path / set `FLAG_DIR`), run `scripts/flag-sync.sh status` to see what changed, `scripts/flag-sync.sh sync` to re-copy every locked file (locked directories are mirrored: files flag added appear, files it removed go) and rewrite the lock, then run `make test` — `TestTemplatesReferenceOnlyShippedAssetsAndIcons` fails if a template names an asset or a `core-icons.svg#id` symbol that no longer ships — and render the dashboard to check nothing moved. CI checks this two ways: the PR gate is `make stack-check` (offline: hash and hand-edit detection, no flag checkout), and the weekly `Flag drift` workflow (`.github/workflows/flag-drift.yml`, shared verbatim across repos apart from the pinned checkout action) checks out flag `main` into `.flag/` and runs `scripts/flag-sync.sh status` against it, so a red run there means upstream moved, not that this repo is broken — fix it with a `sync` PR. To vendor another flag file, `scripts/flag-sync.sh add <flag-path> static/design-system/<flag-path>`. If Lens needs something flag lacks or gets wrong, the fix goes into flag first; do not patch the copy. `scripts/flag-sync.sh` and `scripts/stack-check.sh` (and `.github/workflows/flag-drift.yml`) are shared verbatim by every Elcano repo — don't fork them here, and don't add an SPDX header to them or to the generated `flag.lock`; a bug in them is fixed in every repo at once.

Product styling belongs in `static/styles.css`, which reads design tokens and never names a family, colour or radius itself. The tree is embedded in the binary (`assets.go`), so a change needs a rebuild. Flag is MIT-licensed (`static/design-system/FLAG-LICENSE`, listed in `NOTICE`); some glyphs in `icons/core-icons.svg` are drawn after Lucide (ISC, notice kept at the top of the sprite and credited in `NOTICE`); the logos remain ElcanoTek trademarks.

Elcano ships exactly **two typefaces**, everywhere: **Nebula Sans** (SIL OFL 1.1) for UI, body and headings, and **Hack** (MIT) for code, logs and tabular output. `static/design-system/fonts/fonts.css` is the one font sheet — it declares every `@font-face` and sets `--font-brand` / `--font-code-brand` / `--font-code-ui-brand`, which `tokens/design-tokens.css` reads. Both licences require the licence text to travel with the binaries, so `fonts/nebula-sans/OFL.txt` and `fonts/hack/LICENSE.md` must be copied with the font files and stay listed in `NOTICE`. Nothing is CDN-loaded; there is no Google Fonts request in the dashboard. `TestBundledFontsAreTheTwoBrandFaces` (`internal/web/server_test.go`) fails if a third face, a dead `url()`, a missing licence or a CDN host appears.

### Web UI model picker

The dashboard's Advanced settings populate two model dropdowns from OpenRouter's `/models` endpoint (`internal/catalog`, cached 1h): the **classification model** (must support tool calling; price-capped by `ModelPromptPriceCap`/`ModelCompletionPriceCap` — classification is high-volume and doesn't benefit from frontier-priced models) and the **research model** (must advertise `web_search_options`, i.e. built-in web search; capped by `ResearchPromptPriceCap`/`ResearchCompletionPriceCap`). `~vendor/…-latest` aliases (OpenRouter's self-updating pins) sort first; `~openai/gpt-luna-latest` and `perplexity/sonar` are the recommended defaults (browsers that saved the old `perplexity/sonar-pro` default follow the new one once). A `config.json` still pinning the previous shipped classifier `~google/gemini-flash-latest` is upgraded on load. Selections reach the job subprocess via `--llm-model` / `--research-model`, choices persist per browser in localStorage, and non-default settings show on the run's row in the Runs table.

Research model choices are additionally restricted to the verified automatic-search
models in `catalog.ResearchModels` (Sonar and Sonar Pro). Advertising `web_search_options`
alone does not ensure a plain chat request searches. Search fees appear separately
in picker labels. `--research-model` applies to both website and CTV research;
custom-category questions are included as evidence needs in both research prompts.
See [`docs/RESEARCH_MODELS.md`](docs/RESEARCH_MODELS.md) for the live cost comparison.

Gemini classification requests use low reasoning effort to leave room for the
structured answer; merely excluding reasoning text does not reduce billed tokens.
This applies to website, mobile-app and CTV classification, not research calls.
CTV rejects empty/insufficient research before classification, and passes the
input publisher into both research and classification prompts.

### Cost tracking

`internal/cost` records OpenRouter's reported `usage.cost` for every billed
response: every chat call in `internal/openrouter` (including ones that are
then retried or rejected) and the Decisions request in `internal/categories`,
each via `cost.Record(ctx, …)`. Two meters ride on the `context.Context` so
concurrent items never mix: the orchestrator installs a run meter
(`cost.WithRun`, restored from and saved to the progress file's `cost` key),
and each processor entry point opens an item meter (`cost.WithItem`) that is
read into `Cost_USD` when the row is written (`cost.ItemUSD`). A new processor
entry point needs `WithItem`, and a new model call path must call
`cost.Record` with the request's context. `make test-race` is the gate here.

### Web service spawns subprocesses

`lens serve` runs `lens run` (its own executable, `os.Executable`) as a subprocess for each job, in the Lens home, passing CLI args. It does **not** call the pipeline directly. This means environment variables and CLI flags are the only way to configure job execution.

### Taxonomy and assets are embedded

`content_taxonomy.tsv`, `iana_tlds.txt`, `static/` and `templates/` are compiled into the binary (`assets.go`), so a deployed binary can never classify against a missing taxonomy or serve a `main.js` from another release, and `bin/lens` runs from any directory. Editing any of them needs a rebuild; `lens doctor` checks that the running binary carries them.

### iTunes API returns text/javascript

The iOS client (`internal/stores/ios.go`) parses the body as JSON whatever the `Content-Type`, because the iTunes Search API returns `text/javascript` instead of `application/json`. Don't add a content-type check.

### Progress is saved per-item

`progress.Tracker.Mark` writes the progress file (atomic write + rename) after every single item, making the pipeline crash-resilient. Already-processed items are skipped on re-run.

### On-disk formats are the Python release's

Deployed hosts upgraded in place from Python, so every persisted format is byte-compatible with what that release wrote and read: the output CSV (Python `csv` quoting, `\r\n`, `str()` rendering), `progress.json`, `_jobs.json`, the `<output>.categories.json` sidecar and `Decision_Answers` (`pyjson`), log lines (`internal/logx`), `.env` grammar (`internal/dotenv`), and the SQLite schemas of `access.db` and `api_tokens.db`. Golden and byte-level tests recorded against the Python release pin these. Don't "tidy" a format; Fleet, operators' scripts and resumed runs read them.

## Dependencies and in-housed code

Prefer the standard library or a small package we own; add a module only
when it clearly earns its maintenance and supply-chain cost. Dependabot
proposes updates for `gomod` and GitHub Actions (daily, cooldown 7 days (3 for
patch releases), grouped); a human merges them only when the bump earns it —
a security fix whose code path we use, or a specific commit we need. In-housed
code is ours and is not tracked by Dependabot. On a CVE, check reachability
first: `make govulncheck` (CI's go job) is call-graph aware and reports only
vulnerabilities reachable from Lens, stdlib included. If the affected code is
unused, say so in the PR (or delete the code that pulls it in); if it is used,
take the targeted fix. `go.mod` / `go.sum` are changed with `go get` and
`go mod tidy`, never by hand. A Go security release means moving `go.mod`'s
`go` line (govulncheck reports stdlib CVEs against the toolchain that built
the binary) — and possibly the golangci-lint pin with it (see `.golangci.yml`).

Worked example: GO-2026-6809 (`crypto/x509`) was reported as reachable from
`lens tls status`; the fix was moving `go.mod` to go 1.27.3, with no module
pin moved.

Deliberately still dependencies (`go.mod`):

| Module | Why it earns its place |
|---|---|
| `golang.org/x/net` | `publicsuffix` for the direct scraper's cookie jar — the stdlib jar without a public-suffix list would scope cookies wrongly across registrable domains. Maintained by the Go team. |
| `golang.org/x/text` | `encoding/htmlindex` decodes non-UTF-8 pages (Shift_JIS, GBK, KOI8-R, …) by their WHATWG label before classification. Maintained by the Go team. |
| `modernc.org/sqlite` | SQLite for `access.db` and `api_tokens.db`, in pure Go: the binary stays `CGO_ENABLED=0` and static, so a distro libc upgrade can't break it, and the schemas stay identical to the Python release's. Its indirect modules (`modernc.org/libc`, `mathutil`, `memory`, `x/sys`, …) come with it. |

What a PyPI dependency did in the Python release and now lives in the tree:

| Replaced | Now | Upstream / version / licence |
|---|---|---|
| `beautifulsoup4` + stdlib `html.parser` | `internal/htmldom` — a port of CPython 3.12's `html.parser` tokenizer and a tree builder reproducing bs4's `html.parser` tree (no implicit closes, bs4's entity rules, whitespace collapse, script/style text excluded). `golang.org/x/net/html` is an HTML5 tokenizer and differs in many corner cases, so it is not used. | tokenizer: CPython 3.12 (PSF); tree: own code; behaviour pinned by golden and differential fixtures recorded from bs4 4.15.0 / the Python `html_dom` module |
| `selenium` (+ its transitive tree) | `internal/scrape/webdriver` — own W3C WebDriver client (new session, navigate, current URL, page source, quit, page-load timeout) on `net/http` | own code; the Chrome container image is still `selenium/standalone-chrome` |
| `python-dotenv` | `internal/dotenv` — port of `parser.py`, `variables.py` and the parts of `main.py` Lens used (no `get_key`/`unset_key`), plus `SetKey` for bootstrap | python-dotenv 1.2.3, BSD-3-Clause; licence in `internal/dotenv/LICENSE`, upstream test cases ported into `dotenv_test.go` / `parser_test.go` |
| `pandas` / `numpy` / `openpyxl` | `internal/tabular` — CSV via `encoding/csv`, and an XLSX reader on `archive/zip` + `encoding/xml` with zip-bomb and cell-count limits | own code |
| stdlib `json` output bytes | `internal/categories/pyjson` — ordered objects, Python's int/float split and float repr, `ensure_ascii`, `", "`/`": "` separators, for the few outputs that must stay byte-identical | own code |
| `openai` SDK, `aiohttp`, `fastapi`/`starlette`/`uvicorn`/`python-multipart`, `jinja2`, `itsdangerous`, `cryptography`, `rich` | the standard library: `net/http`, `html/template`, `crypto/ed25519`, `crypto/hmac`, `log/slog` | — |

Ported files carry a header naming the upstream release and path, keep their
licence beside them (`internal/dotenv/LICENSE`) and stay listed in `NOTICE`.
Change them only to apply an upstream fix or to match the behaviour the
Python release had; their tests are the contract. Nothing ported here has a
data file to refresh except `internal/htmldom/entities.go`, which is generated
from Python's `html.entities.html5`.

## Testing

- **Framework**: the standard `testing` package; `httptest` servers stand in for OpenRouter, the stores, WebDriver and Auth. No third-party test libraries.
- **CI** (`.github/workflows/tests.yml`): `stack` job — `make stack-check` (frontend standard, offline); `go` job — `make fmt-check`, `go build ./...`, `make build` + `./bin/lens --version` (must name `HEAD`), `go vet`, `go test -race -count=1 ./...`, the Python → Go update-path test as root, `make govulncheck`; `lint` job — golangci-lint v2.14.0 (full gate); `shell` job — `make shellcheck-install` (the ShellCheck release pinned by version and sha256 in the Makefile, the same one the cloud hook installs) then `make shellcheck`; `deploy-build` job — `lens_build` from `scripts/lib/deploy.sh` with Go 1.24 and stable (`GOTOOLCHAIN=auto` must fetch the pin), then the relocated binary must run from `/`.
- **Classification smoke** (`.github/workflows/test-classification.yml`): three parallel jobs (test-urls, test-apps, test-ctv) run `./bin/lens run --scrape-mode direct --quiet` (plus `--ctv`) on 10 sample items each, on push/PR to main/master. They need the `OPENROUTER_API_KEY` secret and self-skip without it (Dependabot PRs); the build still runs.
- **CodeQL** (`codeql.yml`): Go and Actions, on PRs and weekly.
- **Flag drift** (`flag-drift.yml`): weekly (and on demand) `scripts/flag-sync.sh status` against flag `main`; reports upstream changes, never gates a PR.
- **No key needed**: nothing reads `OPENROUTER_API_KEY` at package load; tests that need it call `t.Setenv`.
- **CTV tests**: the two-step pipeline with fakes; platform detection, news routing, markdown stripping, research truncation (`internal/openrouter/ctv_test.go`, `internal/pipeline`).
- **Golden fixtures** (`testdata/` in `htmldom`, `detect`, `openrouter`, `tabular`, and inline expectations elsewhere) were recorded from the Python release — HTML trees, detection results, prompts and request bodies, spreadsheet parsing — and pin cross-release compatibility.
- **Filesystem isolation is by construction**: no package keeps a path global. The Lens home is an argument (`web.Options{Home}`, `jobs.NewManager(home)`) or `LENS_HOME`, and `lens run` resolves its relative defaults against the working directory, so tests build everything under `t.TempDir()` and pass it in, or use `t.Setenv("LENS_HOME", …)` / `t.Chdir(t.TempDir())`. A test therefore cannot write into a developer's gitignored `managed-files/`, `.env` or `config.json` — the failure mode that once let a Python test silently replace a real operator's job history. Keep new tests that way; never default to the repository's working directory. This is prevention, not detection: do not replace it with a check that inspects `managed-files/` afterwards (that reports damage instead of stopping it, and false-positives whenever a local dev server is running).
- **Claude Code cloud sessions**: `.claude/hooks/session-start.sh` (registered in `.claude/settings.json`, runs only when `CLAUDE_CODE_REMOTE=true`) downloads the modules, pre-builds, installs golangci-lint v2.14.0 into `$(go env GOPATH)/bin` and the pinned ShellCheck (`make shellcheck-install`) into `~/.local/bin` (both put first on `PATH`; a failed install fails the hook), so `make test`, `make lint`, `make shellcheck` and `make stack-check` work out of the box.
- **Deploy scripts**: `internal/deploytest` sources and runs `install.sh`, `scripts/*.sh` and `scripts/lib/*.sh` in a scratch tree with stub `dnf`/`systemctl`/`podman`/… on `PATH`, including a full `update.sh` run and the first Python → Go migration.

## Deployment

Production target: **Fedora/RHEL 9+**

- `scripts/bootstrap.sh` — Full installer: installs `golang`, `make` and the rest via dnf, creates the `lens` system user, installs to `/opt/lens`, keeps persistent central-auth state under `/var/lib/lens`, builds `bin/lens` with `make build` (`lens_build`; `bin/` is owned by root, because root runs the binary for every operator command), writes `/opt/lens/.env` with `lens env set` (`AUTH_SIGNING_PUBKEY` + `OPENROUTER_API_KEY` …), installs the systemd unit, links `/usr/local/bin/lens` → `/opt/lens/bin/lens`, and optionally sets up Caddy TLS. It also removes Cockpit when `cockpit-ws` is installed (`lens_cockpit_uninstall`; `LENS_KEEP_COCKPIT=1` keeps it). It makes SSH key-only with `/etc/ssh/sshd_config.d/01-no-password-login.conf` when at least one authorized key is installed (`lens_ssh_key_only`; `LENS_KEEP_SSH_PASSWORDS=1` skips it; never fatal). Given a public hostname, it names the box after it when the hostname is an unset cloud default (`scripts/lib/hostname.sh`; `LENS_SET_HOSTNAME=1` forces, `=0` skips; update never renames). Lens owns no password of its own — sign-in is either the legacy shared `elcano_auth` cookie or central Auth's one-use application handoff.
- `install.sh` — Public HTTPS entry point: installs Git, clones main and runs bootstrap; refuses to overwrite an existing checkout.
- `scripts/update.sh` — Locked staged update: fetch, `make build` in `/opt/lens.staging.*` (the staged binary must report the target commit), snapshot the live release, stop, `lens migrate-access-db`, swap, health check and automatic rollback of code, binary, unit and CLI. `.deployed-revision` distinguishes the source checkout from deployed code so retries work. On a host still on the Python release, the old `update.sh` fetches and re-execs this one, which installs Go itself, snapshots the Python `.venv` with the code so a failed health check brings Python back, and removes the venv only after the Go release answers `/health`.
- `scripts/lib/deploy.sh` — Shared instance-state rsync exclusions (`LENS_STATE_EXCLUDES`), deploy lock, `lens_build` (root-owned Go caches in `/var/cache/lens-build`, `GOTOOLCHAIN=auto`, `GOENV=off`), `lens_ensure_go`, release restoration and bootstrap's `lens_cockpit_uninstall` and `lens_ssh_key_only`. Keep bootstrap/update/rollback exclusions centralized here.
- `internal/doctor` (`lens doctor`) — Read-only deployment checks with JSON/strict modes; depends only on the dotenv parser, config defaults and the embedded assets, never prints secret values.
- `scripts/host.sh` — DNF check/update, optional distro Node/npm/Go, and latest stable Fedora upgrade download (max two releases; no automatic reboot).
- `deploy/lens.service` — systemd unit; runs `/opt/lens/bin/lens serve --addr 127.0.0.1:8808` with `WorkingDirectory=/opt/lens`; hardened with `ProtectSystem=full`, `ProtectKernelModules`, `LockPersonality` and a restricted address-family set. `NoNewPrivileges`, `ProtectControlGroups`, `ProtectKernelTunables`, `PrivateTmp` and `ProtectHome` are deliberately **absent** — each one breaks rootless podman (see the comments in the unit).
- `deploy/lens.caddy` — Caddy reverse proxy config with security headers; `{{HOSTNAME}}` placeholder substituted by bootstrap
- `deploy/nginx-lens.conf` — Nginx alternative reverse proxy config
- `/usr/local/bin/lens` — the operator CLI is the binary itself (a root-owned copy, never a link into the service-owned `/opt/lens`; `internal/opscli`): `start|stop|restart|status|logs|update|rebuild|doctor|host|env|tls`, plus `access`, `token` and `submit`. Root-only verbs re-run themselves under sudo; service-user verbs re-run under runuser/sudo -u.

## CLI Flags Reference (`lens run`)

| Flag | Description |
|---|---|
| `--quiet` | Suppress per-item output |
| `--verbose` | Show size, retries, cache status |
| `--jsonl PATH` | Write per-item JSONL log |
| `--scrape-mode auto\|direct\|deep` | Auto ladder (direct → deep → research), HTTP-only, or headless-Chrome deep scrape |
| `--llm-model MODEL` | Override the classification model (OpenRouter ID) for this run |
| `--deep-retry on\|off` | Enable/disable the deep (headless Chrome) retry pass for this run |
| `--research-fallback on\|off` | Enable/disable the research fallback pass for this run |
| `--research-model MODEL` | Override the research-fallback model (needs built-in web search) |
| `--custom-categories JSON_FILE` | Add custom categories |
| `--decision-model MODEL` | Override the custom-category decision model |
| `--deep-scrape` | Shortcut for `--scrape-mode deep` |
| `--ctv` | Run CTV workflow |
| `--allow-redirects` | Follow HTTP redirects |
| `--input-csv PATH` | Custom input file (CSV or xlsx) |
| `--output-csv PATH` | Custom output CSV |
| `--progress-file PATH` | Custom progress JSON path |
| `--log-file PATH` | Custom log file path |
