# Deploying Lens

The authoritative guide to running the Lens dashboard as a service. Everything
here is verified against `scripts/bootstrap.sh`, `scripts/update.sh`,
`scripts/firecrawl.sh` and the units in `deploy/`.

You do **not** need any of this to use the CLI. `python main.py` on a laptop
with an OpenRouter key is the whole story — see the
[README quick start](../README.md#quick-start). This document is about the
long-running web dashboard.

> **Licence note.** Lens is source-available under BSL 1.1 with **no
> Additional Use Grant**, so a production deployment needs a commercial
> licence. Non-production use — evaluation, development, testing — is granted
> by the licence itself. See [LICENSING.md](LICENSING.md).

---

## Contents

- [Prerequisites](#prerequisites)
- [Install](#install)
- [What bootstrap.sh does](#what-bootstrapsh-does)
- [Layout and identity](#layout-and-identity)
- [Configuration reference](#configuration-reference)
- [TLS and reverse proxy](#tls-and-reverse-proxy)
- [The optional Firecrawl stack](#the-optional-firecrawl-stack)
- [Deep scraping (headless Chrome)](#deep-scraping-headless-chrome)
- [Service management and the operator CLI](#service-management-and-the-operator-cli)
- [Updating and rolling back](#updating-and-rolling-back)
- [Doctor and host maintenance](#doctor-and-host-maintenance)
- [Run artifacts and backup](#run-artifacts-and-backup)
- [Health checks and monitoring](#health-checks-and-monitoring)
- [Troubleshooting](#troubleshooting)

---

## Prerequisites

**Supported OS: Fedora, RHEL 9+, and RHEL rebuilds (Rocky, Alma).**
`bootstrap.sh` installs packages with `dnf` and assumes systemd, SELinux and
`firewalld` conventions. It has no apt path; on Debian/Ubuntu, run the
dashboard by hand (`uvicorn web_service:app`) or adapt the script.

| Requirement | Notes |
|---|---|
| Root access | `bootstrap.sh` refuses to run as a non-root user |
| curl | The public installer installs Git and clones main over HTTPS; no GitHub credentials needed |
| An OpenRouter API key | Every classification is an OpenRouter call. Runs cost real money — budget before pointing it at a large list. |
| Outbound HTTPS | To `openrouter.ai`, the sites being crawled, the iTunes Search API and the Google Play Store |
| 2 vCPU / 8 GB RAM | The floor the Firecrawl memory limits are tuned for. Without Firecrawl, 1 vCPU / 2 GB is enough. |
| ~10 GB disk | ~3 GB of Firecrawl images plus ~1 GB for the Selenium Chrome image, both optional |
| A public DNS record | Only if you want Caddy to obtain a Let's Encrypt certificate |

On RHEL, `podman-compose` lives in EPEL. Without it, bootstrap warns and
continues with Firecrawl disabled rather than failing.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/ElcanoTek/lens/main/install.sh | sudo bash
```

The installer function is parsed in full before it runs. In a root SSH shell, use `bash`
instead of `sudo bash`. `install.sh` refuses to replace an existing checkout:
use `lens update` for upgrades, or `/opt/lens-src/scripts/bootstrap.sh` to
reconfigure. For a reviewed/pinned install, clone the repository, check out the
desired commit, inspect its scripts and run `sudo bash scripts/bootstrap.sh`.

The installer prompts for:

1. **`AUTH_SIGNING_PUBKEY`** — the Ed25519 *public* key of the auth service
   that mints the session cookie. Required by startup validation.
2. **Authentication mode** — legacy `elcano`, or `central` with issuer URL,
   public Lens URL, client ID and client secret.
3. **OpenRouter API key** — required; input is hidden.
4. **Public hostname** — blank skips Caddy entirely.
5. **Let's Encrypt** — yes/no, plus an optional contact email.

Existing `/opt/lens/.env` values are parsed as data, and custom keys/comments
are preserved. Reconfiguration restarts the application; finish active jobs first.

### Unattended install

```bash
sudo env \
  LENS_BOOTSTRAP_NON_INTERACTIVE=1 \
  AUTH_SIGNING_PUBKEY="<base64 ed25519 public key>" \
  OPENROUTER_API_KEY="sk-or-v1-..." \
  LENS_BOOTSTRAP_HOSTNAME="lens.example.com" \
  LENS_BOOTSTRAP_SETUP_CADDY=Y \
  LENS_BOOTSTRAP_USE_LETSENCRYPT=Y \
  LENS_BOOTSTRAP_LE_EMAIL="ops@example.com" \
  bash /opt/lens-src/scripts/bootstrap.sh
```

In non-interactive mode any prompt without an environment value **and** without
a default aborts the run, except the optional hostname, contact email and TypeSafe key.
For a fresh unattended host, download `install.sh` to a private temporary file,
then pass these same variables to `bash` with that file.

### Bootstrap environment variables

| Variable | Default | Purpose |
|---|---|---|
| `LENS_BOOTSTRAP_NON_INTERACTIVE` | `0` | Never prompt; fail on a missing required value |
| `TYPESAFE_API_KEY` | — | Optional custom categorization; hidden prompt interactively, skipped if absent in unattended installs; existing `.env` value preserved |
| `LENS_BOOTSTRAP_SKIP_CHROME` | `0` | Skip the `chromium` + `chromedriver` packages |
| `LENS_BOOTSTRAP_SKIP_DEEP` | `0` | Skip podman and the Selenium Chrome image (also defaults `SKIP_FIRECRAWL` to 1) |
| `LENS_BOOTSTRAP_SKIP_FIRECRAWL` | `$LENS_BOOTSTRAP_SKIP_DEEP` | Skip `podman-compose`, the Firecrawl images and `firecrawl.service` |
| `LENS_BOOTSTRAP_HOSTNAME` | — | Public hostname for TLS; blank skips Caddy |
| `LENS_BOOTSTRAP_SETUP_CADDY` | `Y` | Install and configure Caddy (only asked when a hostname is given) |
| `LENS_BOOTSTRAP_USE_LETSENCRYPT` | `Y` | `n` uses Caddy's `tls internal` self-signed certificate instead |
| `LENS_BOOTSTRAP_LE_EMAIL` | — | ACME contact address, written into the global Caddyfile |
| `LENS_DEEP_SCRAPE_IMAGE` | `docker.io/selenium/standalone-chrome:latest` | Chrome image to pre-pull |
| `APP_DIR` | `/opt/lens` | Runtime install directory |
| `APP_USER` | `lens` | Service account |
| `LENS_SRC_DIR` | `/opt/lens-src` | Where the git checkout is seeded |

## What bootstrap.sh does

Seven steps, in order:

1. **`dnf install`** — `git curl jq python3 python3-devel gcc uv rsync openssl`,
   plus `chromium chromedriver` (unless `SKIP_CHROME`), `podman` (unless both
   deep and Firecrawl are skipped) and `podman-compose` (best-effort).
2. **User and directory** — creates the `lens` *system* user with home
   `/opt/lens` and shell `/usr/sbin/nologin`. Then the rootless-podman
   prerequisites: subuid/subgid ranges `200000-265535` (system users get none
   by default, and without them image unpacking fails), `loginctl
   enable-linger lens` so `/run/user/<uid>` survives without a login session,
   `podman system migrate` to clear any stale pause process, and a pre-pull of
   the Chrome and Firecrawl images so the first job doesn't stall on a
   multi-gigabyte download.
3. **Source sync** — seeds `/opt/lens-src` (keeping `.git`), then rsyncs it to
   `/opt/lens` with `--delete`, preserving `.git`, `.venv`, `.env`,
   `managed-files`, the legacy `data` directory during migration, and podman's
   `.local`/`.config`/`.cache`.
4. **venv** — refresh uv-managed Python to the latest patch of the minor in
   `.python-version` (3.12, covered by CI), then create a relocatable venv and
   install `requirements.txt`. This avoids following Fedora's default Python
   major before Lens dependencies have been tested on it. Downloads live under
   the service user's `.local/share/uv`; Node/npm/Go are optional host tools.
   Python refresh uses `uv python install` followed by `uv python upgrade`,
   compatible with uv 0.7.22 and current uv. Patch availability comes from uv's
   bundled catalogue: keep the distro's uv package current with `lens host update`.
5. **Configuration and state** — writes `.env`, owned `lens:lens`, mode `0600`,
   then creates `/var/lib/lens` and atomically migrates the legacy
   `/opt/lens/data/access.db` when present. A custom `LENS_ACCESS_DB` is never
   moved or rewritten.
6. **Units and CLI** — installs `lens.service`, `/usr/local/bin/lens`, and
   `firecrawl.service` when podman-compose is present.
7. **Start** — enables and starts the services, polls
   `http://127.0.0.1:8808/health` for up to 15 s and fails the installation if
   Lens never becomes ready. It then optionally installs Caddy, opens
   http/https in firewalld and waits up to 45 s for TLS.
   Finally it installs `deploy/motd` to `/etc/motd`.

Bootstrap, deploys and host-changing operations share a non-blocking lock
(`/run/lock/lens-deploy.lock`); a concurrent operation exits 75. Run bootstrap
again to fill missing configuration; use `lens env edit` to change existing values.

## Layout and identity

```
/opt/lens-src/          git checkout — the only place `lens update` fetches into
/opt/lens/              runtime install (owned by lens:lens)
  ├── .venv/            uv-built virtualenv
  ├── .env              secrets, 0640 lens:lens
  ├── managed-files/
  │   ├── inputs/       uploaded CSV/XLSX lists
  │   └── outputs/      per-job output.csv, progress.json, .log, plus _jobs.json
  ├── .local/ .config/ .cache/   rootless podman storage (holds the pulled images)
  └── (application source, rsynced from /opt/lens-src)
/var/lib/lens/          persistent application state (owned by lens:lens, 0750)
  └── access.db         central allowlist, hashed sessions and revocation state, 0600
/etc/systemd/system/lens.service
/etc/systemd/system/firecrawl.service   (optional)
/usr/local/bin/lens                     operator CLI
/etc/caddy/conf.d/lens.caddy            (optional)
/etc/motd
```

The `lens` account is a system user with no login shell. `lens.service` runs
uvicorn bound to **127.0.0.1:8808** — never directly exposed; a reverse proxy
terminates TLS in front of it.

`lens.service` is hardened, but deliberately less than you might expect.
Each of `NoNewPrivileges`, `ProtectControlGroups`, `ProtectKernelTunables`,
`PrivateTmp` and `ProtectHome` was tried and removed because each breaks a
different layer of rootless podman — the reasons are documented inline in the
unit. Do not "re-harden" it without re-testing a deep scrape.

## Configuration reference

Three layers, later winning: **built-in defaults** → `config.json` →
environment / CLI flags.

### Environment variables

Read from `/opt/lens/.env` via `EnvironmentFile=` (and from `.env` in the
working directory via python-dotenv for CLI runs).

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `OPENROUTER_API_KEY` | **Yes** | — | OpenRouter credential. `config.py` raises `ValueError` at import if unset, so the service will not start without it. |
| `AUTH_SIGNING_PUBKEY` | Yes, for the dashboard | *(empty)* | Base64 of the auth service's 32-byte Ed25519 **public** key. Only verifies signatures — it cannot mint a session, so distributing it is safe. Empty means every request redirects to login. |
| `AUTH_LOGIN_URL` | No | `https://auth.elcanotek.com` | Where unauthenticated browsers are sent. Point at your own auth service. |
| `AUTH_COOKIE_NAME` | No | `elcano_auth` | Session cookie to verify. |
| `LENS_AUTH_MODE` | No | `elcano` | `elcano` preserves legacy magic-link cookies; `central` enables confidential-client handoff and Lens-only sessions. |
| `AUTH_ISSUER_URL`, `LENS_PUBLIC_URL` | Central mode | — | Exact Auth and Lens origins. |
| `AUTH_CLIENT_ID`, `AUTH_CLIENT_SECRET` | Central mode | — | Per-deployment Auth client credentials. |
| `LENS_SESSION_SECRET` | Central mode | — | At least 32 random bytes for signed login state. |
| `LENS_ACCESS_DB` | No | `/var/lib/lens/access.db` | Persistent local access/session/revocation SQLite database. |
| `SCRAPER_VERBOSE` | No | *(unset)* | `1`/`true`/`yes`/`on` enables extra CLI diagnostics. Same as `--verbose`. |
| `PYTHONUNBUFFERED` | No | set to `1` by the unit | Keeps log lines flowing into journald. |

The Firecrawl stack reads its own variables from the optional
`firecrawl/.env`; see [that section](#the-optional-firecrawl-stack).

### config.json

Optional. Every key falls back to the default shown. The full annotated set
lives in [`config.json.example`](../config.json.example); the table below is
the same list grouped by concern.

| Key | Default | Purpose |
|---|---|---|
| `input_csv_path` | `input.csv` | CLI input list |
| `output_csv_path` | `output.csv` | CLI results file (appended) |
| `progress_file_path` | `progress.json` | Resumable state |
| `log_file_path` | `site_analysis.log` | Run log |
| `llm_model` | `~google/gemini-flash-latest` | Classification model |
| `llm_fallback_model` | `~openai/gpt-mini-latest` | Used when the primary is unavailable |
| `llm_temperature` | `0.1` | Low, for repeatable tiers |
| `llm_max_tokens` | `1500` | Truncated classifications are retried, not faked |
| `llm_request_max_retries` | `3` | Per-request retries |
| `llm_request_timeout` | `60` | Seconds per LLM call |
| `scrape_mode` | `auto` | `auto` \| `direct` \| `deep` \| `firecrawl` |
| `concurrent_sessions` | `5` | Website fetch concurrency |
| `request_timeout` | `30` | Seconds per page fetch |
| `max_retries` | `3` | Fetch retries |
| `retry_delay` | `2` | Seconds between fetch retries |
| `session_timeout` | `300` | HTTP session lifetime |
| `min_content_length` | `500` | Shortest body accepted as real content |
| `reject_redirects` | `true` | Direct pass fails off-host redirects |
| `user_agent` | current-Chrome string | An ancient UA is itself a bot signal |
| `firecrawl_enabled` | `true` | Master switch for the auto-mode Firecrawl pass |
| `firecrawl_url` | `http://127.0.0.1:3002` | Local Firecrawl API |
| `firecrawl_timeout` | `60` | Seconds per page |
| `firecrawl_wait_for` | `0` | Extra ms to wait for JS after load |
| `firecrawl_max_age_ms` | `null` | `null` uses the service's own caching |
| `firecrawl_max_concurrent` | `3` | Match `MAX_CONCURRENT_PAGES` in the compose file |
| `firecrawl_proxy` | `basic` | Keep it. `auto` escalates to a stealth proxy a self-hosted stack does not have, and just re-hammers hardened sites. |
| `firecrawl_follow_redirects` | `true` | Retry passes follow brand→parent redirects |
| `skip_deep_retry_on_block` | `true` | Don't re-try an anti-bot block from the same IP |
| `deep_scrape_podman_binary` | `podman` | Container runtime |
| `deep_scrape_image` | `docker.io/selenium/standalone-chrome:latest` | Fully qualified on purpose — Podman refuses short names without a TTY |
| `deep_scrape_container_name` | `elcano-lens-chromedriver` | Container name |
| `deep_scrape_port` | `4444` | ChromeDriver port |
| `deep_scrape_vnc_port` | `7900` | noVNC port for watching a scrape |
| `deep_scrape_extra_args` | `[]` | Extra `podman run` arguments |
| `deep_scrape_startup_timeout` | `45` | Seconds to wait for ChromeDriver |
| `deep_scrape_wait_after_load` | `1.0` | Settle time after page load |
| `deep_scrape_keep_container` | `false` | Leave the container up after a run |
| `deep_scrape_pull_policy` | `always` | Refresh Chrome every run |
| `research_fallback_enabled` | `true` | Classify blocked sites from public research instead of failing them |
| `research_model` | `perplexity/sonar-pro` | Needs built-in web search |
| `research_temperature` | `0.2` | — |
| `research_max_tokens` | `1500` | — |
| `research_max_concurrent` | `4` | — |
| `ios_request_timeout` | `30` | iTunes Search API |
| `ios_request_delay` | `5.0` | Deliberately slow; the API rate-limits hard |
| `ios_max_concurrent` | `1` | Sequential |
| `ios_max_retries` | `3` | — |
| `ios_retry_delay` | `3.0` | — |
| `android_request_timeout` | `30` | Play Store scraper |
| `android_request_delay` | `10.0` | Even slower; Play blocks fast scrapers |
| `android_max_concurrent` | `1` | Sequential |
| `android_max_retries` | `3` | — |
| `android_retry_delay` | `5.0` | — |
| `ctv_research_model` | `perplexity/sonar-pro` | CTV step 1 |
| `ctv_research_temperature` | `0.3` | — |
| `ctv_research_max_tokens` | `2000` | — |
| `ctv_classification_model` | `~google/gemini-flash-latest` | CTV step 2 |
| `ctv_classification_fallback_model` | `~openai/gpt-mini-latest` | Cross-provider fallback |
| `ctv_classification_temperature` | `0.1` | — |
| `ctv_classification_max_tokens` | `1500` | — |
| `ctv_max_concurrent` | `5` | — |
| `ctv_request_delay` | `1.0` | — |
| `ctv_max_retries` | `3` | — |
| `ctv_retry_delay` | `2.0` | — |

Keys are uppercased onto a global `config` singleton, so `llm_model` is
`config.LLM_MODEL`. Note that `config.json` is **git-ignored** — it is local
state, not part of a release.

## TLS and reverse proxy

### Caddy (recommended, and what bootstrap installs)

Give bootstrap a hostname and it does the rest: `dnf install caddy`, appends
`import conf.d/*.caddy` to `/etc/caddy/Caddyfile` if absent, writes the ACME
contact email as a global option, and renders `deploy/lens.caddy` into
`/etc/caddy/conf.d/lens.caddy` with `{{HOSTNAME}}` substituted. The site block
reverse-proxies to `127.0.0.1:8808` and sets HSTS, `X-Content-Type-Options`,
`X-Frame-Options: DENY` and a strict referrer policy.

Answer "no" to Let's Encrypt and bootstrap injects `tls internal`, giving a
Caddy-signed certificate — right for a private network or a host without
public 80/443.

Certificates renew themselves roughly 30 days before expiry. There is no cron
job to install.

```bash
lens tls status     # hostname, certificate subject/issuer/dates, Caddy state
lens tls reload     # caddy reload --config /etc/caddy/Caddyfile
lens tls restart    # systemctl restart caddy
```

Bootstrap also cross-checks DNS: if the hostname resolves somewhere other than
this host's public IP it warns rather than failing, because ACME will fail
later in a much less obvious way.

### nginx (alternative)

`deploy/nginx-lens.conf` exists for shops standardised on nginx. It is **not**
installed by bootstrap. Edit `server_name`, then:

```bash
sudo dnf install -y nginx certbot python3-certbot-nginx
sudo cp deploy/nginx-lens.conf /etc/nginx/conf.d/lens.conf
sudo nginx -t && sudo systemctl enable --now nginx
sudo certbot --nginx -d lens.example.com --agree-tos --redirect
```

`client_max_body_size 100m` must stay at or above `MAX_UPLOAD_BYTES` in
`web_service.py`, or large uploads fail at the proxy with a 413 before Lens
sees them.

If SELinux is enforcing, allow nginx to make outbound connections:

```bash
sudo setsebool -P httpd_can_network_connect 1
```

## The optional Firecrawl stack

Firecrawl is third-party software (AGPL-3.0) that Lens *orchestrates* — it
renders JavaScript through a bundled Playwright service and returns clean
markdown. It runs entirely on this host; nothing is sent to a managed service.
See [`NOTICE`](../NOTICE).

Requirements: `podman` and `podman-compose`, plus the rootless plumbing
bootstrap sets up (subuid/subgid ranges and lingering for the `lens` user).

```bash
scripts/firecrawl.sh up        # start; waits up to 180s for the API
scripts/firecrawl.sh status    # API health + container table
scripts/firecrawl.sh logs api  # tail one service (or all)
scripts/firecrawl.sh pull      # refresh images
scripts/firecrawl.sh reset     # wipe queue state and restart
scripts/firecrawl.sh down      # stop
```

On a bootstrapped host, prefer the unit. `firecrawl.sh` derives
`XDG_RUNTIME_DIR` from `id -u`, so running it as root points podman at
`/run/user/0` and finds none of the images pulled for the `lens` user; the unit
runs it as `lens` with the right `HOME` and lingering already enabled:

```bash
sudo systemctl status firecrawl.service
sudo systemctl restart firecrawl.service
sudo journalctl -u firecrawl -n 200
```

Services in `firecrawl/docker-compose.yaml`: the Firecrawl API and workers, a
Playwright rendering service, Redis, RabbitMQ and a queue Postgres. Only the
API is published, on **127.0.0.1:3002**. Database authentication is off
(`USE_DB_AUTHENTICATION=false`), which is safe precisely because the port never
leaves the host — if you change that binding you have published an
unauthenticated scraper API.

The stack tracks `latest` on purpose: queue state is disposable (all real
output lives in Lens's CSVs), so `firecrawl.sh up` self-heals a schema-breaking
upgrade by wiping that state and retrying. Two consequences worth knowing:

- Don't run `lens update` while a job is mid-flight — the stack restarts.
- If upstream ships a broken `latest`, Lens simply runs without the Firecrawl
  rung until the next good image. Jobs still complete.

Pin versions in `firecrawl/.env` (git-ignored; copy from
`firecrawl/.env.example`) when you need reproducibility: `FIRECRAWL_TAG`,
`PLAYWRIGHT_SERVICE_TAG`, `NUQ_POSTGRES_TAG`. That file also carries
`FIRECRAWL_PORT`, worker fan-out (`NUM_WORKERS_PER_QUEUE`,
`MAX_CONCURRENT_JOBS`, `CRAWL_CONCURRENT_REQUESTS`, `BROWSER_POOL_SIZE`,
`MAX_CONCURRENT_PAGES`), the queue Postgres credentials (`POSTGRES_USER` /
`POSTGRES_PASSWORD` / `POSTGRES_DB`, all defaulting to `postgres` — an
on-host-only database with no exposed port) and an optional upstream proxy
(`PROXY_SERVER`, `PROXY_USERNAME`, `PROXY_PASSWORD`).

### About hardened sites

Self-hosted Firecrawl renders from *this host's IP*. Publishers behind
Cloudflare, DataDome or PerimeterX reject datacenter IPs outright, so those
scrapes fail fast by design no matter how the stack is tuned. The only
reliable fix is a residential or ISP proxy via `PROXY_SERVER`.

Do **not** respond by raising retry counts or setting `firecrawl_proxy` to
`auto`. Repeated hits from one IP are what turn a soft block into a permanent
ban, and Lens's research fallback already classifies those domains from public
sources without touching the site.

## Deep scraping (headless Chrome)

The third rung of the ladder runs `docker.io/selenium/standalone-chrome` under
rootless podman on `127.0.0.1:4444`, with noVNC on `7900` if you want to watch.
Concurrency is forced to 1 — one Chrome session is all that is practical.

```bash
podman ps --filter name=elcano-lens-chromedriver   # is it up?
podman logs elcano-lens-chromedriver               # ChromeDriver logs
podman stop elcano-lens-chromedriver               # kill a stuck session
```

Lens starts and stops the container itself; set `deep_scrape_keep_container` to
keep it alive between runs. `deep_scrape_pull_policy` defaults to `always` so
each run gets current Chrome.

To run a host with no containers at all, install with
`LENS_BOOTSTRAP_SKIP_DEEP=1` (which also skips Firecrawl). Auto mode probes
each rung at runtime and skips what isn't there, degrading cleanly to a plain
direct crawl plus the research fallback.

## Service management and the operator CLI

Bootstrap installs `/usr/local/bin/lens`. Every subcommand shells out to
`sudo`, so an operator needs sudo rights but not a root shell.

| Command | What it does |
|---|---|
| `lens start` | `systemctl start lens.service` |
| `lens stop` | `systemctl stop lens.service` |
| `lens restart` | `systemctl restart lens.service` |
| `lens status` | `systemctl status lens.service` |
| `lens logs` | `journalctl -fu lens.service` |
| `lens logs [args…]` | Passes arguments straight to journalctl, e.g. `lens logs -n 200 --since -1h` |
| `lens update [--yes]` | Fetch, staged build, restart, health check; restore previous code and venv on failure |
| `lens doctor [--json] [--strict]` | Read-only diagnostics; exit 1 on failures (also warnings with `--strict`) |
| `lens host check` | Refresh DNF metadata and show package updates |
| `lens host update [--yes]` | Upgrade packages from enabled DNF repositories |
| `lens host tools [--yes]` | Install/update distro Node, npm and Go |
| `lens host upgrade [--yes]` | Download latest stable Fedora release upgrade; print offline reboot command |
| `lens rebuild` | Rebuild and restart the *current* checkout without fetching. The correct command after a manual rollback. |
| `lens env` | Print `/opt/lens/.env` with anything matching `TOKEN\|KEY\|SECRET\|PASSWORD\|PUBKEY` redacted |
| `lens env edit` | Open `/opt/lens/.env` in `$EDITOR` (default `vi`); follow with `lens restart` |
| `lens tls status` | Hostname, certificate subject/issuer/validity, Caddy state |
| `lens tls reload` | Reload the Caddyfile |
| `lens tls restart` | Restart Caddy |

Overrides: `LENS_SRC_DIR` (default `/opt/lens-src`) and `LENS_APP_DIR`
(default `/opt/lens`).

`lens update` honours `LENS_UPDATE_YES=1` to skip the confirmation prompt —
useful from a deploy pipeline.

## Updating and rolling back

`lens update` runs `scripts/update.sh`, which is deliberately staged so a bad
build cannot take down a working install:

1. **Fetch.** `git fetch origin`, resolve the target branch (the checked-out
   branch; if HEAD is detached, a branch pointing at HEAD, else `origin/HEAD`,
   overridable with `LENS_UPDATE_BRANCH`), print the incoming commits and ask
   for confirmation. Already up to date exits 0 only if the deployed revision
   stamp also matches — a failed previous deployment can be retried normally.
   Local source changes abort before deployment.
   The branch is fast-forwarded, never merged — a diverged branch aborts.
   If the update changed `update.sh` itself, the script re-execs the new copy
   in rebuild-only mode, because bash is still holding the old inode.
2. **Heal.** Ensure `AUTH_SIGNING_PUBKEY` is present (prompting when
   interactive) — without it every request redirects in a loop. Then ensure
   the rootless-podman prerequisites and the pre-pulled Chrome image, which
   fixes hosts bootstrapped before deep-scrape support existed.
3. **Stage.** Build a complete new tree *and* venv in
   `/opt/lens.staging.XXXXXX`. Staging next to `$APP_DIR` rather than in
   `/tmp` is deliberate: a venv built under `/tmp` keeps its SELinux `tmp_t`
   label across a move and systemd then refuses to exec it (203/EXEC). Same
   filesystem also makes the final move atomic. A failed `uv pip install`
   dies here with the live install untouched.
4. **Swap.** Stop the service, migrate a legacy access database to
   `/var/lib/lens`, then rsync the staged tree over `$APP_DIR` (preserving
   `.git`, `.venv`, `.env`, `managed-files`, legacy `data`, and podman's
   caches, `config.json`, `.env*`, `firecrawl/.env` and CLI state). Snapshot the
   actual deployed source and venv under `/opt/lens.rollback.XXXXXX` before
   stopping, then move the old venv aside and the new one in,
   `restorecon` it, reinstall the unit and CLI, refresh Firecrawl and
   `/etc/motd`, then restart.
5. **Verify.** Poll `/health` with bounded requests. On success write
   `.deployed-revision` and remove `.venv.old`. On any failure after stopping,
   automatically restore the previous source, venv, unit and CLI and probe
   readiness again. The command still exits non-zero, even if recovery succeeds.
   Retain the snapshot for inspection; prune old `/opt/lens.rollback.*`
   directories after confirming a good deployment. They exclude instance data
   and do not replace backups. Power loss/SIGKILL requires manual recovery.

Update environment variables: `LENS_UPDATE_YES`, `LENS_UPDATE_NO_PULL`,
`LENS_UPDATE_BRANCH`, `LENS_UPDATE_SKIP_DEEP`, `LENS_UPDATE_SKIP_FIRECRAWL`.

### Rolling back

**Failed deploy** — the updater attempts automatic recovery. If recovery itself
fails, use the snapshot path printed by the updater:

```bash
sudo systemctl stop lens.service
sudo bash -c 'source /opt/lens-src/scripts/lib/deploy.sh; lens_restore_release /opt/lens /opt/lens.rollback.XXXXXX /etc/systemd/system/lens.service /usr/local/bin/lens'
sudo restorecon -RF /opt/lens/.venv
sudo systemctl daemon-reload
sudo systemctl start lens.service
lens doctor
```

Application rollback does not undo DNF packages, uv-managed Python patch
updates, migrated auth state or Firecrawl image/queue changes.

**Bad code** — check out the previous commit and rebuild, without fetching:

```bash
cd /opt/lens-src
sudo git checkout <previous-sha>
sudo lens rebuild
```

Use `lens rebuild`, not `lens update`: from a detached HEAD, `update` resolves
a branch and fast-forwards straight back to the tip you were trying to leave.
The end of a successful update prints the retained snapshot path. Prefer that
snapshot when a previous failed update already advanced the source checkout.

## Doctor and host maintenance

```bash
lens doctor
lens doctor --json --strict                     # monitoring/automation exit status
lens doctor --public-url https://lens.example.com
lens host update --dry-run
lens host update --yes                          # latest packages in enabled repos
lens host tools --yes                           # optional distro Node/npm/Go
lens host upgrade --dry-run                     # current → latest stable Fedora
```

Doctor is read-only: configuration presence/key shape (no secret values), Python
minor, dependency consistency, running interpreter, service/readiness, deployed
revision, source drift, disk space, OS support deadline, rootless Podman and
installed Firecrawl. `--public-url` also checks HTTPS and certificate validation.
Missing optional containers are warnings; `--strict` makes warnings fail the check.
The Podman probe runs from the service user's application directory, so root
operators can invoke doctor from `/root` without causing a permissions false alarm.
Git checks and deployment use a command-scoped `safe.directory` for the configured
checkout; they work with legacy service-owned checkouts even when a systemd
deployment has no `HOME`, without changing global Git configuration.
It reports installed optional tool versions, not remote patch availability; use
`lens host check` for DNF updates. DNF's “updates available” exit 100 is normalized
to success, while repository failures remain errors.

For periodic monitoring, run `lens doctor --json --strict` from your scheduler
and alert on nonzero exit. For unattended application deploys, use
`lens update --yes` during a quiet window. All mutating commands log their steps
to the caller; forward stdout/stderr to your deployment system.

On SELinux-enforcing Fedora, a plain `systemd-run` starts in `initrc_t`, which
transitions rsync into the restricted `rsync_t` domain. A deployment can then
fail with `Permission denied` creating files under `/opt`, even as root and
without a visible AVC report. Running from the usual root SSH shell works.
For a systemd-managed deployment, explicitly select the privileged maintenance
context for that invocation:

```bash
sudo systemd-run --unit="lens-update-$(date +%s)" --wait --pipe \
  -p SELinuxContext=system_u:system_r:unconfined_service_t:s0 \
  /usr/local/bin/lens update --yes
```

This context is for the trusted root deployment command, not `lens.service`.
Keep SELinux enforcing and the application's existing service configuration;
do not enable global `rsync_full_access` or relabel `/opt` to work around this.
For a persistent maintenance unit on Fedora, the equivalent setting is
`SELinuxContext=system_u:system_r:unconfined_service_t:s0` in `[Service]`.

For optional user-defined categories, add `TYPESAFE_API_KEY` to `/opt/lens/.env`
with `lens env edit`, then `lens restart`. It is required only for runs that
enable custom categories; the dashboard never receives the key. See
[Custom categories](CUSTOM_CATEGORIES.md) for the UI, CLI and output contract.

### Fedora release upgrades

The helper reads `https://fedoraproject.org/releases.json`, chooses the highest
numeric stable version (never Beta or Rawhide), and refuses jumps of more than
two releases. It fails closed if the feed cannot be read. RHEL release upgrades
use vendor tooling instead. This targets conventional DNF hosts, not Atomic/ostree.

1. Finish running Lens jobs; take a provider snapshot and back up instance state
   as described below. Keep provider console access for the offline upgrade.
2. `lens host update --yes`, then reboot onto the current release's updated kernel.
3. `lens host upgrade --yes` updates packages and downloads the release transaction.
4. Execute the printed offline reboot command: `sudo dnf5 offline reboot` on
   DNF5, or `sudo dnf system-upgrade reboot` on DNF4.
5. Reconnect and run `lens rebuild`, then `lens doctor --strict` and check public HTTPS.

The helper never reboots automatically or removes conflicting packages with
`--allowerasing`. Package conflicts are reported by DNF for the operator to resolve.
`lens host tools` uses the distro's supported Node/npm pair and Go package;
it does not mix in global `npm@latest` or replace other applications' runtime pins.

## Run artifacts and backup

Everything a run produces lives under `/opt/lens/managed-files/`:

| Path | Contents |
|---|---|
| `inputs/` | Uploaded CSV/XLSX lists (≤100 MB each, `.csv`/`.xlsx` only, magic-byte checked) |
| `inputs/.breakdowns/` | Cached per-file type breakdowns for the dashboard |
| `outputs/<job>_output.csv` | Classifications: quality tier, justification, IAB tiers, language, political leaning, audience size, bot protection, scrape mode, timestamp |
| `outputs/<job>_progress.json` | Resumable per-item state and counters |
| `outputs/<job>.log` | Per-job log |
| `outputs/_jobs.json` | Queue state, reloaded on restart |

Both `bootstrap.sh` and `update.sh` exclude `managed-files` from their
`rsync --delete`, so upgrades never touch it. The central-auth database lives
outside the release tree at `/var/lib/lens/access.db`, so source deployment and
rollback cannot delete it. The legacy `/opt/lens/data` directory is also
preserved during migration; remove it only after verifying the new database.
Nothing prunes run output — it accumulates until you remove it from the
dashboard or on disk.

Back up `managed-files/outputs/` (the results you paid OpenRouter for) and
`/opt/lens/.env` (the credentials). In central mode, also back up
`/var/lib/lens/access.db`; it contains the local allowlist, hashed sessions and
revocation records and is not reproducible from git. Note that output CSVs
contain the identifiers you analysed and the access database contains client
account metadata, so treat backups accordingly.

For a simple manual backup, briefly stop Lens so SQLite and its write-ahead log
cannot change while `tar` reads them:

```bash
sudo systemctl stop lens.service
sudo tar czf lens-backup-$(date +%F).tar.gz \
  -C /opt/lens managed-files/outputs .env \
  -C /var/lib/lens access.db
sudo systemctl start lens.service
```

For automated backups without downtime, use SQLite's backup API to create a
consistent snapshot first, then upload that snapshot to private backup storage.
Do not copy a live SQLite file directly and do not use S3 as its active
filesystem.

Jobs interrupted by a restart resume where they left off: `progress.json` is
written after every single item.

## Health checks and monitoring

`/health` is public — no cookie required — which makes it usable as a
load-balancer readiness probe. Lens constructs the selected authentication
provider during startup, and the probe reuses that validated provider. Missing
central credentials, an invalid signing key, or an inaccessible access
database therefore prevents the service from advertising readiness instead of
failing on the first user request. The JSON response includes the active
`auth_mode` for operator diagnostics.

```bash
curl -fsS http://127.0.0.1:8808/health          # behind the proxy
curl -fsS https://lens.example.com/health       # through it
```

`HEAD /health` is supported too, for probes that prefer it. Both bootstrap and
update gate on this endpoint returning 200.

In central mode, logout revokes the Lens session, clears the cookie, and
redirects to `<AUTH_ISSUER_URL>/logout?client_id=<AUTH_CLIENT_ID>`. Auth ends
every central session of the account and fans a back-channel logout out to
every application, so no still-valid Auth cookie can recreate a Lens session;
the browser lands on Auth's login page. The public `/signed-out` page remains
for direct visits. Legacy mode still delegates logout to the Elcano Auth
service.

Beyond that:

```bash
lens status                                  # unit state
lens logs -n 200                             # recent output
sudo journalctl -u lens.service --since -1h -p warning
scripts/firecrawl.sh status                  # Firecrawl API + containers
```

There is no metrics endpoint. Per-job progress and counters are visible in the
dashboard and in each job's `progress.json`.

## Troubleshooting

### Every request redirects to the login page, forever

`AUTH_SIGNING_PUBKEY` is unset or malformed. `verify_session` returns `None`
for every failure mode — no key, wrong length, bad signature, expired token —
and callers treat them all as "logged out", so a missing key looks exactly
like a logged-out user.

```bash
lens env | grep AUTH_SIGNING_PUBKEY   # [REDACTED] means set; absent means not
lens env edit                          # paste the key
lens restart
```

The value must be base64 of exactly 32 raw bytes. A PEM-wrapped key, a
base64url variant or a private key will all decode to the wrong length and be
silently rejected.

### Service won't start: `OPENROUTER_API_KEY environment variable is required`

`config.py` builds its singleton at import time and raises without the key, so
this is a startup crash, not a runtime error. Check `journalctl -u lens` for
the traceback, then `lens env edit` and `lens restart`.

### 203/EXEC Permission denied after an update

An SELinux label problem on the venv. `update.sh` runs `restorecon -RF` for
this reason; if you built a venv by hand, do the same:

```bash
sudo restorecon -RF /opt/lens/.venv
sudo systemctl restart lens.service
```

### Many websites come back `Failed` with block signatures

Expected on a datacenter IP. Commercial anti-bot (Cloudflare, DataDome,
PerimeterX) rejects the whole IP range, and Lens fails those fast on purpose:
hammering escalates a soft block into a permanent ban.

Check the `Bot_Protection` column to see what actually happened —
`None Detected`, `Moderate` (plain HTTP blocked, a rendering browser got
through), `Aggressive` (everything blocked) or `Unknown` (timeouts, dead
domains — not a block at all). Rows rescued by the research pass carry
`Scrape_Mode=research`.

The fix is a residential/ISP proxy in `firecrawl/.env`, not more retries.
Stronger bot protection is also a *positive* buying signal, so `Aggressive`
rows are not necessarily bad inventory.

### Classifications truncated, or OpenRouter rate limits

Symptoms: many `Failed` rows on sites that scraped fine, or log lines about
truncated tool calls and 429s.

- Truncated classifications are retried rather than written out as a fake
  `Standard` row. Persistent truncation means `llm_max_tokens` is too low for
  your prompt — raise it.
- 429s: lower `concurrent_sessions`, and `research_max_concurrent` if the
  research pass is the noisy one. `llm_request_max_retries` already backs off.
- A dead model pin makes every request fail then silently fall back.
  `config.py` warns and substitutes the default for models known to be
  permanently gone; check the logs for that warning.
- Watch the spend. A large list against a frontier model is expensive; the
  dashboard's model picker is price-capped for exactly this reason.

### Firecrawl won't come up

```bash
scripts/firecrawl.sh status
scripts/firecrawl.sh logs api
sudo journalctl -u firecrawl -n 200
```

- `podman-compose is required` — install it (EPEL on RHEL).
- API never becomes ready: `up` already wipes and rebuilds queue state once
  automatically. If that didn't help, `scripts/firecrawl.sh reset`.
- Still broken: leave it down. Auto mode skips the rung and jobs still
  complete.

### Podman permission errors

`potentially insufficient UIDs or GIDs available` — the `lens` user has no
subuid/subgid range:

```bash
sudo usermod --add-subuids 200000-265535 lens
sudo usermod --add-subgids 200000-265535 lens
sudo loginctl enable-linger lens
sudo runuser -u lens -- env XDG_RUNTIME_DIR=/run/user/$(id -u lens) \
  HOME=/opt/lens podman system migrate
```

`XDG_RUNTIME_DIR not set` or a missing `/run/user/<uid>` means lingering is
off — `loginctl enable-linger lens`. `lens update` re-applies all of this, so
running an update is usually the fastest fix.

If containers die on restart with mount-namespace errors, a stale pause
process is pinning an old namespace; `podman system migrate` clears it.
That command stops the user's running containers. Normal updates only run it
when they add subuid/subgid mappings, so an unsuccessful staging build does not
stop an otherwise healthy Firecrawl stack.

### Staging fails to hardlink a root-owned uv cache file

Older manual Python maintenance can leave root-owned `__pycache__` files in the
service user's uv cache. Linux's protected-hardlink checks then reject staging
installs with `Operation not permitted`. The cache is disposable; clear it and
restore ownership of that cache only, then retry `lens update`:

```bash
sudo uv cache clean --cache-dir /opt/lens/.cache/uv
sudo install -d -o lens -g lens /opt/lens/.cache/uv
```

Do not recursively chown `/opt/lens/.local`: rootless container layers deliberately
contain subordinate UIDs/GIDs. Likewise, avoid recursively byte-compiling Python
under the entire service home as root; it also reaches container storage.

### Missing chromedriver / deep scrape never runs

Two separate Chromes are in play. `chromium` + `chromedriver` come from `dnf`
(skipped by `LENS_BOOTSTRAP_SKIP_CHROME=1`), while the deep-scrape pass uses
the **containerised** Selenium Chrome and needs only podman.

- `chromedriver: command not found`: `sudo dnf install -y chromium chromedriver`.
- `short-name resolution enforced but cannot prompt without a TTY`: the image
  name lost its `docker.io/` prefix. Restore the fully qualified default in
  `deep_scrape_image`.
- Nothing happens at all: `podman ps -a --filter name=elcano-lens-chromedriver`,
  then check whether the image is present for the `lens` user. First use pulls
  ~1 GB, which can exceed `deep_scrape_startup_timeout` — pre-pull it.

### Uploads fail at ~100 MB

Two independent limits: `MAX_UPLOAD_BYTES` in `web_service.py` and
`client_max_body_size` (nginx) or Caddy's default. A 413 from the proxy never
reaches Lens, so raise the proxy limit first.

### Disk filling up

Usual suspects, in order: `managed-files/outputs/` (never pruned), podman
image storage under `/opt/lens/.local/share/containers` (`deep_scrape_pull_policy:
always` accumulates image layers), `/var/lib/lens/access.db`, and journald.

```bash
sudo du -sh /opt/lens/managed-files/* /opt/lens/.local/share/containers /var/lib/lens
sudo runuser -u lens -- env XDG_RUNTIME_DIR=/run/user/$(id -u lens) \
  HOME=/opt/lens podman image prune -a
sudo journalctl --vacuum-time=14d
```

### Auth signing-key rotation

In central mode Lens verifies back-channel logout tokens with `AUTH_SIGNING_PUBKEY`
plus the keys Auth publishes at `/jwks.json` (cached 10 minutes, refreshed once when
a token names an unknown key). An Auth key rotation therefore needs no env edit
here; keep one static key so verification works even if Auth is unreachable at
startup. `AUTH_SIGNING_PREVIOUS_PUBKEYS` remains available as a manual override.
