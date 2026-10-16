# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""Lens as an MCP server: ``POST /mcp`` (Streamable HTTP, stateless, JSON).

This is how Fleet (or any MCP host) drives Lens: start a classification job,
watch it, read results and fetch the files. Every tool is a thin adapter over
``lens_api``, so the REST API and MCP cannot drift apart, and both
authenticate with the same scoped bearer token (``lens token create``).

Transport scope, deliberately small:

* Stateless. No ``Mcp-Session-Id`` is issued, every POST stands alone, and
  responses are plain ``application/json`` (the spec allows a server to
  answer with JSON instead of an SSE stream). ``GET`` and ``DELETE`` return
  405 because there is no server-initiated stream or session to end.
* One JSON-RPC message per POST. Batches were removed from the protocol in
  2025-06-18 and are refused.
* Tools only (``tools/list``, ``tools/call``), plus ``initialize`` and
  ``ping``. ``tools/list`` shows a token only the tools its scopes allow, and
  ``tools/call`` re-checks the scope, so hiding is never the only gate.

Uploads use a base64 string property declared with
``contentEncoding: base64``. Fleet recognises that annotation and lets the
model pass a workspace file reference instead of pasting bytes into context.
Downloads hand back a short-lived signed URL that Fleet's ``download_url``
fetches into the workspace, for the same reason.
"""

from __future__ import annotations

import json
from typing import Any, Dict
from urllib.parse import urlsplit

from fastapi import APIRouter, Request, Response
from fastapi.responses import JSONResponse

import lens_api
from api_tokens import SCOPE_READ, SCOPE_RUN, ApiToken
from lens_api import ApiError

SERVER_INFO = {"name": "lens", "title": "Elcano Lens", "version": "1.0.0"}
# Newest first: the Streamable HTTP revisions. A client asking for one of
# these gets it back; anything else gets the newest, and the client decides
# whether it can proceed. 2024-11-05 is deliberately absent: it used the old
# HTTP+SSE transport (a GET stream plus a message endpoint), which this
# server does not implement, so agreeing to it would strand the client.
SUPPORTED_PROTOCOL_VERSIONS = ("2025-11-25", "2025-06-18", "2025-03-26")
MAX_BODY_BYTES = 4 * 1024 * 1024  # a 2 MiB upload is ~2.8 MB once base64-encoded

INSTRUCTIONS = " ".join(
    [
        "Elcano Lens classifies websites, iOS apps, Android apps and CTV apps into advertising",
        "quality tiers (Premium / Standard / Long Tail / Failed) with IAB categories, language,",
        "political leaning, audience size and optional custom questions.",
        "Workflow: create_job -> get_job until status is completed -> get_job_results for a",
        "summary and rows -> get_download_link(file='output') for the full CSV.",
        "Jobs run in the background and take from a minute (a few domains) to hours (tens of",
        "thousands); never create a second job for the same list while one is queued or running.",
        "For a short list pass items; for a CSV/XLSX in the workspace pass content_base64 as a",
        "workspace file reference with filename; for a file already uploaded to Lens pass",
        "input_file (see list_input_files). Jobs spend real OpenRouter money, so confirm large",
        "runs with the user first. The defaults are recommended: only set models or turn off",
        "deep_retry / research_fallback when the user asks.",
    ]
)

_JOB_ID = {
    "type": "string",
    "pattern": "^[0-9]{14}-[0-9a-f]{6}$",
    "description": "Lens job id, e.g. 20261015143000-a1b2c3 (from create_job or list_jobs).",
}

_READ = {
    "readOnlyHint": True,
    "destructiveHint": False,
    "idempotentHint": True,
    "openWorldHint": False,
}


TOOLS: Dict[str, Dict[str, Any]] = {
    "create_job": {
        "scope": SCOPE_RUN,
        "title": "Start a Lens classification job",
        "description": (
            "Queue a Lens job that classifies websites, iOS/Android apps or CTV apps. Give exactly "
            "one input: items (identifiers such as 'nytimes.com', '284882215' or "
            "'com.spotify.music'), content_base64 + filename (a .csv/.xlsx; prefer a workspace "
            "file reference), or input_file (a file already in the Lens library). Returns the job "
            "with its id; poll get_job until it completes. The run spends OpenRouter credit."
        ),
        "annotations": {
            "title": "Start a Lens classification job",
            "readOnlyHint": False,
            "destructiveHint": False,
            "idempotentHint": False,
            "openWorldHint": True,
        },
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "properties": {
                "name": {
                    "type": "string",
                    "maxLength": 120,
                    "description": "Run name shown in the Lens dashboard, e.g. 'Q4 allowlist review'.",
                },
                "items": {
                    "type": "array",
                    "items": {
                        "type": "string",
                        "minLength": 1,
                        "maxLength": lens_api.MAX_ITEM_CHARS,
                    },
                    "minItems": 1,
                    "maxItems": lens_api.MAX_INLINE_ITEMS,
                    "description": "Domains, App Store ids or Android package names, one per entry.",
                },
                "content_base64": {
                    "type": "string",
                    "contentEncoding": "base64",
                    "maxLength": 2_900_000,
                    "description": (
                        "The input file's bytes (CSV or XLSX), base64-encoded. Requires filename. "
                        "Lens finds the identifier column itself (Domain, url, pageURL, app_id, "
                        "Bundle ID for CTV)."
                    ),
                },
                "filename": {
                    "type": "string",
                    "pattern": "^[^/\\\\]+\\.([cC][sS][vV]|[xX][lL][sS][xX])$",
                    "description": "Name for the uploaded file, ending .csv or .xlsx.",
                },
                "input_file": {
                    "type": "string",
                    "description": "Name of a file already in the Lens input library (list_input_files).",
                },
                "ctv": {
                    "type": "boolean",
                    "description": "Force the CTV workflow. Lens detects CTV lists by their columns, so this is rarely needed.",
                },
                "custom_categories": {
                    "type": "object",
                    "description": (
                        'Optional extra questions answered per row, as {"categories": [{"name", '
                        '"type": "boolean"|"choice", "question", "options"?}]} (max 12). '
                        "get_options shows an example."
                    ),
                },
                "research_fallback": {
                    "type": "boolean",
                    "description": "Classify unscrapeable sites from web research (default true).",
                },
                "deep_retry": {
                    "type": "boolean",
                    "description": "Retry failed sites with headless Chrome (default true; slow on big lists).",
                },
                "llm_model": {
                    "type": "string",
                    "description": "Classification model override (see get_options).",
                },
                "research_model": {
                    "type": "string",
                    "description": "Research model override (see get_options).",
                },
                "decision_model": {
                    "type": "string",
                    "description": "Custom-category model override (see get_options).",
                },
            },
        },
        "handler": lambda request, token, args: lens_api.create_job(request, token, args),
    },
    "get_job": {
        "scope": SCOPE_READ,
        "title": "Check a Lens job",
        "description": (
            "Status, progress (processed/total, successes, errors), estimated seconds remaining, "
            "spend so far, settings, available files and, for failed jobs, the log tail."
        ),
        "annotations": {"title": "Check a Lens job", **_READ},
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "required": ["job_id"],
            "properties": {"job_id": _JOB_ID},
        },
        "handler": lambda request, token, args: lens_api.get_job(request, args.get("job_id")),
    },
    "list_jobs": {
        "scope": SCOPE_READ,
        "title": "List Lens jobs",
        "description": "Recent Lens jobs, newest first, from every source (dashboard, CLI, API).",
        "annotations": {"title": "List Lens jobs", **_READ},
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "properties": {
                "status": {
                    "type": "string",
                    "enum": sorted(lens_api.JOB_STATUSES),
                    "description": "Only jobs in this state.",
                },
                "limit": {
                    "type": "integer",
                    "minimum": 1,
                    "maximum": lens_api.MAX_JOB_LIST,
                    "description": f"How many (default {lens_api.DEFAULT_JOB_LIST}).",
                },
            },
        },
        "handler": lambda request, token, args: lens_api.list_jobs(
            request, status=args.get("status"), limit=args.get("limit")
        ),
    },
    "get_job_results": {
        "scope": SCOPE_READ,
        "title": "Read a Lens job's results",
        "description": (
            "Summary counts (by quality tier, content type and top IAB tier 1) over the whole "
            "output plus a page of rows. Filter with quality (e.g. 'Premium'), pick columns, page "
            "with offset/limit. Works on running jobs too (rows written so far). For the full "
            "file use get_download_link."
        ),
        "annotations": {"title": "Read a Lens job's results", **_READ},
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "required": ["job_id"],
            "properties": {
                "job_id": _JOB_ID,
                "offset": {
                    "type": "integer",
                    "minimum": 0,
                    "description": "Rows to skip (default 0).",
                },
                "limit": {
                    "type": "integer",
                    "minimum": 0,
                    "maximum": lens_api.MAX_RESULT_ROWS,
                    "description": f"Rows to return (default {lens_api.DEFAULT_RESULT_ROWS}; 0 for the summary only).",
                },
                "quality": {
                    "type": "string",
                    "description": "Only rows with this Quality value: Premium, Standard, Long Tail or Failed.",
                },
                "columns": {
                    "type": "array",
                    "items": {"type": "string"},
                    "description": "Only these columns, e.g. ['Domain', 'Quality', 'IAB Tier 1'].",
                },
            },
        },
        "handler": lambda request, token, args: lens_api.job_results(
            request,
            args.get("job_id"),
            offset=args.get("offset"),
            limit=args.get("limit"),
            columns=args.get("columns"),
            quality=args.get("quality"),
        ),
    },
    "get_download_link": {
        "scope": SCOPE_READ,
        "title": "Get a download link for a Lens file",
        "description": (
            "A short-lived (15 minute), single-file URL for a job's output CSV, run log, input "
            "list or custom-category definitions. Fetch it into the workspace with download_url; "
            "it needs no credentials."
        ),
        "annotations": {
            "title": "Get a download link for a Lens file",
            **_READ,
            "idempotentHint": False,
        },
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "required": ["job_id"],
            "properties": {
                "job_id": _JOB_ID,
                "file": {
                    "type": "string",
                    "enum": list(lens_api.FILE_KINDS),
                    "description": "Which file (default output).",
                },
            },
        },
        "handler": lambda request, token, args: lens_api.download_link(
            request, token, args.get("job_id"), args.get("file") or "output"
        ),
    },
    "cancel_job": {
        "scope": SCOPE_RUN,
        "title": "Cancel a Lens job",
        "description": (
            "Cancel a queued or running job. A running job stops after its current items; rows "
            "already written stay in its output."
        ),
        "annotations": {
            "title": "Cancel a Lens job",
            "readOnlyHint": False,
            "destructiveHint": True,
            "idempotentHint": True,
            "openWorldHint": False,
        },
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "required": ["job_id"],
            "properties": {"job_id": _JOB_ID},
        },
        "handler": lambda request, token, args: lens_api.cancel_job(
            request, token, args.get("job_id")
        ),
    },
    "list_input_files": {
        "scope": SCOPE_READ,
        "title": "List Lens input files",
        "description": "Files in the Lens input library, newest first, with how many websites/apps/CTV rows each holds.",
        "annotations": {"title": "List Lens input files", **_READ},
        "inputSchema": {"type": "object", "additionalProperties": False, "properties": {}},
        "handler": lambda request, token, args: lens_api.list_input_files(),
    },
    "get_options": {
        "scope": SCOPE_READ,
        "title": "Lens job options",
        "description": (
            "Default and selectable models, limits, the custom-categories format and the output "
            "CSV columns. Call it before overriding a model or writing custom categories."
        ),
        "annotations": {"title": "Lens job options", **_READ},
        "inputSchema": {"type": "object", "additionalProperties": False, "properties": {}},
        "handler": lambda request, token, args: lens_api.get_options(),
    },
}


def _tool_listing(token: ApiToken):
    return [
        {
            "name": name,
            "title": tool["title"],
            "description": tool["description"],
            "inputSchema": tool["inputSchema"],
            "annotations": tool["annotations"],
        }
        for name, tool in TOOLS.items()
        if token.allows(tool["scope"])
    ]


def _validate_arguments(schema: Dict[str, Any], args: Dict[str, Any]) -> None:
    """The structural checks a host might skip; lens_api re-validates values."""
    properties = schema.get("properties", {})
    unknown = sorted(set(args) - set(properties))
    if unknown:
        raise ApiError(400, "invalid_argument", f"Unknown argument(s): {', '.join(unknown)}")
    missing = [name for name in schema.get("required", []) if args.get(name) in (None, "")]
    if missing:
        raise ApiError(
            400, "invalid_argument", f"Missing required argument(s): {', '.join(missing)}"
        )


def _text_result(body: Dict[str, Any], is_error: bool = False) -> Dict[str, Any]:
    return {
        # Hosts such as Fleet pass only text content to the model, so the
        # text carries the whole structured result.
        "content": [{"type": "text", "text": json.dumps(body, ensure_ascii=False, default=str)}],
        "structuredContent": body,
        "isError": is_error,
    }


async def _call_tool(request: Request, token: ApiToken, params: Dict[str, Any]) -> Dict[str, Any]:
    name = params.get("name")
    args = params.get("arguments")
    if args is None:
        args = {}
    tool = TOOLS.get(name) if isinstance(name, str) else None
    if tool is None or not token.allows(tool["scope"]):
        raise _RpcError(-32602, f"Unknown tool: {name}")
    if not isinstance(args, dict):
        raise _RpcError(-32602, "arguments must be an object")
    try:
        _validate_arguments(tool["inputSchema"], args)
        body = await tool["handler"](request, token, args)
    except ApiError as exc:
        return _text_result(exc.body(), is_error=True)
    except Exception:  # noqa: BLE001 - report, never leak a traceback to the caller
        lens_api._ws().logger.exception("MCP tool %s failed", name)
        return _text_result(
            {
                "ok": False,
                "error": "Lens hit an internal error; see the Lens service log.",
                "code": "internal_error",
            },
            is_error=True,
        )
    return _text_result({"ok": True, **body})


class _RpcError(Exception):
    def __init__(self, code: int, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


def _rpc_error(message_id: Any, code: int, message: str, status: int = 200) -> JSONResponse:
    return JSONResponse(
        {"jsonrpc": "2.0", "id": message_id, "error": {"code": code, "message": message}},
        status_code=status,
    )


def _origin_allowed(request: Request) -> bool:
    """DNS-rebinding guard from the spec: a browser Origin must be this host."""
    origin = request.headers.get("origin")
    if not origin:
        return True  # server-to-server callers (Fleet) send none
    allowed = {urlsplit(lens_api.public_base_url(request)).netloc}
    host = request.headers.get("host")
    if host:
        allowed.add(host)
    return urlsplit(origin).netloc in allowed


router = APIRouter()


@router.post("/mcp")
async def mcp_endpoint(request: Request):
    if not _origin_allowed(request):
        return JSONResponse({"error": "Origin not allowed"}, status_code=403)
    try:
        token = lens_api.bearer_token(request)
    except ApiError as exc:
        return JSONResponse(
            exc.body(), status_code=401, headers={"WWW-Authenticate": 'Bearer realm="lens"'}
        )

    try:
        raw = await lens_api.read_body_capped(request, MAX_BODY_BYTES)
    except ApiError:
        return _rpc_error(None, -32600, "Request too large", status=413)
    try:
        message = json.loads(raw)
    except ValueError:
        return _rpc_error(None, -32700, "Parse error", status=400)
    if isinstance(message, list):
        return _rpc_error(None, -32600, "Batch requests are not supported", status=400)
    if not isinstance(message, dict) or message.get("jsonrpc") != "2.0":
        return _rpc_error(None, -32600, "Invalid Request", status=400)

    method = message.get("method")
    message_id = message.get("id")
    if "id" not in message:
        # Notifications (initialized, cancelled, …) and stray responses need no reply.
        return Response(status_code=202)
    if not isinstance(method, str):
        return _rpc_error(message_id, -32600, "Invalid Request", status=400)
    # Only an absent or null params means "none"; [] / false / 0 are invalid.
    params = message.get("params")
    if params is None:
        params = {}
    if not isinstance(params, dict):
        return _rpc_error(message_id, -32602, "params must be an object")

    try:
        if method == "initialize":
            requested = params.get("protocolVersion")
            version = (
                requested
                if requested in SUPPORTED_PROTOCOL_VERSIONS
                else SUPPORTED_PROTOCOL_VERSIONS[0]
            )
            result: Dict[str, Any] = {
                "protocolVersion": version,
                "capabilities": {"tools": {"listChanged": False}},
                "serverInfo": SERVER_INFO,
                "instructions": INSTRUCTIONS,
            }
        elif method == "ping":
            result = {}
        elif method == "tools/list":
            result = {"tools": _tool_listing(token)}
        elif method == "tools/call":
            result = await _call_tool(request, token, params)
        else:
            raise _RpcError(-32601, f"Method not found: {method}")
    except _RpcError as exc:
        return _rpc_error(message_id, exc.code, exc.message)
    return JSONResponse({"jsonrpc": "2.0", "id": message_id, "result": result})


@router.get("/mcp")
@router.delete("/mcp")
async def mcp_no_stream() -> Response:
    # Stateless JSON server: no SSE stream to open and no session to end.
    return Response(status_code=405, headers={"Allow": "POST"})
