# Lens API and MCP server

Lens can be driven by programs as well as people. Fleet (Elcano's agent
platform) uses this API to start Lens jobs from chat or a scheduled task,
check on them and pull the results into its workspace. Any other service can
use the same API.

There are two surfaces over one implementation (`internal/api`, with the MCP
adapter in `internal/mcp`):

| Surface | Endpoint | For |
|---|---|---|
| MCP (Streamable HTTP) | `POST /mcp` | Agent hosts such as Fleet |
| REST | `/api/v1/...` | Scripts, other services, `curl` |

Both authenticate with a **scoped API token** and never with a browser
session, so the dashboard's cookie and CSRF rules don't apply to them.

## Tokens

An operator mints tokens on the Lens host:

```bash
lens token create --label fleet-elcano                    # jobs:read + jobs:run
lens token create --label reporting --scope jobs:read     # read-only
lens token list                                           # never shows secrets
lens token revoke 3
```

The secret (`lens_…`) is printed once. Lens stores only its SHA-256 hash in
`/var/lib/lens/api_tokens.db` (override with `LENS_API_TOKEN_DB`), next to the
central-auth access database and outside the release tree, so updates keep it.

| Scope | Allows |
|---|---|
| `jobs:read` | List jobs and input files, read status, progress and results, and download files (directly, or by minting a 15-minute signed link) |
| `jobs:run` | Queue jobs (inline identifiers, an uploaded file, or a library file) and cancel queued or running jobs |

No token is an admin key. A token can't delete jobs or files, manage users
or tokens, or change settings: those stay with the dashboard and the host
CLI. Revoking a token takes effect on the next request and also kills every
download link that token minted.

Jobs queued with a token record its label (`submitted_by`). The dashboard
shows it on the run's row as `via API: <label>`.

## MCP

`POST /mcp` with `Authorization: Bearer lens_…`. The server is stateless: no
`Mcp-Session-Id` is issued, each POST carries one JSON-RPC message, responses
are `application/json`, and notifications get `202`. `GET`/`DELETE` return
`405`. Requests from a browser `Origin` other than the Lens host are refused.
Supported protocol versions are 2025-11-25, 2025-06-18 and 2025-03-26 (the
Streamable HTTP ones). 2024-11-05's HTTP+SSE transport is not served.

`tools/list` shows a token only the tools its scopes allow, and `tools/call`
re-checks the scope.

| Tool | Scope | What it does |
|---|---|---|
| `create_job` | `jobs:run` | Queue a job from `items` (≤10,000 identifiers), `content_base64` + `filename` (CSV/XLSX), or `input_file` (a library file). Optional: `name`, `ctv`, `custom_categories`, `research_fallback`, `deep_retry` and model overrides |
| `get_job` | `jobs:read` | Status, progress, `eta_seconds`, spend, settings, available files, queue position, and the log tail for failed or running jobs, plus a `next_step` hint |
| `list_jobs` | `jobs:read` | Recent jobs from every source, optionally filtered by `status` |
| `get_job_results` | `jobs:read` | Whole-file summary (by quality tier, content type, and top IAB tier 1) plus a page of rows; filter by `quality`, pick `columns`, page with `offset`/`limit` (≤200). Works while running |
| `get_download_link` | `jobs:read` | 15-minute signed URL for one file: `output` (CSV), `log`, `input`, or `categories` |
| `cancel_job` | `jobs:run` | Cancel a queued or running job |
| `list_input_files` | `jobs:read` | The input library, with per-file website/app/CTV counts |
| `get_options` | `jobs:read` | Default and selectable models, limits, the custom-categories format, and the output columns |

**Options follow this Lens's configuration.** An option left out of
`create_job` uses this instance's `config.json` value, which `get_options`
reports under `defaults`, and `get_job.settings` reports what the run
actually uses. A model named explicitly is passed to the run even when it is
the recommended one. `research_fallback` and `deep_retry` can only turn a
pass *off*: an explicit `true` for a pass this instance has disabled is
refused rather than silently ignored. Unknown option names (a typo such as
`deep_rety`) are refused too, so a costly run never starts with a default the
caller meant to change.

Tool failures come back as tool results with `isError: true` and a body of
`{"ok": false, "error": "...", "code": "..."}`, not as protocol errors, so an
agent can read and act on them. Codes include `unauthorized`, `forbidden`,
`job_not_found`, `invalid_job_id`, `input_not_found`, `invalid_file`,
`no_identifiers`, `too_many_items`, `invalid_model`,
`invalid_custom_categories`, `not_cancellable`, `not_started`,
`no_results_yet`, `output_missing`, `file_missing` and `too_large` (a request body over the cap; bodies are read incrementally, so a chunked upload can't bypass it).

### Files in and out of Fleet

- **In:** `content_base64` is declared `contentEncoding: base64`. Fleet sees
  that annotation and lets the model pass a workspace file reference
  (`{"workspace_file", "sha256", "offset", "length"}`) instead of pasting
  bytes. Fleet caps such files at 2 MiB, which is roughly 100,000 domains.
  Larger lists go through the dashboard upload, then
  `create_job(input_file=...)`.
- **Out:** `get_download_link` returns a URL that needs no credentials, works
  for one file only, and expires in 15 minutes. Fleet's `download_url` tool
  fetches it into the workspace, so the CSV never passes through the model.
  Links point at `LENS_PUBLIC_URL` when it is set, otherwise at the `Host`
  the request came in on.

## REST

All routes return JSON errors in the same `{"ok": false, "error", "code"}`
shape. A `401` carries `WWW-Authenticate: Bearer`.

| Method & path | Scope | Notes |
|---|---|---|
| `GET /api/v1/whoami` | any | Token label and scopes; a cheap credential check |
| `GET /api/v1/jobs?status=&limit=` | read | |
| `POST /api/v1/jobs` | run | JSON body with the `create_job` arguments, or `multipart/form-data` with `file`, optional `name`, and `options` (a JSON string of the other arguments). Returns `201` |
| `GET /api/v1/jobs/{id}` | read | Same body as `get_job` |
| `POST /api/v1/jobs/{id}/cancel` | run | `409 not_cancellable` once the job has finished |
| `GET /api/v1/jobs/{id}/results?offset=&limit=&quality=&columns=a,b` | read | Same body as `get_job_results` |
| `GET /api/v1/jobs/{id}/files/{output\|log\|input\|categories}` | read | Streams the file |
| `POST /api/v1/jobs/{id}/files/{kind}/link` | read | Signed link, as in `get_download_link` |
| `GET /api/v1/downloads/{ticket}` | none | Serves a signed link; `403 invalid_link` when expired, forged or revoked |
| `GET /api/v1/inputs` | read | |
| `GET /api/v1/options` | read | |

```bash
TOKEN=lens_...
curl -sX POST https://lens.example.com/api/v1/jobs \
  -H "Authorization: Bearer $TOKEN" -F file=@sites.csv -F name="Q4 review"
curl -s https://lens.example.com/api/v1/jobs/20261015143000-a1b2c3 -H "Authorization: Bearer $TOKEN"
curl -sOJ https://lens.example.com/api/v1/jobs/20261015143000-a1b2c3/files/output \
  -H "Authorization: Bearer $TOKEN"
```

## Failure behaviour

Every call answers immediately. Nothing blocks waiting for a job to finish,
so a caller polls `get_job` and is never left hanging on a long request.

- A bad, revoked or missing token gets `401` at once.
- Inputs are checked before anything is queued: an unknown library file, a
  non-CSV/XLSX upload, invalid base64, a file with no recognisable
  identifiers, unlisted models and malformed custom categories are all
  refused.
- A finished job with no output file reports `output_missing`, not an empty
  result.
- Lens being down shows up as a connection error or a `5xx` from the proxy.
  Lens itself never returns a partial success.

## What this does not do

- No push notifications or webhooks: callers poll `get_job`.
- No per-token job isolation: like the dashboard, every token sees every
  job on its Lens instance. Separate customers run separate Lens hosts.
- No deleting jobs or files over the API.
- No chunked upload larger than Fleet's 2 MiB workspace-file cap. The REST
  multipart upload accepts the dashboard's 100 MB.
