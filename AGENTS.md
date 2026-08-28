# AGENTS.md — Elcano Lens

Orientation for anyone (human or agent) making changes here. Companion docs: [`CONTRIBUTING.md`](CONTRIBUTING.md) for workflow, [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) for operations, and [`docs/LICENSING.md`](docs/LICENSING.md) for what the BSL 1.1 licence allows. New source files carry an SPDX header:

```python
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
```

## Project Overview

Elcano Lens is an async content analysis pipeline that classifies **websites**, **iOS apps**, **Android apps**, and **CTV (Connected TV) apps** into advertising quality tiers using LLM classification via OpenRouter. It outputs CSV with IAB content taxonomy categories, quality ratings, and metadata.

Two runtime modes: **CLI** (`main.py`) and **web dashboard** (`web_service.py`), sharing the same processing pipeline.

## Essential Commands

```bash
# Install dependencies (runtime only; use requirements-dev.txt for tests).
# Both are pip-compile lock files — edit requirements.in, not the .txt.
pip install -r requirements.txt

# Run website analysis (default scrape mode)
python main.py

# Run with direct HTTP scraping (no browser), quiet mode
python main.py --scrape-mode direct --quiet

# Run CTV workflow
python main.py --ctv

# Run with deep scrape (Selenium via Podman container)
python main.py --deep-scrape

# Run through the local Firecrawl service (start it first: scripts/firecrawl.sh up)
python main.py --scrape-mode firecrawl
# Firecrawl tracks `latest` (refreshed on every `lens update`); its queue DB is
# disposable — scripts/firecrawl.sh reset wipes and rebuilds it

# Run with JSONL logging and custom I/O
python main.py --jsonl run.jsonl --input-csv my_input.csv --output-csv my_output.csv

# Run all tests
pytest tests/

# Run specific test file
pytest tests/test_input_detector.py

# Start web service
uvicorn web_service:app --host 127.0.0.1 --port 8808

# Management utility
python manage.py setup          # Scaffold .env and config.json
python manage.py validate      # Validate config
python manage.py progress      # Show progress summary
python manage.py reset         # Reset all progress
python manage.py reset-failed  # Reset only failed items
python manage.py stats         # Show statistics
```

## Architecture and Data Flow

```
Input CSV/xlsx
    │
    ▼
orchestration.py (SiteAnalysisOrchestrator)
    │
    ├─ input_detector.py → auto-detect ContentType (WEBSITE, IOS_APP, ANDROID_APP, CTV_APP)
    │
    ├─ WEBSITE path:
    │   scraper_client.py → domain_processing.py → openrouter_client.py (classify_website)
    │
    ├─ IOS_APP path:
    │   ios_api_client.py → app_processor.py → openrouter_client.py (classify_app)
    │
    ├─ ANDROID_APP path:
    │   android_scraper.py → app_processor.py → openrouter_client.py (classify_app)
    │
    └─ CTV_APP path (two-step pipeline):
        openrouter_client.py (research_ctv_app with Perplexity Sonar Pro)
        → openrouter_client.py (classify_ctv_app with fast model)
        → ctv_processor.py (post-processing, news routing)
    │
    ▼
Output CSV + progress.json (resumable)
```

The web service (`web_service.py`) spawns `main.py` as a subprocess for each job — it does **not** import and call the pipeline directly.

## Code Organization

| Module | Responsibility |
|---|---|
| `main.py` | CLI entry point; argparse for all flags; creates orchestrator and runs async |
| `orchestration.py` | Core orchestrator; loads input, auto-detects types, initializes clients, processes items concurrently with semaphore; CTV workflow dispatch; signal handlers for graceful shutdown |
| `config.py` | Central config class; loads from `config.json` + env vars via dotenv; **creates global singleton on import** (`config = Config()`) |
| `shared_types.py` | `ContentType` enum, `WorkItem` (frozen dataclass), `CTVWorkItem` (mutable dataclass) |
| `input_detector.py` | Content type detection: TLD classification, Android package validation, CTV input detection, column detection |
| `scraper_client.py` | Async HTTP scraper (aiohttp direct mode + Selenium deep mode via Podman container + local Firecrawl service mode) |
| `domain_processing.py` | Website pipeline: scrape → validate → classify → record |
| `openrouter_client.py` | OpenRouter LLM client (OpenAI SDK); function calling schemas for structured output; taxonomy loading; retry with exponential backoff; CTV two-step methods |
| `ios_api_client.py` | iTunes Search API client; async with rate limiting |
| `android_scraper.py` | Google Play Store HTML scraper; BeautifulSoup parsing; block page detection |
| `app_processor.py` | iOS/Android app processor; fetches metadata then classifies via LLM |
| `ctv_processor.py` | CTV two-step processor; research → classify → post-process; markdown stripping; research truncation; platform detection from bundle ID |
| `progress_tracker.py` | JSON-based progress persistence; atomic writes; resumable processing |
| `reporting.py` | Terminal reporter; Rich-based progress bar; JSONL logging |
| `web_service.py` | FastAPI web dashboard; unified `elcano_auth` cookie auth (see `auth_cookie.py`); job queue (one at a time); file upload/download |
| `manage.py` | CLI utility for setup, progress, reset, validation, stats |
| `content_taxonomy.tsv` | IAB content taxonomy used for classification; loaded and cached as class variable in `openrouter_client.py` |

## Configuration

Three-layer config: **defaults** → `config.json` → **environment variables** (highest priority).

- `config.json` — all scrape mode defaults, model names, concurrency limits, iOS/Android/CTV-specific settings (see `config.json.example`)
- `.env` — secrets and per-host settings: `OPENROUTER_API_KEY` (required), `AUTH_SIGNING_PUBKEY` (required for web sign-in — the auth service's Ed25519 public key; the dashboard verifies the `elcano_auth` cookie with it), `AUTH_LOGIN_URL` / `AUTH_COOKIE_NAME` (optional overrides), `SCRAPER_VERBOSE` (optional). See `.env.example`.
- All config keys are uppercased as attributes on the global `config` singleton

## Gotchas and Non-Obvious Patterns

### Config singleton import crash

`config.py` creates `config = Config()` at module level (line ~184). This means **any import from any module that imports `config` will crash if `OPENROUTER_API_KEY` is not set**. Tests work around this with `os.environ.setdefault("OPENROUTER_API_KEY", "test-key")` before importing project modules.

### CTV news routing

The IAB taxonomy lacks a Tier-1 "News" category. CTV news apps are deterministically routed post-classification to `Politics > Civic Affairs` (or `Elections`/`Weather`/`Crime`/`Disasters` based on content signals). This logic lives in `openrouter_client.py` and is tested extensively in `test_ctv_functionality.py`.

### Content type detection edge cases

`input_detector.py` handles ambiguous cases where domains start with TLD-like prefixes (e.g., `io.example.com` should be a website, not an Android app). The detection rules are:
- Numeric-only → iOS app ID
- Starts with `com.` → Android package
- Otherwise → website (domain)

CTV items are detected by column headers or the `--ctv` flag, not by the identifier format.

### Deep scrape forces single concurrency

`--deep-scrape` mode uses Selenium via a Podman Chrome container and forces `max_concurrent=1` because only one Chrome session is practical.

### Auto-mode ladder, anti-bot, and ban avoidance

Auto mode runs `direct → Firecrawl → deep → research`. `direct` is a plain HTTP fetch (no JS); Firecrawl (headless Chromium via Playwright) rescues JS-rendered sites; the deep (Selenium Chrome) pass mops up sites Firecrawl mishandled; and the **research fallback** classifies whatever still failed from a web-search research summary (`research_website` in `openrouter_client.py`, default model `perplexity/sonar-pro`) instead of failing it. Research rows carry `Scrape_Mode="research"`; domains the research model finds nothing about stay Failed rather than being guessed at.

What no scrape backend can do is beat commercial anti-bot (Cloudflare/DataDome/PerimeterX) from this host's datacenter IP. Those sites (reuters, bloomberg, wsj, economist, politico, barrons, marketwatch, mansionglobal, …) block the IP outright. The setup is deliberately tuned to fail those **fast and quietly** rather than fight them, because repeated hammering from one IP is what escalates a soft block into a permanent ban — the research fallback then classifies them without ever touching the site from this host:

- `firecrawl_proxy` defaults to `"basic"` — one render attempt, no stealth-proxy escalation. (Firecrawl's `"auto"` escalation is futile self-hosted: there's no stealth proxy backend, so it just burns 30–90s/site re-fetching and then returns `document_antibot`.)
- `SCRAPE_RETRY_LIMIT` / `document_antibot` responses are treated as **permanent** in `scraper_client._scrape_with_firecrawl` (not retried) — an anti-bot verdict won't change on retry.
- `skip_deep_retry_on_block` (default on): the deep pass **skips** domains whose Firecrawl failure was an anti-bot/401/403 block. The deep crawler is also headless Chrome on the same IP, so it would fail identically while adding load. It still runs for non-block failures (timeouts, service hiccups). Blocked domains go straight to the research fallback.
- `firecrawl_follow_redirects` (default on): the Firecrawl **and** deep retry passes follow cross-host redirects (the `direct` pass still rejects them). Publishers that redirect a brand domain to a parent (`foxnewsdigital.com → foxnews.com`, `abcnews.go.com → abcnews.com`) classify on the destination's content instead of failing.
- `research_fallback_enabled` (default on): master switch for the research pass; also togglable per job from the web UI ("Research unscrapeable sites") and per run with `--research-fallback on|off`.

To actually scrape the hardened sites, route Firecrawl's browser through a residential/ISP proxy: set `PROXY_SERVER` (+ `PROXY_USERNAME`/`PROXY_PASSWORD`) in `firecrawl/.env` (see `firecrawl/.env.example`). Do not raise retry counts or flip `firecrawl_proxy` to `auto` to "try harder" — that increases ban risk without helping.

### Direct-pass body reads must drain the stream

`aiohttp`'s `StreamReader.read(n)` returns whatever is buffered (typically the first 128 KiB chunk), not the full body. `ScraperClient._read_body` loops until EOF/cap for this reason — a single `read()` call silently truncates modern pages to their `<head>` and made real sites fail validation as "content too short".

### Web UI model picker

The dashboard's Advanced settings populate two model dropdowns from OpenRouter's `/models` endpoint (`_get_model_catalog` in `web_service.py`, cached 1h): the **classification model** (must support tool calling; price-capped by `MODEL_PROMPT_PRICE_CAP`/`MODEL_COMPLETION_PRICE_CAP` — classification is high-volume and doesn't benefit from frontier-priced models) and the **research model** (must advertise `web_search_options`, i.e. built-in web search; capped by `RESEARCH_*_PRICE_CAP`). `~vendor/…-latest` aliases (OpenRouter's self-updating pins) sort first; `~google/gemini-flash-latest` and `perplexity/sonar-pro` are the recommended defaults. Selections reach the job subprocess via `--llm-model` / `--research-model`, choices persist per browser in localStorage, and non-default settings show on the run's row in the Runs table.

### Web service spawns subprocesses

`web_service.py` runs `main.py` as a subprocess for each job, passing CLI args. It does **not** import the pipeline directly. This means environment variables and CLI flags are the only way to configure job execution.

### Taxonomy file must be present

`content_taxonomy.tsv` is loaded by `OpenRouterClient` as a class variable. If this file is missing, classification will fail. It's not optional.

### iTunes API returns text/javascript

The iOS API client explicitly sets `content_type=None` when parsing JSON responses because the iTunes Search API returns `text/javascript` instead of `application/json`.

### Progress is saved per-item

`ProgressTracker.save_progress()` is called after every single item is processed, making the pipeline crash-resilient. Already-processed items are skipped on re-run.

### Asyncio mode in tests

`pytest.ini` sets `asyncio_mode = auto`, so all async test functions are automatically wrapped — no need for `@pytest.mark.asyncio` decorators.

## Testing

- **Framework**: pytest + pytest-asyncio
- **CI**: GitHub Actions (`.github/workflows/test-classification.yml`) — 3 parallel jobs (test-urls, test-apps, test-ctv) on push/PR to main/master
- **CI Python**: 3.11; uses `pip install -r requirements.txt` (not uv). The
  pytest job uses 3.12 and installs `requirements-dev.txt`. Both lock files
  are verified against 3.11 and 3.12.
- **CI runs**: `python main.py --scrape-mode direct --quiet` with 10 sample items per content type
- **Test pattern**: Set `OPENROUTER_API_KEY=test-key` via `os.environ.setdefault()` before any project imports
- **OpenRouter integration test**: Uses `pytest.mark.skipif` to skip when no real API key is available
- **CTV tests**: Comprehensive mocking of the two-step pipeline; tests platform detection, news routing, markdown stripping, research truncation

## Deployment

Production target: **Fedora/RHEL 9+**

- `scripts/bootstrap.sh` — Full installer: creates the `lens` system user, installs to `/opt/lens`, builds the venv with `uv`, writes `/opt/lens/.env` (`AUTH_SIGNING_PUBKEY` + `OPENROUTER_API_KEY`), installs the systemd units and operator CLI, and optionally sets up Caddy TLS. Lens owns no password of its own — sign-in is the shared `elcano_auth` cookie.
- `scripts/update.sh` — Staged update: git pull, build staging venv, atomic swap, health check
- `deploy/lens.service` — systemd unit; runs uvicorn on 127.0.0.1:8808; hardened with `ProtectSystem=full`, `ProtectKernelModules`, `LockPersonality` and a restricted address-family set. `NoNewPrivileges`, `ProtectControlGroups`, `ProtectKernelTunables`, `PrivateTmp` and `ProtectHome` are deliberately **absent** — each one breaks rootless podman (see the comments in the unit).
- `deploy/lens.caddy` — Caddy reverse proxy config with security headers; `{{HOSTNAME}}` placeholder substituted by bootstrap
- `deploy/nginx-lens.conf` — Nginx alternative reverse proxy config
- `deploy/lens-cli` — Operator CLI (`/usr/local/bin/lens`): `start|stop|restart|status|logs|update|rebuild|env|tls`

## CLI Flags Reference

| Flag | Description |
|---|---|
| `--quiet` | Suppress per-item output |
| `--verbose` | Show size, retries, cache status |
| `--jsonl` | Write per-item JSONL log |
| `--scrape-mode auto\|direct\|deep\|firecrawl` | Auto ladder (direct → Firecrawl → deep → research), HTTP-only, Selenium deep scrape, or local Firecrawl service |
| `--llm-model MODEL` | Override the classification model (OpenRouter ID) for this run |
| `--research-fallback on\|off` | Enable/disable the research fallback pass for this run |
| `--research-model MODEL` | Override the research-fallback model (needs built-in web search) |
| `--deep-scrape` | Shortcut for `--scrape-mode deep` |
| `--ctv` | Run CTV workflow |
| `--allow-redirects` | Follow HTTP redirects |
| `--input-csv PATH` | Custom input file (CSV or xlsx) |
| `--output-csv PATH` | Custom output CSV |
| `--progress-file PATH` | Custom progress JSON path |
| `--log-file PATH` | Custom log file path |
