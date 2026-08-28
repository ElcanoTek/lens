# Security Policy

## Reporting a vulnerability

Email **security@elcanotek.com**. Please do not open a public issue, pull
request or discussion for a security problem.

Helpful to include:

- What the issue is and where in the code it lives.
- How to reproduce it — a minimal request, input file shape, or config.
- What an attacker gets out of it.
- The commit sha or release you tested.
- Whether you intend to disclose publicly, and on what timeline.

If you would rather encrypt, say so in a first message with no details and we
will arrange a key.

## What to expect

| Stage | Target |
|---|---|
| Acknowledgement that a human has read it | 3 business days |
| Initial assessment and severity | 10 business days |
| Fix or documented mitigation for a confirmed high-severity issue | 30 days |

Lower-severity issues are fixed on the normal release cadence. We will keep you
updated and tell you when a fix ships. Credit in the release notes is offered
unless you prefer otherwise.

We do not operate a paid bug-bounty programme.

## Scope

**In scope** — this repository:

- The FastAPI dashboard (`web_service.py`): authentication and session
  handling, file upload and download, path traversal, job control,
  authorisation gaps between routes.
- Cookie verification (`auth_cookie.py`): signature checking, expiry,
  key handling.
- The pipeline (`orchestration.py`, `scraper_client.py`,
  `openrouter_client.py`, `input_detector.py`, and the processors): injection
  through crawled content or input files, SSRF, resource exhaustion, unsafe
  parsing.
- Deployment assets (`scripts/`, `deploy/`): privilege escalation, unsafe
  defaults, secrets exposure, unsafe file permissions.
- Accidentally committed credentials or customer data.

**Out of scope:**

- Third-party services and software Lens talks to — OpenRouter, Firecrawl,
  Selenium/Chrome, Podman, Caddy, nginx. Report those upstream. Report to us
  only if *our* orchestration of them is what creates the vulnerability.
- The auth service that mints the `elcano_auth` cookie. It is a separate
  system; Lens only holds its public key.
- Findings that require an already-compromised host, root on the box, or
  physical access.
- Anything that follows from a documented configuration choice made against our
  advice — for example publishing the Firecrawl port (documented as
  localhost-only with authentication disabled), or exposing
  `127.0.0.1:8808` directly instead of behind a TLS proxy.
- Missing hardening directives in `deploy/lens.service`. `NoNewPrivileges`,
  `ProtectControlGroups`, `ProtectKernelTunables`, `PrivateTmp` and
  `ProtectHome` are absent deliberately — each one breaks rootless podman, and
  the reasons are documented in the unit. A concrete exploit that those
  directives would have prevented is in scope; their absence alone is not.
- Denial of service through deliberately abusive input volumes on a deployment
  you control.
- Reports from automated scanners with no demonstrated impact.

## Supported versions

Fixes land on `main`. There are no long-lived maintenance branches, so the
supported version is the current `main`. Deployments update with `lens update`
(see [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md)).

## Operating Lens securely

Worth knowing whether or not you are reporting anything:

- **The dashboard has no login of its own.** It verifies a signed cookie minted
  elsewhere. Without `AUTH_SIGNING_PUBKEY` set, every request redirects to the
  login URL — it fails closed, not open.
- `AUTH_SIGNING_PUBKEY` is a *public* key. It can verify, never sign, so it is
  safe to distribute and a leak cannot forge a session.
- **Bind the app to localhost** and terminate TLS in a reverse proxy. That is
  what `lens.service` and the shipped Caddy config do.
- **Keep the Firecrawl port on localhost.** The stack runs with
  `USE_DB_AUTHENTICATION=false`, which is only safe because 127.0.0.1:3002
  never leaves the host.
- `/opt/lens/.env` holds the OpenRouter key and is written `0640 lens:lens`.
  `lens env` redacts secrets when printing.
- `/health` is intentionally unauthenticated so load balancers can probe it. It
  returns only a status and a timestamp.
- **Uploads and output are sensitive.** `managed-files/` holds the identifiers
  you analyse and the results; nothing prunes it automatically.
- Lens fetches and feeds untrusted third-party web content to an LLM. Treat
  classifications as data derived from hostile input, not as trusted output.
