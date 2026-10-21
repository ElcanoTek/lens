# Deploying Lens

The authoritative guide to running the Lens dashboard as a service. Everything
here is verified against `scripts/bootstrap.sh`, `scripts/update.sh`,
`scripts/lib/deploy.sh`, the operator commands (`internal/opscli`,
`internal/doctor`) and the units in `deploy/`.

You do **not** need any of this to use the CLI. `make build` and
`./bin/lens run` on a laptop with an OpenRouter key is the whole story — see the
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
- [Deep scraping (headless Chrome)](#deep-scraping-headless-chrome)
- [Service management and the operator CLI](#service-management-and-the-operator-cli)
- [Updating and rolling back](#updating-and-rolling-back)
- [Upgrading from the Python release](#upgrading-from-the-python-release)
- [Doctor and host maintenance](#doctor-and-host-maintenance)
- [Run artifacts and backup](#run-artifacts-and-backup)
- [Health checks and monitoring](#health-checks-and-monitoring)
- [Troubleshooting](#troubleshooting)

---

## Prerequisites

**Supported OS: Fedora, RHEL 9+, and RHEL rebuilds (Rocky, Alma).**
`bootstrap.sh` installs packages with `dnf` and assumes systemd, SELinux and
`firewalld` conventions. It has no apt path; on Debian/Ubuntu, build and run
the dashboard by hand (`make build && ./bin/lens serve --addr 127.0.0.1:8808`
from the Lens home) or adapt the script.

| Requirement | Notes |
|---|---|
| Root access | `bootstrap.sh` refuses to run as a non-root user |
| curl | The public installer installs Git and clones main over HTTPS; no GitHub credentials needed |
| An OpenRouter API key | Every classification is an OpenRouter call. Runs cost real money — budget before pointing it at a large list. |
| Outbound HTTPS | To `openrouter.ai`, the sites being crawled, the iTunes Search API and the Google Play Store; at build time (install and every update) to the Go module proxy and checksum database (`proxy.golang.org`, `sum.golang.org`), which also serve the pinned Go toolchain |
| 1 vCPU / 2 GB RAM | Enough for the direct crawl and research fallback. The deep crawler's Chrome container wants ~2 GB more RAM. |
| ~2 GB free disk | For the Selenium Chrome image (optional, deep crawl only), the Go build cache (`/var/cache/lens-build`, a few hundred MB) and whatever run output you keep |
| A public DNS record | Only if you want Caddy to obtain a Let's Encrypt certificate |

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
a default aborts the run, except the optional hostname, contact email and TypeSafe fallback key.
For a fresh unattended host, download `install.sh` to a private temporary file,
then pass these same variables to `bash` with that file.

### Bootstrap environment variables

| Variable | Default | Purpose |
|---|---|---|
| `LENS_BOOTSTRAP_NON_INTERACTIVE` | `0` | Never prompt; fail on a missing required value |
| `TYPESAFE_API_KEY` | — | Optional direct fallback for Jev custom categories when OpenRouter fails; hidden prompt interactively, skipped if absent in unattended installs; existing `.env` value preserved |
| `LENS_BOOTSTRAP_SKIP_CHROME` | `0` | Skip the `chromium` + `chromedriver` packages |
| `LENS_BOOTSTRAP_SKIP_DEEP` | `0` | Skip podman and the Selenium Chrome image |
| `LENS_BOOTSTRAP_HOSTNAME` | — | Public hostname for TLS; blank skips Caddy |
| `LENS_SET_HOSTNAME` | — | `1` renames the box to the public hostname whatever its current name; `0` never renames. Unset renames only from `vultr.guest`/`localhost`/`localhost.localdomain` |
| `LENS_BOOTSTRAP_SETUP_CADDY` | `Y` | Install and configure Caddy (only asked when a hostname is given) |
| `LENS_BOOTSTRAP_USE_LETSENCRYPT` | `Y` | `n` uses Caddy's `tls internal` self-signed certificate instead |
| `LENS_BOOTSTRAP_LE_EMAIL` | — | ACME contact address, written into the global Caddyfile |
| `LENS_DEEP_SCRAPE_IMAGE` | `docker.io/selenium/standalone-chrome:latest` | Chrome image to pre-pull |
| `APP_DIR` | `/opt/lens` | Runtime install directory |
| `APP_USER` | `lens` | Service account |
| `LENS_SRC_DIR` | `/opt/lens-src` | Where the git checkout is seeded |

## What bootstrap.sh does

Seven steps, in order:

1. **`dnf install`** — `git curl jq golang make rsync openssl util-linux
   ca-certificates`, plus `chromium chromedriver` (unless `SKIP_CHROME`), and
   `podman` (unless `SKIP_DEEP`). Any distro Go ≥ 1.21 is enough: the build
   runs with `GOTOOLCHAIN=auto`, so Go downloads the exact release `go.mod`
   pins (see step 3). Then, when `cockpit-ws` is installed, it removes Cockpit with
   `dnf -y remove 'cockpit*'`: the Vultr Fedora image ships Cockpit's
   root-capable web login on :9090, and Lens hosts are administered over ssh
   only. `LENS_KEEP_COCKPIT=1` keeps it; a failed removal only warns.
   Next it makes SSH key-only: Vultr's cloud-init leaves
   `PasswordAuthentication yes` in `50-cloud-init.conf`, so bootstrap writes
   `/etc/ssh/sshd_config.d/01-no-password-login.conf` (passwords off, root by
   key only; it sorts first, and sshd keeps the first value it reads), checks
   it with `sshd -t` and reloads sshd. It is the safety net for a box created
   without the `elcano-key-only-ssh` startup script. It skips with a warning
   when root and `/home/*` have no authorized key (that would lock everyone
   out), does nothing when the file is already in place, removes the file
   again if `sshd -t` rejects it, and never fails the install. It also skips
   when `sshd -T` shows public-key login off.
   `LENS_KEEP_SSH_PASSWORDS=1` skips it.
2. **User, directory and source** — creates the `lens` *system* user with home
   `/opt/lens` and shell `/usr/sbin/nologin`. Then the rootless-podman
   prerequisites: subuid/subgid ranges `200000-265535` (system users get none
   by default, and without them image unpacking fails), `loginctl
   enable-linger lens` so `/run/user/<uid>` survives without a login session,
   `podman system migrate` to clear any stale pause process, and a pre-pull of
   the Chrome image so the first deep crawl doesn't stall on a large
   download. Finally it seeds `/opt/lens-src` (keeping `.git`) and rsyncs it to
   `/opt/lens` with `--delete`, preserving `.git`, `.env*`, `config.json`,
   `managed-files`, the legacy `data` directory during migration, a
   Python-era `.venv`, CLI run files, and podman's `.local`/`.config`/`.cache`.
3. **Build** — `make build` in `/opt/lens` emits `/opt/lens/bin/lens`, stamped
   with the checkout's commit (`scripts/lib/deploy.sh`, `lens_build`). The
   build runs with `GOTOOLCHAIN=auto` and `GOENV=off`, and keeps GOPATH, the
   module cache, the build cache and any downloaded toolchain in the
   root-owned `/var/cache/lens-build`, so it neither litters `/root` nor
   depends on `HOME`, and reruns reuse what was downloaded. The binary is
   static (`CGO_ENABLED=0`) and carries the dashboard assets and taxonomy.
   `bin/` is owned by root: root runs this binary for every operator command,
   so the service account must not be able to rewrite it.
4. **Configuration and state** — writes `.env` with `lens env set` (values
   travel through the environment, never argv), owned `lens:lens`, mode
   `0600`, then creates `/var/lib/lens` and atomically migrates the legacy
   `/opt/lens/data/access.db` when present (`lens migrate-access-db`). A
   custom `LENS_ACCESS_DB` is never moved or rewritten.
5. **Unit and CLI** — installs `lens.service` and installs `/usr/local/bin/lens`
   as a root-owned copy of the binary (replacing the copied bash script a
   Python release installed there). It is a copy, not a link into `/opt/lens`,
   because that tree belongs to the service user and root runs the CLI.
6. **Start** — enables and starts the service, polls
   `http://127.0.0.1:8808/health` for up to 15 s and, if Lens never becomes
   ready, stops it and fails the installation. Once it is ready, a Python-era
   `.venv` left in `/opt/lens` is removed.
7. **TLS** — optionally installs Caddy, opens http/https in firewalld and
   waits up to 45 s for TLS. Finally it installs `deploy/motd` to `/etc/motd`.

Given a public hostname (step 4), bootstrap also names the box after it, so
prompts stop reading `root@vultr`. It renames only when the current static
hostname is an unset cloud-image default (`vultr.guest`, `localhost`,
`localhost.localdomain`); any other name, short ones included, is left alone and
bootstrap prints the `hostnamectl set-hostname <fqdn>` to run.
`LENS_SET_HOSTNAME=1` forces the rename, `LENS_SET_HOSTNAME=0` skips the step.
A rename runs `hostnamectl set-hostname`, writes
`/etc/cloud/cloud.cfg.d/99-preserve-hostname.cfg` (`preserve_hostname: true`,
`manage_etc_hosts: false`) when `/etc/cloud` exists, and replaces the old hostname on the `127.0.0.1`/`::1` lines in `/etc/hosts`
with `<fqdn> <short>`, keeping every other alias on the line (localhost names
are never replaced). When no line names the FQDN, it appends both loopback lines. It is idempotent, never fails the
install, and `lens update` never renames a box (`scripts/lib/hostname.sh`).

Bootstrap, deploys and host-changing operations share a non-blocking lock
(`/run/lock/lens-deploy.lock`); a concurrent operation exits 75. Run bootstrap
again to fill missing configuration; use `lens env edit` to change existing values.

## Layout and identity

```
/opt/lens-src/          git checkout — the only place `lens update` fetches into
/opt/lens/              runtime install and Lens home (owned by lens:lens)
  ├── bin/lens          the release: one static binary (owned by root)
  ├── .deployed-revision   commit the running release was built from
  ├── .env              secrets, 0600 lens:lens
  ├── managed-files/
  │   ├── inputs/       uploaded CSV/XLSX lists
  │   ├── outputs/      per-job output.csv, progress.json, .log, plus _jobs.json
  │   └── spool/        lens submit hand-off (0700)
  ├── .local/ .config/ .cache/   rootless podman storage (holds the pulled images)
  └── (release tree, rsynced from /opt/lens-src; the binary needs none of it)
/var/lib/lens/          persistent application state (owned by lens:lens, 0750)
  ├── access.db         central allowlist, hashed sessions and revocation state, 0600
  └── api_tokens.db     API token hashes and the download-link key (once minted)
/var/cache/lens-build/  root-owned Go module/build cache and toolchain (disposable)
/etc/systemd/system/lens.service
/usr/local/bin/lens                     operator CLI: root-owned copy of bin/lens
/etc/caddy/Caddyfile.d/lens.caddyfile   (optional; or conf.d/lens.caddy)
/etc/motd
```

The `lens` account is a system user with no login shell. `lens.service` runs
`/opt/lens/bin/lens serve --addr 127.0.0.1:8808` with `WorkingDirectory=/opt/lens`
(the Lens home: `.env`, `config.json`, `managed-files/`) — never directly
exposed; a reverse proxy terminates TLS in front of it. Each dashboard job is a
`lens run` subprocess of the same binary, run as `lens` in that directory.

`lens.service` is hardened, but deliberately less than you might expect.
Each of `NoNewPrivileges`, `ProtectControlGroups`, `ProtectKernelTunables`,
`PrivateTmp` and `ProtectHome` was tried and removed because each breaks a
different layer of rootless podman — the reasons are documented inline in the
unit. Do not "re-harden" it without re-testing a deep scrape.

## Configuration reference

Three layers, later winning: **built-in defaults** → `config.json` →
environment / CLI flags.

### Environment variables

Read from `/opt/lens/.env` via `EnvironmentFile=`, and by every `lens`
process from the nearest `.env` in its working directory or a parent
(`internal/dotenv`, a port of python-dotenv, so the grammar is unchanged);
variables already set win. Jobs run as `lens` and load `.env` themselves, so the
service user must be able to read it.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `OPENROUTER_API_KEY` | **Yes** | — | OpenRouter credential. `lens run` refuses to start without it, so every dashboard job fails while it is unset (the dashboard itself still starts; `lens doctor` reports it). |
| `AUTH_SIGNING_PUBKEY` | Yes, for the dashboard | *(empty)* | Base64 of the auth service's 32-byte Ed25519 **public** key. Only verifies signatures — it cannot mint a session, so distributing it is safe. Empty means every request redirects to login. |
| `AUTH_LOGIN_URL` | No | `https://auth.elcanotek.com` | Where unauthenticated browsers are sent. Point at your own auth service. |
| `AUTH_COOKIE_NAME` | No | `elcano_auth` | Session cookie to verify. |
| `LENS_AUTH_MODE` | No | `elcano` | `elcano` preserves legacy magic-link cookies; `central` enables confidential-client handoff and Lens-only sessions. |
| `AUTH_ISSUER_URL`, `LENS_PUBLIC_URL` | Central mode | — | Exact Auth and Lens origins. |
| `AUTH_CLIENT_ID`, `AUTH_CLIENT_SECRET` | Central mode | — | Per-deployment Auth client credentials. |
| `LENS_SESSION_SECRET` | Central mode | — | At least 32 random bytes for signed login state. |
| `LENS_ACCESS_DB` | No | `/var/lib/lens/access.db` | Persistent local access/session/revocation SQLite database. |
| `LENS_MAX_PARALLEL_JOBS` | No | `1` | Dashboard jobs run at once (1–8). Website jobs each run their own crawler pool, so 2–3 suits a small host. |
| `SCRAPER_VERBOSE` | No | *(unset)* | `1`/`true`/`yes`/`on` enables extra CLI diagnostics. Same as `--verbose`. |
| `LENS_HOME` | No | working directory | Lens home for `lens serve` (`.env`, `config.json`, `managed-files/`). The unit relies on `WorkingDirectory=/opt/lens` instead. |

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
| `llm_model` | `~openai/gpt-luna-latest` | Classification model. Tracks the current GPT Luna release. |
| `llm_fallback_model` | `~openai/gpt-mini-latest` | Used when the primary is unavailable |
| `llm_temperature` | `0.1` | Low, for repeatable tiers |
| `llm_max_tokens` | `1500` | Truncated classifications are retried, not faked |
| `llm_request_max_retries` | `3` | Per-request retries |
| `llm_request_timeout` | `60` | Seconds per LLM call |
| `scrape_mode` | `auto` | `auto` (direct → deep → research) \| `direct` \| `deep`. A legacy `firecrawl` value runs as `auto`, with a warning. |
| `concurrent_sessions` | `5` | Website fetch concurrency |
| `request_timeout` | `30` | Seconds per page fetch |
| `max_retries` | `3` | Fetch retries |
| `retry_delay` | `2` | Seconds between fetch retries |
| `session_timeout` | `300` | HTTP session lifetime |
| `min_content_length` | `500` | Shortest body accepted as real content |
| `reject_redirects` | `true` | Direct pass fails off-host redirects (a domain ↔ its own subdomains is not off-host) |
| `user_agent` | current-Chrome string | An ancient UA is itself a bot signal |
| `skip_deep_retry_on_block` | `true` | Deep pass skips sites the direct pass proved are blocked (401/403/429 or a block page) rather than retrying from the same IP |
| `deep_retry_enabled` | `true` | Run the deep retry pass at all. Per run: dashboard "Retry with headless Chrome", `lens submit --no-deep`, `lens run --deep-retry off` |
| `retry_follow_redirects` | `true` | Deep retry pass follows cross-host brand→parent redirects (e.g. `foxnewsdigital.com` → `foxnews.com`) that the direct pass rejects. A legacy `firecrawl_follow_redirects` is honoured when this is unset. |
| `deep_scrape_podman_binary` | `podman` | Container runtime |
| `deep_scrape_image` | `docker.io/selenium/standalone-chrome:latest` | Fully qualified on purpose — Podman refuses short names without a TTY |
| `deep_scrape_container_name` | `elcano-lens-chromedriver` | Container name prefix; each run uses `<name>-<pid>` on free ports so parallel jobs never share a browser |
| `deep_scrape_port` | `4444` | ChromeDriver port, used only with `deep_scrape_keep_container` |
| `deep_scrape_vnc_port` | `7900` | noVNC port for watching a scrape |
| `deep_scrape_extra_args` | `[]` | Extra `podman run` arguments |
| `deep_scrape_startup_timeout` | `45` | Seconds to wait for ChromeDriver |
| `deep_scrape_wait_after_load` | `1.0` | Settle time after page load |
| `deep_scrape_keep_container` | `false` | Leave the container up after a run and reuse it by its fixed name and port (one job at a time) |
| `deep_scrape_pull_policy` | `always` | Refresh Chrome every run |
| `research_fallback_enabled` | `true` | Classify blocked sites from public research instead of failing them |
| `research_model` | `perplexity/sonar` | Needs built-in web search. `perplexity/sonar-pro` is also supported. |
| `research_temperature` | `0.2` | — |
| `research_max_tokens` | `1500` | — |
| `research_max_concurrent` | `12` | Concurrent research-fallback requests; API-bound, independent of `concurrent_sessions` |
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
| `ctv_research_model` | `perplexity/sonar` | CTV step 1 |
| `ctv_research_temperature` | `0.3` | — |
| `ctv_research_max_tokens` | `2000` | — |
| `ctv_classification_model` | `~openai/gpt-luna-latest` | CTV step 2. Same moving alias as `llm_model`. |
| `ctv_classification_fallback_model` | `~openai/gpt-mini-latest` | Cross-provider fallback |
| `ctv_classification_temperature` | `0.1` | — |
| `ctv_classification_max_tokens` | `1500` | — |
| `ctv_max_concurrent` | `5` | — |
| `ctv_request_delay` | `1.0` | — |
| `ctv_max_retries` | `3` | — |
| `ctv_retry_delay` | `2.0` | — |

`lens serve` reads `/opt/lens/config.json` at start, and every job reads it
again when it starts, so a change applies to the next job (restart for the
dashboard's own defaults). Note that `config.json` is **git-ignored** — it is local
state, not part of a release. Old `firecrawl_*` keys left in a `config.json`
are ignored.

## TLS and reverse proxy

### Caddy (recommended, and what bootstrap installs)

Give bootstrap a hostname and it does the rest: `dnf install caddy`, writes the
ACME contact email as a global option, and renders `deploy/lens.caddy` with
`{{HOSTNAME}}` substituted into a directory `/etc/caddy/Caddyfile` already
imports. On stock Fedora that is `/etc/caddy/Caddyfile.d/lens.caddyfile`; when
no usable import exists, bootstrap appends `import conf.d/*.caddy` and writes
`/etc/caddy/conf.d/lens.caddy`. Before opening the firewall it checks that the
configuration validates and that Caddy actually loads a Lens proxy for the
hostname, and it removes its own copies left in a directory that is no longer
imported (files without the first-line marker are never touched). The site block
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

`client_max_body_size 100m` must stay at or above Lens's 100 MB upload limit
(`MaxUploadBytes` in `internal/jobs/files.go`), or large uploads fail at the proxy with a 413 before Lens
sees them.

If SELinux is enforcing, allow nginx to make outbound connections:

```bash
sudo setsebool -P httpd_can_network_connect 1
```

## Deep scraping (headless Chrome)

The second rung of the ladder (direct → deep → research) runs `docker.io/selenium/standalone-chrome` under
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
`LENS_BOOTSTRAP_SKIP_DEEP=1`. Auto mode probes the deep rung at runtime and
skips it when Podman isn't there, degrading cleanly to a plain
direct crawl plus the research fallback.

Sites the direct pass proved are blocked by anti-bot (401/403/429 or a block
page) skip this rung (`skip_deep_retry_on_block`): Chrome renders from the
same datacenter IP and would fail identically, and repeated hits from one IP
are what turn a soft block into a permanent ban. They go straight to the
research fallback, which classifies them from public sources without
contacting the site from this host.

## Service management and the operator CLI

Bootstrap and every update install `/usr/local/bin/lens` as a root-owned copy
of the release binary, so the operator CLI and the service are always the same
build (`lens --version` prints it). It is never a link into `/opt/lens`: that
tree belongs to the service user, and root runs the CLI.
Commands that need root re-run themselves under `sudo`; `lens token`,
`lens access` and the second half of `lens submit` re-run as the service user.
An operator needs sudo rights but not a root shell. `lens --help` lists every
command.

| Command | What it does |
|---|---|
| `lens start` | `systemctl start lens.service` |
| `lens stop` | `systemctl stop lens.service` |
| `lens restart` | `systemctl restart lens.service` |
| `lens status` | `systemctl status lens.service` |
| `lens logs` | `journalctl -fu lens.service` |
| `lens logs [args…]` | Passes arguments straight to journalctl, e.g. `lens logs -n 200 --since -1h` |
| `lens update [--yes]` | Fetch, staged Go build, restart, health check; restore the previous release on failure |
| `lens doctor [--json] [--strict] [--public-url https://HOST]` | Read-only diagnostics; exit 1 on failures (also warnings with `--strict`) |
| `lens host check` | Refresh DNF metadata and show package updates |
| `lens host update [--yes]` | Upgrade packages from enabled DNF repositories |
| `lens host tools [--yes]` | Install/update distro Node, npm and Go |
| `lens host upgrade [--yes]` | Download latest stable Fedora release upgrade; print offline reboot command |
| `lens rebuild` | Rebuild and restart the *current* checkout without fetching. The correct command after a manual rollback. |
| `lens env [show]` | Print `/opt/lens/.env` with anything matching `TOKEN\|KEY\|SECRET\|PASSWORD\|PUBKEY` redacted |
| `lens env edit` | Open `/opt/lens/.env` in `$EDITOR` (default `vi`); follow with `lens restart` |
| `lens submit FILE [options]` | Queue a dashboard run from a file on the host (see below) |
| `lens access grant\|revoke <email>`, `lens access list` | Central-auth allowlist on this instance |
| `lens token create\|list\|revoke` | API tokens (see below) |
| `lens --version` | Build version and full commit of this binary |
| `lens tls status` | Hostname, certificate subject/issuer/validity, Caddy state |
| `lens tls reload` | Reload the Caddyfile |
| `lens tls restart` | Restart Caddy |

Overrides: `LENS_SRC_DIR` (default `/opt/lens-src`), `LENS_APP_DIR`
(default `/opt/lens`) and `LENS_APP_USER` (default `lens`); they are carried
across the sudo re-run.

`lens update` honours `LENS_UPDATE_YES=1` to skip the confirmation prompt —
useful from a deploy pipeline.

### Submitting runs from the host

`lens submit` queues a run exactly as if the file had been uploaded in the
dashboard, so it shows in the Runs table with live progress, cost and
downloads, and the input lands in the file library:

```bash
lens submit /tmp/sites.csv --name "Q4 sites" --categories /tmp/categories.json
lens submit /tmp/apps.csv --ctv --decision-model '~typesafe/jev-latest'
```

Options: `--name`, `--categories JSON` (same format as the CLI's
`--custom-categories`), `--ctv`, `--llm-model`, `--research-model`,
`--decision-model`, `--no-research-fallback`, `--no-deep` (skip the
headless-Chrome retry; much faster on large lists), `--timeout SECONDS` (how
long to wait for the service, default 30). It prints the job id once the
service has queued it.

The hand-off goes through `/opt/lens/managed-files/spool/` (mode 0700, owned by
the service user): the CLI copies the file in as root, then validation and the
request run as the service user. The service checks the spool every two
seconds and applies the same file checks as an upload. If the service is down,
the request waits in the spool and is queued when it starts. Jobs run one at a
time unless `LENS_MAX_PARALLEL_JOBS` allows more.

### API tokens for Fleet and other services

Programs drive Lens through `POST /mcp` (MCP, which is what Fleet uses) and
`/api/v1` (REST). Both need a scoped bearer token that you mint on the host:

```bash
lens token create --label fleet-elcano     # prints the token once, plus Fleet's .env lines
lens token list
lens token revoke ID
```

Tokens carry `jobs:read` and/or `jobs:run` and never admin rights. Only their
hashes are stored, in `/var/lib/lens/api_tokens.db` (`LENS_API_TOKEN_DB`).
Set `LENS_PUBLIC_URL` so download links name the public host. The contract
is in [API.md](API.md).

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
   If the update changed `update.sh` or `scripts/lib/deploy.sh`, the script
   re-execs the new copy in rebuild-only mode, because bash is still holding
   the old inode.
2. **Heal.** Ensure `AUTH_SIGNING_PUBKEY` is present (prompting when
   interactive) — without it every request redirects in a loop. Then ensure
   the rootless-podman prerequisites and the pre-pulled Chrome image, which
   fixes hosts bootstrapped before deep-scrape support existed.
3. **Stage.** Install `golang` and `make` from dnf if either is missing, copy
   the release tree into `/opt/lens.staging.XXXXXX` and `make build` there,
   with the same `GOTOOLCHAIN=auto` and `/var/cache/lens-build` caches as
   bootstrap. The staged `bin/lens --version` must report the target commit.
   Staging next to `$APP_DIR` rather than in `/tmp` is deliberate: a tree
   built under `/tmp` keeps its SELinux `tmp_t` label across a move and
   systemd then refuses to exec it (203/EXEC). A failed build or toolchain
   download dies here with the live install untouched.
4. **Swap.** Snapshot the deployed release under `/opt/lens.rollback.XXXXXX`
   — the release tree with its `bin/lens`, a Python-era `.venv` if there is
   one, the unit and the operator CLI — then stop the service, migrate a
   legacy access database to `/var/lib/lens` (`lens migrate-access-db`, run
   from the staged binary), and rsync the staged tree over `$APP_DIR`
   (preserving `.git`, `.venv`, `.env*`, `config.json`, `managed-files`,
   legacy `data`, podman's caches and CLI run files). `restorecon` the new
   `bin/`, reinstall the unit, install `/usr/local/bin/lens` as a root-owned copy of the new binary,
   retire the old Firecrawl stack if present (see below), refresh `/etc/motd`,
   then restart.
5. **Verify.** Poll `/health` with bounded requests. On success write
   `.deployed-revision` and remove a leftover Python `.venv` / `.venv.old`.
   On any failure after stopping, automatically restore the snapshot (code,
   binary, venv if any, unit and CLI) and probe readiness again. The command
   still exits non-zero, even if recovery succeeds.
   After a successful update only the newest two `/opt/lens.rollback.*`
   snapshots are kept (`LENS_KEEP_ROLLBACKS` changes the count); a failed
   update prunes nothing. Snapshots exclude instance data and do not replace
   backups. Power loss/SIGKILL requires manual recovery.

Update environment variables: `LENS_UPDATE_YES`, `LENS_UPDATE_NO_PULL`,
`LENS_UPDATE_BRANCH`, `LENS_UPDATE_SKIP_DEEP`, `LENS_KEEP_ROLLBACKS`.
The build honours `GOTOOLCHAIN` (default `auto`), `GOPROXY`, `GOSUMDB` and
`LENS_BUILD_CACHE` (default `/var/cache/lens-build`) from the environment.

### Retiring the old Firecrawl stack

Earlier releases ran a self-hosted Firecrawl stack as an extra crawl rung. It
has been removed, and `lens update` cleans it off existing hosts automatically:
it disables and deletes `firecrawl.service`, removes the `elcano-firecrawl`
containers and volumes, removes the Firecrawl images (plus `redis:alpine`,
`rabbitmq:3-alpine` and dangling layers — typically many GB) and deletes
`/opt/lens/firecrawl`. The deep crawler's Chrome image is kept. The step is
idempotent, and `lens doctor` warns while `firecrawl.service` is still loaded.

### Rolling back

**Failed deploy** — the updater attempts automatic recovery. If recovery itself
fails, use the snapshot path printed by the updater:

```bash
sudo systemctl stop lens.service
sudo bash -c 'source /opt/lens-src/scripts/lib/deploy.sh; lens_restore_release /opt/lens /opt/lens.rollback.XXXXXX /etc/systemd/system/lens.service /usr/local/bin/lens'
sudo restorecon -RF /opt/lens/bin    # and /opt/lens/.venv when restoring a Python release
sudo systemctl daemon-reload
sudo systemctl start lens.service
lens doctor
```

`lens_restore_release` puts back the snapshot's tree, its `bin/lens`, its
Python `.venv` when it has one, the unit and whatever `/usr/local/bin/lens`
was (a copy of the binary, or the Python release's copied bash script; a
symlink left by an early Go build is restored as a copy).
Application rollback does not undo DNF packages, the Go toolchain or module
cache in `/var/cache/lens-build`, migrated auth state or the removal of the
old Firecrawl stack.

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
Going back across the Go rewrite this way — checking out a Python-era commit
and rebuilding — is not supported: that commit's `update.sh` expects uv and the
Python runtime the Go release no longer has. Restore the Python release from a
rollback snapshot that still holds its `.venv` instead (above).

## Upgrading from the Python release

Hosts installed before the Go rewrite move to it with a normal update — no
reinstall, no manual steps:

```bash
lens update --yes
```

On such a host `lens` is still the Python release's bash CLI, which runs the
*old* `scripts/update.sh`. That script fetches and fast-forwards as usual,
sees that `update.sh` changed and re-execs the new one in rebuild-only mode
(`LENS_UPDATE_NO_PULL=1`, with the deploy lock still held). From there the
Go `update.sh` runs on its own:

- It installs `golang` and `make` with dnf (a Python host has neither) and
  builds `bin/lens` in staging; `GOTOOLCHAIN=auto` downloads the toolchain
  `go.mod` pins into `/var/cache/lens-build`, so an older distro Go is fine.
  The first build fetches the toolchain and modules and takes a few minutes;
  later ones reuse the cache.
- The rollback snapshot includes the Python `.venv` (about 250 MB) along with
  the code, the old unit and the old bash CLI.
- The swap leaves `.venv` in place, installs the new unit
  (`ExecStart=/opt/lens/bin/lens serve …`) and replaces the copied
  `/usr/local/bin/lens` script with a root-owned copy of the binary.
- **Only after the Go release answers `/health`** is `.venv` (and any
  `.venv.old`) removed. If the health check fails, rollback restores the
  Python release — code, venv, unit and bash CLI — and starts it again, and
  `lens update` exits non-zero. `lens logs` shows why the Go release did not
  come up; fix that and run `lens update` again (it rebuilds the already
  fetched commit). The restored Python CLI's `lens doctor` does not work in
  this state — it runs `scripts/doctor.sh` from `/opt/lens-src`, which is
  already on the Go release.

Everything instance-specific carries over unchanged: `.env`, `config.json`,
`managed-files/` (job history, inputs, outputs, `_jobs.json`),
`/var/lib/lens/access.db` and `api_tokens.db` (same schemas, so central
sessions, the allowlist and Fleet's tokens keep working), and the podman
storage with the Chrome image. Queued jobs stay queued. As with any update,
the service is stopped for the swap, so let running jobs finish first: a job
running at the stop is marked failed ("Interrupted by server restart").

Afterwards:

```bash
lens --version          # Go build version and commit
lens doctor --strict    # binary, assets, release, python-venv: no leftovers
```

The Python release's uv cache under `/opt/lens/.cache/uv` and uv-managed
Pythons under `/opt/lens/.local/share/uv` are no longer used; they can be
removed once you no longer want to roll back to a Python snapshot. Leave the
rest of `.local`, `.config` and `.cache` alone — podman's storage lives there.
Once the snapshot holding the venv ages out (`LENS_KEEP_ROLLBACKS`, two
updates), the Python release can no longer be restored automatically.

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

Doctor is read-only and bounded (every command it runs has a timeout): required
tools and the Go toolchain `lens update` builds with, configuration
presence/key shape (no secret values), the access and token databases, the
release binary (present, runs, and reports the commit in `.deployed-revision`),
the dashboard assets embedded in it, service/readiness, whether the running
process is the installed `bin/lens`, deployed revision, source drift, a
retired Python `.venv` still on disk, `managed-files/` ownership, the job
queue file, disk space, OS support deadline, rootless Podman and the Chrome
image, Cockpit, Caddy, and a leftover `firecrawl.service` (run `lens update`
to retire it). Run as
root, it also warns when `sshd -T` reports `passwordauthentication yes` or
`permitrootlogin yes` (see the production-support README, Access: SSH keys).
`env-permissions` only checks that `.env` is mode 0600 or 0640. `env-readable` checks that the service user can open it. A root-owned
0600 file passes the mode check, the dashboard still starts (systemd reads
EnvironmentFile as root), and every job crashes because the analyzer loads
`.env` as `lens`. `lens env edit` puts ownership back afterwards. `--public-url` also checks HTTPS and certificate validation.
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

Custom categories need no extra setup: they run on `OPENROUTER_API_KEY`. To give
Jev requests a direct fallback when OpenRouter's Decisions API is unavailable,
add `TYPESAFE_API_KEY` to `/opt/lens/.env` with `lens env edit`, then
`lens restart`; the dashboard never receives the key. See
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
`/var/lib/lens/access.db`; it contains the local allowlist, Auth provisioning
versions, hashed sessions, and revocation records and is not reproducible from
git. If you have minted API tokens, also back up `/var/lib/lens/api_tokens.db`:
it holds the token hashes and the download-link signing key, and restoring a
host without it invalidates every Fleet or script integration until new tokens
are minted and handed out. Note that output CSVs
contain the identifiers you analysed and the access database contains client
account metadata, so treat backups accordingly.

For a simple manual backup, briefly stop Lens so SQLite and its write-ahead log
cannot change while `tar` reads them:

```bash
sudo systemctl stop lens.service
sudo tar czf lens-backup-$(date +%F).tar.gz \
  -C /opt/lens managed-files/outputs .env \
  -C /var/lib/lens access.db api_tokens.db   # drop api_tokens.db if no token was ever minted
sudo systemctl start lens.service
```

For automated backups without downtime, use SQLite's backup API to create a
consistent snapshot first, then upload that snapshot to private backup storage.
Do not copy a live SQLite file directly and do not use S3 as its active
filesystem.

A dashboard job running when the service stops is marked failed
("Interrupted by server restart") on the next start; queued jobs stay queued.
Its `progress.json` and output CSV hold every item finished before the stop
(`progress.json` is written after every single item). A CLI run (`lens run`)
interrupted the same way resumes where it left off when re-run with the same
output and progress paths.

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

Auth uses the same public back-channel endpoint for durable application-access
events. Lens verifies the Ed25519 signature, issuer, audience, subject, and
monotonic per-subject version before enabling or disabling the local allowlist
entry. A revoke also ends active Lens sessions. Older/replayed events are
ignored, and Auth keeps retrying its latest desired state until Lens
acknowledges it, so access converges after an outage.

Beyond that:

```bash
lens status                                  # unit state
lens logs -n 200                             # recent output
sudo journalctl -u lens.service --since -1h -p warning
```

There is no metrics endpoint. Per-job progress and counters are visible in the
dashboard and in each job's `progress.json`.

## Troubleshooting

### Every request redirects to the login page, forever

`AUTH_SIGNING_PUBKEY` is unset or malformed. The cookie verifier
(`internal/auth`) reports one "not signed in" for every failure mode — no key, wrong length, bad signature, expired token —
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

### Every job fails: `OPENROUTER_API_KEY environment variable is required`

The dashboard starts without the key, but each job is a `lens run` that refuses
to run without it, so every run fails at once with this line in its log.
`lens doctor` reports the missing key under `configuration`. Set it with
`lens env edit`, then `lens restart`. If the key is set and jobs still fail
this way, check `env-readable`: jobs run as `lens` and read `.env` themselves.

### 203/EXEC Permission denied after an update

An SELinux label problem on the binary. `bootstrap.sh` and `update.sh` run
`restorecon -RF` on `bin/` for this reason; if you built or copied a binary by
hand, do the same:

```bash
sudo restorecon -RF /opt/lens/bin
sudo systemctl restart lens.service
```

A binary built under `/tmp` and moved into place keeps its `tmp_t` label, which
is exactly what this fixes.

### Many websites come back `Failed` with block signatures

Expected on a datacenter IP. Commercial anti-bot (Cloudflare, DataDome,
PerimeterX) rejects the whole IP range, and Lens fails those fast on purpose:
hammering escalates a soft block into a permanent ban.

Check the `Bot_Protection` column to see what actually happened —
`None Detected`, `Moderate` (plain HTTP blocked, a rendering browser got
through), `Aggressive` (everything blocked) or `Unknown` (timeouts, dead
domains — not a block at all). Rows rescued by the research pass carry
`Scrape_Mode=research`.

Do not raise retry counts: the research fallback already classifies these
domains without contacting them from this host, and routing the crawl through
a residential proxy is not currently supported. Stronger bot protection is also a *positive* buying signal, so `Aggressive`
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
  Lens warns and substitutes the default for models known to be
  permanently gone; check the logs for that warning.
- Watch the spend. A large list against a frontier model is expensive; the
  dashboard's model picker is price-capped for exactly this reason.

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
stop an otherwise healthy deep-crawl container.

### Build fails during an update or bootstrap

The live install is untouched (the build runs in staging), so this is never an
outage. The usual causes:

- **No Go.** `lens update` installs `golang` and `make` itself; if dnf cannot,
  install them by hand (`sudo dnf install -y golang make`) or with
  `lens host tools --yes`.
- **Toolchain or module download fails.** With `GOTOOLCHAIN=auto` a distro Go
  older than `go.mod`'s pin downloads the pinned release, and modules come
  from `proxy.golang.org` (checked against `sum.golang.org`). Allow those
  hosts, or set `GOPROXY`/`GOSUMDB` for your mirror in the environment of
  `lens update`. On an air-gapped box, install the pinned Go release and run
  with `GOTOOLCHAIN=local`.
- **A corrupt cache.** `/var/cache/lens-build` is disposable. Go makes module
  files read-only, so clear it with
  `sudo chmod -R u+w /var/cache/lens-build && sudo rm -rf /var/cache/lens-build`
  and retry; the next build downloads everything again.

Do not recursively chown `/opt/lens/.local`: rootless container layers
deliberately contain subordinate UIDs/GIDs.

### Missing chromedriver / deep scrape never runs

Two separate Chromes are in play. `chromium` + `chromedriver` come from `dnf`
(skipped by `LENS_BOOTSTRAP_SKIP_CHROME=1`), while the deep-scrape pass uses
the **containerised** Chrome (`selenium/standalone-chrome`, driven by Lens's
own WebDriver client) and needs only podman.

- `chromedriver: command not found`: `sudo dnf install -y chromium chromedriver`.
- `short-name resolution enforced but cannot prompt without a TTY`: the image
  name lost its `docker.io/` prefix. Restore the fully qualified default in
  `deep_scrape_image`.
- Nothing happens at all: `podman ps -a --filter name=elcano-lens-chromedriver`,
  then check whether the image is present for the `lens` user. First use pulls
  ~1 GB, which can exceed `deep_scrape_startup_timeout` — pre-pull it.

### Uploads fail at ~100 MB

Two independent limits: Lens's own 100 MB (`MaxUploadBytes` in
`internal/jobs/files.go`) and `client_max_body_size` (nginx) or Caddy's
default. A 413 from the proxy never
reaches Lens, so raise the proxy limit first.

### Disk filling up

Usual suspects, in order: `managed-files/outputs/` (never pruned), podman
image storage under `/opt/lens/.local/share/containers` (`deep_scrape_pull_policy:
always` accumulates image layers), `/opt/lens.rollback.*` snapshots (the
first Go update's also holds the Python venv), the Go build cache in
`/var/cache/lens-build` (disposable; see above), `/var/lib/lens/access.db`, and
journald.

```bash
sudo du -sh /opt/lens/managed-files/* /opt/lens/.local/share/containers /opt/lens.rollback.* /var/cache/lens-build /var/lib/lens
sudo runuser -u lens -- env XDG_RUNTIME_DIR=/run/user/$(id -u lens) \
  HOME=/opt/lens podman image prune -a
sudo journalctl --vacuum-time=14d
```

### Auth signing-key rotation

In central mode Lens verifies the sign-in `id_token` and every back-channel token
with `AUTH_SIGNING_PUBKEY` plus the keys Auth publishes at `/jwks.json` (cached 10
minutes, refreshed once when a token names an unknown key). An Auth key rotation
therefore needs no env edit here; keep one static key so verification works even if
Auth is unreachable at startup. `AUTH_SIGNING_PREVIOUS_PUBKEYS` remains available as a manual override.
