# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""Programmatic Lens: the REST API and the logic the MCP endpoint shares.

Everything here authenticates with a scoped bearer token from
``api_tokens`` and never with a browser session, so the dashboard's cookie
and CSRF rules and this surface cannot leak into each other.

Jobs created here go through ``web_service.manager`` exactly like a browser
upload or ``lens submit``: same queue, same validation, same Runs table. The
caller is recorded on the job (``submitted_by``) so a run started by Fleet is
recognisable in the dashboard.

The module reads web_service's globals at call time (``_ws()``) rather than
importing names, so the test suite's path isolation, which repoints
``web_service.INPUT_DIR``/``OUTPUT_DIR`` per test, applies here too.

See docs/API.md for the contract.
"""

from __future__ import annotations

import asyncio
import base64
import binascii
import csv
import io
import json
import os
import re
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, List, Optional

from fastapi import APIRouter, Request
from fastapi.responses import FileResponse, JSONResponse

import api_tokens
from api_tokens import SCOPE_READ, SCOPE_RUN, ApiToken, TokenError
from config import config
from custom_categories import MAX_CATEGORIES, validate_categories

# Inline identifier lists are for chat-sized work; bigger lists belong in a
# file (or the dashboard). 10,000 short identifiers is ~300 KB of JSON.
MAX_INLINE_ITEMS = 10_000
MAX_ITEM_CHARS = 512
MAX_RESULT_ROWS = 200
DEFAULT_RESULT_ROWS = 25
PREVIEW_CELL_CHARS = 600
DEFAULT_JOB_LIST = 20
MAX_JOB_LIST = 100
LOG_TAIL_LINES = 15
TERMINAL_STATUSES = {"completed", "failed", "cancelled"}
JOB_STATUSES = {"queued", "running", "cancelling", *TERMINAL_STATUSES}
# A job's downloadable files. "output" is the classified CSV; "log" the run
# log; "input" the list the job was given; "categories" the custom-category
# definitions it ran with.
FILE_KINDS = ("output", "log", "input", "categories")
JOB_ID_RE = re.compile(r"[0-9]{14}-[0-9a-f]{6}")


def _ws():
    # web_service imports this module, so resolve it lazily. Using
    # sys.modules (not `import web_service`) also means a test's
    # monkeypatched globals are the ones read.
    return sys.modules["web_service"]


class ApiError(Exception):
    """An error with a stable code and a message written for the caller."""

    def __init__(self, status: int, code: str, message: str, **details: Any) -> None:
        super().__init__(message)
        self.status = status
        self.code = code
        self.message = message
        self.details = details

    def body(self) -> Dict[str, Any]:
        body: Dict[str, Any] = {"ok": False, "error": self.message, "code": self.code}
        if self.details:
            body["details"] = self.details
        return body


# --- Authentication --------------------------------------------------------


def bearer_token(request: Request) -> ApiToken:
    """Resolve the request's bearer token or raise a 401 ApiError."""
    header = request.headers.get("authorization", "")
    scheme, _, raw = header.partition(" ")
    if scheme.lower() != "bearer" or not raw.strip():
        raise ApiError(
            401,
            "unauthorized",
            "Missing API token. Send 'Authorization: Bearer lens_…' with a token from "
            "`lens token create` on the Lens host.",
        )
    try:
        return api_tokens.verify_token(raw)
    except TokenError as exc:
        raise ApiError(401, "unauthorized", str(exc)) from None


def require_scope(token: ApiToken, scope: str) -> None:
    if not token.allows(scope):
        raise ApiError(
            403,
            "forbidden",
            f"This token lacks the {scope} scope (it has: {' '.join(token.scopes)}).",
        )


# --- Views -----------------------------------------------------------------


def public_base_url(request: Optional[Request]) -> str:
    """The URL people and Fleet reach this Lens at, for links in responses."""
    configured = os.getenv("LENS_PUBLIC_URL", "").strip().rstrip("/")
    if configured:
        return configured
    if request is None:
        return ""
    host = request.headers.get("x-forwarded-host") or request.headers.get("host") or ""
    host = host.split(",")[0].strip()
    if not host:
        return str(request.base_url).rstrip("/")
    local = host.split(":")[0] in {"127.0.0.1", "localhost", "::1", "[::1]"}
    scheme = request.headers.get("x-forwarded-proto") or ("http" if local else "https")
    return f"{scheme.split(',')[0].strip()}://{host}"


def _as_int(value: object) -> int:
    try:
        return max(0, int(str(value or 0)))
    except (TypeError, ValueError):
        return 0


def _parse_iso(value: Optional[str]) -> Optional[datetime]:
    if not value:
        return None
    try:
        parsed = datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except ValueError:
        return None
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=timezone.utc)


def _eta_seconds(job, progress: Dict[str, object]) -> Optional[int]:
    """Same two-rate estimate the dashboard shows; None when it can't tell yet."""
    if job.status != "running":
        return None
    total = _as_int(progress.get("total_domains"))
    processed = _as_int(progress.get("processed"))
    retrying = _as_int(progress.get("retrying"))
    attempts = processed + retrying
    started = _parse_iso(job.started_at)
    if total <= 0 or attempts <= 0 or started is None:
        return None
    elapsed = (datetime.now(timezone.utc) - started).total_seconds()
    if elapsed <= 5:
        return None
    remaining = max(0, total - attempts) * (elapsed / attempts)
    remaining += retrying * _ws().DEEP_RETRY_SECONDS_PER_ITEM
    return int(round(remaining))


def _file_path(job, kind: str) -> Optional[Path]:
    ws = _ws()
    if kind == "output":
        name = job.output_file
        return ws.OUTPUT_DIR / name if name else None
    if kind == "log":
        name = job.log_file
        return ws.OUTPUT_DIR / name if name else None
    if kind == "input":
        return ws.INPUT_DIR / job.input_file
    if kind == "categories":
        if not job.custom_categories:
            return None
        return ws.OUTPUT_DIR / f"{job.id}_output.csv.categories.json"
    return None


def _available_files(job) -> List[str]:
    kinds = []
    for kind in FILE_KINDS:
        path = _file_path(job, kind)
        if path is not None and path.is_file():
            kinds.append(kind)
    return kinds


def _settings(job) -> Dict[str, object]:
    ws = _ws()
    return {
        "mode": job.mode,
        "llm_model": job.llm_model or ws.RECOMMENDED_MODEL,
        "research_fallback": job.research_fallback,
        "research_model": job.research_model or ws.RECOMMENDED_RESEARCH_MODEL,
        "deep_retry": job.deep_retry,
        "custom_categories": [
            c["name"] for c in (job.custom_categories or {}).get("categories", [])
        ],
        "decision_model": job.decision_model,
    }


def _next_step(job, files: List[str]) -> str:
    if job.status in {"queued", "running", "cancelling"}:
        return (
            "The job is still in progress. Check again with get_job in a minute or two "
            "(eta_seconds is an estimate); do not create a duplicate job."
        )
    if job.status == "completed":
        if "output" in files:
            return (
                "Done. Use get_job_results for a summary and the first rows, or "
                "get_download_link with file='output' for the full CSV."
            )
        return "The job completed but its output CSV is missing on the Lens host."
    if job.status == "failed":
        return "The job failed; see error and log_tail. The input can be re-run with create_job."
    return "The job was cancelled."


def job_view(job, request: Optional[Request] = None, *, detail: bool = False) -> Dict[str, object]:
    ws = _ws()
    progress: Dict[str, object] = {}
    if job.progress_file:
        progress = ws._read_progress(ws.OUTPUT_DIR / job.progress_file)
    total = _as_int(progress.get("total_domains"))
    processed = _as_int(progress.get("processed"))
    view: Dict[str, object] = {
        "id": job.id,
        "name": job.name,
        "status": job.status,
        "input_file": job.input_file,
        "submitted_by": job.submitted_by,
        "created_at": job.created_at,
        "started_at": job.started_at,
        "finished_at": job.finished_at,
        "progress": {
            "total": total,
            "processed": processed,
            "successful": _as_int(progress.get("successful")),
            "errors": _as_int(progress.get("errors")),
            "retrying": _as_int(progress.get("retrying")),
            "percent": round(processed / total * 100, 1) if total else 0.0,
        },
        "cost_usd": progress.get("cost_usd"),
        "eta_seconds": _eta_seconds(job, progress),
        "error": job.error,
    }
    base = public_base_url(request)
    if base:
        view["dashboard_url"] = f"{base}/"
    if detail:
        files = _available_files(job)
        view["settings"] = _settings(job)
        view["files"] = files
        if job.status == "queued":
            queue = list(ws.manager.queue)
            view["queue_position"] = queue.index(job.id) + 1 if job.id in queue else None
        if job.status in {"failed", "running"} and job.log_file:
            view["log_tail"] = ws._tail_text(ws.OUTPUT_DIR / job.log_file, max_lines=LOG_TAIL_LINES)
        view["next_step"] = _next_step(job, files)
    return view


# --- Operations (shared by REST and MCP) -----------------------------------


async def get_job_or_404(job_id: object):
    job_id = str(job_id or "").strip()
    if not JOB_ID_RE.fullmatch(job_id):
        raise ApiError(
            400, "invalid_job_id", f"{job_id!r} is not a Lens job id (YYYYMMDDHHMMSS-xxxxxx)."
        )
    job = await _ws().manager.get_job(job_id)
    if job is None:
        raise ApiError(
            404, "job_not_found", f"No job {job_id} on this Lens. Use list_jobs to find it."
        )
    return job


async def list_jobs(
    request: Optional[Request], *, status: Optional[str] = None, limit: object = None
) -> Dict[str, object]:
    if status is not None and status not in JOB_STATUSES:
        raise ApiError(400, "invalid_status", f"status must be one of {sorted(JOB_STATUSES)}")
    count = _bounded_int(limit, "limit", default=DEFAULT_JOB_LIST, low=1, high=MAX_JOB_LIST)
    jobs = await _ws().manager.list_jobs()
    if status:
        jobs = [job for job in jobs if job.status == status]
    return {
        "jobs": [job_view(job, request) for job in jobs[:count]],
        "total_matching": len(jobs),
    }


async def get_job(request: Optional[Request], job_id: object) -> Dict[str, object]:
    return job_view(await get_job_or_404(job_id), request, detail=True)


def _bounded_int(value: object, name: str, *, default: int, low: int, high: int) -> int:
    if value is None or value == "":
        return default
    if isinstance(value, bool):
        raise ApiError(400, "invalid_argument", f"{name} must be an integer")
    try:
        number = int(value)
    except (TypeError, ValueError):
        raise ApiError(400, "invalid_argument", f"{name} must be an integer") from None
    if not low <= number <= high:
        raise ApiError(400, "invalid_argument", f"{name} must be between {low} and {high}")
    return number


def _optional_bool(value: object, name: str, default: bool = True) -> bool:
    if value is None:
        return default
    if isinstance(value, bool):
        return value
    if isinstance(value, str) and value.lower() in {"true", "false", "on", "off", "1", "0"}:
        return value.lower() in {"true", "on", "1"}
    raise ApiError(400, "invalid_argument", f"{name} must be true or false")


def _input_name(name: Optional[str], suffix: str) -> str:
    stamp = datetime.now(tz=timezone.utc).strftime("%Y%m%d-%H%M%S")
    stem = re.sub(r"[^A-Za-z0-9._ ()-]", "_", (name or "api-list").strip())[:60].strip("._ -")
    return f"{stem or 'api-list'} {stamp}{suffix}"


def _items_csv(items: object) -> bytes:
    if not isinstance(items, list) or not items:
        raise ApiError(400, "invalid_argument", "items must be a non-empty list of identifiers")
    if len(items) > MAX_INLINE_ITEMS:
        raise ApiError(
            400,
            "too_many_items",
            f"items holds {len(items)} identifiers; the inline limit is {MAX_INLINE_ITEMS}. "
            "Send a CSV/XLSX file instead (content_base64 with filename).",
        )
    cleaned: List[str] = []
    seen = set()
    for raw in items:
        if not isinstance(raw, str):
            raise ApiError(400, "invalid_argument", "every item must be a string")
        value = raw.strip()
        if not value:
            continue
        if len(value) > MAX_ITEM_CHARS or "\n" in value or "\r" in value:
            raise ApiError(
                400, "invalid_argument", f"item {value[:40]!r}… is not a single identifier"
            )
        if value.lower() not in seen:
            seen.add(value.lower())
            cleaned.append(value)
    if not cleaned:
        raise ApiError(400, "invalid_argument", "items contained no identifiers")
    buffer = io.StringIO()
    writer = csv.writer(buffer)
    writer.writerow(["Domain"])
    for value in cleaned:
        writer.writerow([value])
    return buffer.getvalue().encode("utf-8")


def _decode_upload(content_base64: object, filename: object) -> tuple:
    ws = _ws()
    if not isinstance(filename, str) or not filename.strip():
        raise ApiError(
            400,
            "invalid_argument",
            "filename (ending .csv or .xlsx) is required with content_base64",
        )
    try:
        safe = ws._validate_input_filename(filename.strip())
    except ValueError as exc:
        raise ApiError(400, "invalid_file", str(exc)) from None
    if not isinstance(content_base64, str) or not content_base64:
        raise ApiError(400, "invalid_argument", "content_base64 must be a non-empty base64 string")
    if len(content_base64) > (ws.MAX_UPLOAD_BYTES * 4) // 3 + 4:
        raise ApiError(
            413, "file_too_large", f"File exceeds {ws.MAX_UPLOAD_BYTES // (1024 * 1024)} MB"
        )
    try:
        payload = base64.b64decode(content_base64, validate=True)
    except (binascii.Error, ValueError):
        raise ApiError(400, "invalid_file", "content_base64 is not valid base64") from None
    return safe, payload


async def _store_input(filename: str, payload: bytes) -> str:
    ws = _ws()
    if not payload:
        raise ApiError(400, "invalid_file", "The input file is empty")
    if len(payload) > ws.MAX_UPLOAD_BYTES:
        raise ApiError(
            413, "file_too_large", f"File exceeds {ws.MAX_UPLOAD_BYTES // (1024 * 1024)} MB"
        )
    try:
        ws._verify_input_payload(filename, payload)
    except ValueError as exc:
        raise ApiError(400, "invalid_file", str(exc)) from None
    ws.INPUT_DIR.mkdir(parents=True, exist_ok=True)
    stored = ws._unused_input_name(filename, payload)
    await asyncio.to_thread((ws.INPUT_DIR / stored).write_bytes, payload)
    breakdown = await asyncio.to_thread(ws._get_breakdown, ws.INPUT_DIR / stored)
    if not breakdown.get("total"):
        # Keep the file (it is now in the library and harmless) but refuse to
        # queue a run that would find nothing to classify.
        raise ApiError(
            400,
            "no_identifiers",
            f"Lens found no websites, apps or CTV channels in {stored}. Put domains, app "
            "store IDs or package names in a column named Domain (or url/app_id), or send a "
            "CTV list with a Bundle ID column.",
            input_file=stored,
        )
    return stored


async def _validated_models(args: Dict[str, Any], categories) -> Dict[str, Optional[str]]:
    """Same model rules as the dashboard form: defaults pass, others must be listed."""
    ws = _ws()

    def clean(name: str) -> Optional[str]:
        value = args.get(name)
        if value is None or value == "":
            return None
        try:
            return ws._clean_model_id(value)
        except ValueError as exc:
            raise ApiError(400, "invalid_model", str(exc)) from None

    llm = clean("llm_model")
    research = clean("research_model")
    decision = clean("decision_model")
    checks = [
        (llm, ws.RECOMMENDED_MODEL, "classify", "llm_model"),
        (research, ws.RECOMMENDED_RESEARCH_MODEL, "research", "research_model"),
    ]
    if categories:
        decision = decision or ws.RECOMMENDED_DECISION_MODEL
        checks.append((decision, ws.RECOMMENDED_DECISION_MODEL, "decision", "decision_model"))
    else:
        decision = None
    for value, recommended, kind, name in checks:
        if value in (None, recommended):
            continue
        await ws._get_model_catalog()
        if value not in ws._allowed_model_ids(kind):
            raise ApiError(
                400,
                "invalid_model",
                f"{name} {value!r} is not offered by this Lens. Call get_options for the list.",
            )
    return {
        "llm_model": None if llm == ws.RECOMMENDED_MODEL else llm,
        "research_model": None if research == ws.RECOMMENDED_RESEARCH_MODEL else research,
        "decision_model": decision,
    }


async def create_job(
    request: Optional[Request], token: ApiToken, args: Dict[str, Any]
) -> Dict[str, object]:
    ws = _ws()
    require_scope(token, SCOPE_RUN)
    sources = [key for key in ("items", "content_base64", "input_file") if args.get(key)]
    if len(sources) != 1:
        raise ApiError(
            400,
            "invalid_argument",
            "Give exactly one input: items (a list of identifiers), content_base64 + filename "
            "(a CSV/XLSX file), or input_file (a file already in the Lens library).",
        )
    name = ws._normalize_job_name(args.get("name"))
    ctv = _optional_bool(args.get("ctv"), "ctv", default=False)

    categories = None
    raw_categories = args.get("custom_categories")
    if raw_categories:
        try:
            categories = validate_categories(
                json.loads(raw_categories) if isinstance(raw_categories, str) else raw_categories
            )
        except (ValueError, TypeError) as exc:
            raise ApiError(400, "invalid_custom_categories", str(exc)) from None
    models = await _validated_models(args, categories)

    if args.get("items"):
        if ctv:
            raise ApiError(
                400,
                "invalid_argument",
                "CTV jobs need a file with app names and bundle IDs; send content_base64 instead of items.",
            )
        stored = await _store_input(_input_name(name, ".csv"), _items_csv(args["items"]))
    elif args.get("content_base64"):
        filename, payload = _decode_upload(args.get("content_base64"), args.get("filename"))
        stored = await _store_input(filename, payload)
    else:
        try:
            stored = ws._validate_input_filename(str(args["input_file"]).strip())
        except ValueError as exc:
            raise ApiError(400, "invalid_file", str(exc)) from None
        if not (ws.INPUT_DIR / stored).is_file():
            raise ApiError(
                404,
                "input_not_found",
                f"No input file named {stored!r}. Use list_input_files to see the library.",
            )

    job = await ws.manager.create_job(
        stored,
        "ctv" if ctv else ws.DEFAULT_MODE,
        name,
        research_fallback=_optional_bool(args.get("research_fallback"), "research_fallback"),
        deep_retry=_optional_bool(args.get("deep_retry"), "deep_retry"),
        custom_categories=categories,
        submitted_by=token.label,
        **models,
    )
    ws.logger.info("Queued job %s from API token %s (%s)", job.id, token.id, stored)
    view = job_view(job, request, detail=True)
    view["breakdown"] = ws._get_breakdown(ws.INPUT_DIR / stored)
    return view


async def cancel_job(
    request: Optional[Request], token: ApiToken, job_id: object
) -> Dict[str, object]:
    require_scope(token, SCOPE_RUN)
    job = await get_job_or_404(job_id)
    try:
        await _ws().manager.cancel_job(job.id)
    except ValueError:
        raise ApiError(
            409,
            "not_cancellable",
            f"Job {job.id} is {job.status}; only queued or running jobs can be cancelled.",
        ) from None
    return job_view(job, request, detail=True)


def _read_output(
    path: Path, offset: int, limit: int, columns: Optional[List[str]], quality: Optional[str]
):
    summary: Dict[str, Dict[str, int]] = {"quality": {}, "type": {}, "iab_tier_1": {}}
    rows: List[Dict[str, str]] = []
    matched = 0
    total = 0
    with open(path, newline="", encoding="utf-8", errors="replace") as handle:
        reader = csv.DictReader(handle)
        fieldnames = list(reader.fieldnames or [])
        if columns:
            unknown = [c for c in columns if c not in fieldnames]
            if unknown:
                raise ApiError(
                    400, "invalid_argument", f"Unknown column(s) {unknown}; available: {fieldnames}"
                )
        for row in reader:
            total += 1
            for key, column in (
                ("quality", "Quality"),
                ("type", "Type"),
                ("iab_tier_1", "IAB Tier 1"),
            ):
                value = (row.get(column) or "").strip() or "(blank)"
                summary[key][value] = summary[key].get(value, 0) + 1
            if quality and (row.get("Quality") or "").strip().lower() != quality.lower():
                continue
            matched += 1
            if matched <= offset or len(rows) >= limit:
                continue
            picked = {c: row.get(c, "") for c in (columns or fieldnames)}
            rows.append(
                {
                    k: (v if len(v or "") <= PREVIEW_CELL_CHARS else v[:PREVIEW_CELL_CHARS] + "…")
                    for k, v in picked.items()
                }
            )
    top_tier_1 = dict(sorted(summary["iab_tier_1"].items(), key=lambda kv: -kv[1])[:15])
    return {
        "columns": columns or fieldnames,
        "total_rows": total,
        "matching_rows": matched,
        "summary": {
            "quality": summary["quality"],
            "type": summary["type"],
            "top_iab_tier_1": top_tier_1,
        },
        "rows": rows,
    }


async def job_results(
    request: Optional[Request],
    job_id: object,
    *,
    offset: object = None,
    limit: object = None,
    columns: object = None,
    quality: object = None,
) -> Dict[str, object]:
    job = await get_job_or_404(job_id)
    start = _bounded_int(offset, "offset", default=0, low=0, high=10_000_000)
    count = _bounded_int(limit, "limit", default=DEFAULT_RESULT_ROWS, low=0, high=MAX_RESULT_ROWS)
    if columns is not None and (
        not isinstance(columns, list) or not all(isinstance(c, str) for c in columns)
    ):
        raise ApiError(400, "invalid_argument", "columns must be a list of column names")
    if quality is not None and not isinstance(quality, str):
        raise ApiError(400, "invalid_argument", "quality must be a string such as 'Premium'")
    path = _file_path(job, "output")
    if path is None or not path.is_file():
        if job.status in {"queued"}:
            raise ApiError(
                409, "not_started", f"Job {job.id} is still queued; there are no results yet."
            )
        raise ApiError(
            404 if job.status in TERMINAL_STATUSES else 409,
            "output_missing" if job.status in TERMINAL_STATUSES else "no_results_yet",
            f"Job {job.id} is {job.status} and has no output CSV"
            + (
                " on the Lens host."
                if job.status in TERMINAL_STATUSES
                else " yet; try again shortly."
            ),
        )
    result = await asyncio.to_thread(
        _read_output, path, start, count, columns or None, quality or None
    )
    result.update(
        {
            "job": job_view(job, request),
            "offset": start,
            "complete": job.status == "completed",
        }
    )
    if job.status != "completed":
        result["note"] = f"Job is {job.status}; these are the rows written so far."
    elif result["matching_rows"] > start + len(result["rows"]):
        result["note"] = (
            "More rows remain: page with offset, or use get_download_link(file='output') for the whole CSV."
        )
    return result


async def download_link(
    request: Optional[Request], token: ApiToken, job_id: object, kind: object
) -> Dict[str, object]:
    require_scope(token, SCOPE_READ)
    kind = str(kind or "output")
    if kind not in FILE_KINDS:
        raise ApiError(400, "invalid_argument", f"file must be one of {list(FILE_KINDS)}")
    job = await get_job_or_404(job_id)
    path = _file_path(job, kind)
    if path is None or not path.is_file():
        raise ApiError(
            404,
            "file_missing",
            f"Job {job.id} ({job.status}) has no {kind} file"
            + ("" if job.status in TERMINAL_STATUSES else " yet")
            + f". Available: {_available_files(job) or 'none'}.",
        )
    ticket, expires = await asyncio.to_thread(api_tokens.sign_download, token, job.id, kind)
    base = public_base_url(request)
    return {
        "job_id": job.id,
        "file": kind,
        "filename": path.name,
        "size_bytes": path.stat().st_size,
        "url": f"{base}/api/v1/downloads/{ticket}",
        "expires_at": datetime.fromtimestamp(expires, tz=timezone.utc).isoformat(
            timespec="seconds"
        ),
        "next_step": (
            "Fetch this URL into the workspace (in Fleet: download_url). It needs no "
            "Authorization header, works for one file only and expires at expires_at."
        ),
    }


async def list_input_files() -> Dict[str, object]:
    ws = _ws()
    ws.INPUT_DIR.mkdir(parents=True, exist_ok=True)
    files = await asyncio.to_thread(ws._list_input_files)
    return {
        "files": [
            {
                "name": item["name"],
                "size": item["size"],
                "uploaded": item["uploaded"],
                "breakdown": item["breakdown"],
            }
            for item in files
        ]
    }


async def get_options() -> Dict[str, object]:
    ws = _ws()
    catalog = await ws._get_model_catalog()

    def ids(kind: str) -> List[Dict[str, str]]:
        return [{"id": o["id"], "label": o["label"]} for o in catalog.get(kind, [])]

    return {
        "defaults": {
            "llm_model": ws.RECOMMENDED_MODEL,
            "research_model": ws.RECOMMENDED_RESEARCH_MODEL,
            "decision_model": ws.RECOMMENDED_DECISION_MODEL,
            "research_fallback": True,
            "deep_retry": True,
        },
        "models": {
            "classify": ids("classify"),
            "research": ids("research"),
            "decision": ids("decision"),
        },
        "limits": {
            "inline_items": MAX_INLINE_ITEMS,
            "upload_bytes": ws.MAX_UPLOAD_BYTES,
            "custom_categories": MAX_CATEGORIES,
            "result_rows_per_call": MAX_RESULT_ROWS,
        },
        "custom_categories_format": {
            "categories": [
                {
                    "name": "Brand safe",
                    "type": "boolean",
                    "question": "Is this content brand safe? …",
                },
                {
                    "name": "Primary purpose",
                    "type": "choice",
                    "question": "What is the main purpose of the site or app?",
                    "options": ["News", "Entertainment", "Shopping", "Other", "Unknown"],
                },
            ]
        },
        "output_columns": {"standard": config.CSV_FIELDNAMES, "ctv": config.CTV_CSV_FIELDNAMES},
    }


# --- REST routes -------------------------------------------------------------

router = APIRouter(prefix="/api/v1", tags=["api"])


def _json_error(exc: ApiError) -> JSONResponse:
    headers = {"WWW-Authenticate": 'Bearer realm="lens"'} if exc.status == 401 else None
    return JSONResponse(exc.body(), status_code=exc.status, headers=headers)


async def _run(request: Request, scope: str, operation):
    try:
        token = bearer_token(request)
        require_scope(token, scope)
        return JSONResponse(await operation(token))
    except ApiError as exc:
        return _json_error(exc)


@router.get("/whoami")
async def api_whoami(request: Request):
    try:
        token = bearer_token(request)
    except ApiError as exc:
        return _json_error(exc)
    return {"label": token.label, "scopes": list(token.scopes), "token_prefix": token.prefix}


@router.get("/jobs")
async def api_list_jobs(
    request: Request, status: Optional[str] = None, limit: Optional[int] = None
):
    return await _run(
        request, SCOPE_READ, lambda _t: list_jobs(request, status=status, limit=limit)
    )


async def _create_args(request: Request) -> Dict[str, Any]:
    content_type = request.headers.get("content-type", "")
    if content_type.startswith("multipart/form-data"):
        form = await request.form()
        upload = form.get("file")
        args: Dict[str, Any] = {}
        raw_options = form.get("options")
        if raw_options:
            try:
                args = json.loads(str(raw_options))
            except ValueError:
                raise ApiError(400, "invalid_argument", "options must be a JSON object") from None
            if not isinstance(args, dict):
                raise ApiError(400, "invalid_argument", "options must be a JSON object")
        for key in ("name", "input_file"):
            if form.get(key):
                args[key] = str(form.get(key))
        if upload is not None and hasattr(upload, "read"):
            payload = await upload.read()
            args["content_base64"] = base64.b64encode(payload).decode() if payload else ""
            args.setdefault("filename", upload.filename or "")
            if not payload:
                raise ApiError(400, "invalid_file", "The uploaded file is empty")
        return args
    try:
        body = await request.json()
    except ValueError:
        raise ApiError(
            400, "invalid_argument", "Send a JSON object or multipart/form-data"
        ) from None
    if not isinstance(body, dict):
        raise ApiError(400, "invalid_argument", "The request body must be a JSON object")
    return body


@router.post("/jobs")
async def api_create_job(request: Request):
    async def operation(token):
        return await create_job(request, token, await _create_args(request))

    response = await _run(request, SCOPE_RUN, operation)
    if response.status_code == 200:
        response.status_code = 201
    return response


@router.get("/jobs/{job_id}")
async def api_get_job(request: Request, job_id: str):
    return await _run(request, SCOPE_READ, lambda _t: get_job(request, job_id))


@router.post("/jobs/{job_id}/cancel")
async def api_cancel_job(request: Request, job_id: str):
    return await _run(request, SCOPE_RUN, lambda token: cancel_job(request, token, job_id))


@router.get("/jobs/{job_id}/results")
async def api_job_results(
    request: Request,
    job_id: str,
    offset: Optional[int] = None,
    limit: Optional[int] = None,
    quality: Optional[str] = None,
    columns: Optional[str] = None,
):
    picked = [c.strip() for c in columns.split(",") if c.strip()] if columns else None
    return await _run(
        request,
        SCOPE_READ,
        lambda _t: job_results(
            request, job_id, offset=offset, limit=limit, columns=picked, quality=quality
        ),
    )


@router.post("/jobs/{job_id}/files/{kind}/link")
async def api_download_link(request: Request, job_id: str, kind: str):
    return await _run(
        request, SCOPE_READ, lambda token: download_link(request, token, job_id, kind)
    )


def _file_response(job, kind: str):
    path = _file_path(job, kind)
    if path is None or not path.is_file():
        raise ApiError(404, "file_missing", f"Job {job.id} has no {kind} file")
    media = {"output": "text/csv", "log": "text/plain", "categories": "application/json"}.get(kind)
    return FileResponse(path, filename=path.name, media_type=media)


@router.get("/jobs/{job_id}/files/{kind}")
async def api_download_file(request: Request, job_id: str, kind: str):
    try:
        token = bearer_token(request)
        require_scope(token, SCOPE_READ)
        if kind not in FILE_KINDS:
            raise ApiError(404, "file_missing", f"file must be one of {list(FILE_KINDS)}")
        return _file_response(await get_job_or_404(job_id), kind)
    except ApiError as exc:
        return _json_error(exc)


@router.get("/downloads/{ticket}")
async def api_signed_download(ticket: str):
    try:
        try:
            _token, job_id, kind = await asyncio.to_thread(api_tokens.verify_download, ticket)
        except TokenError as exc:
            raise ApiError(403, "invalid_link", str(exc)) from None
        if kind not in FILE_KINDS:
            raise ApiError(403, "invalid_link", "Invalid download link")
        return _file_response(await get_job_or_404(job_id), kind)
    except ApiError as exc:
        return _json_error(exc)


@router.get("/inputs")
async def api_list_inputs(request: Request):
    return await _run(request, SCOPE_READ, lambda _t: list_input_files())


@router.get("/options")
async def api_options(request: Request):
    return await _run(request, SCOPE_READ, lambda _t: get_options())
