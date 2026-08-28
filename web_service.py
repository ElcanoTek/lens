#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""Internal web service for running Lens jobs."""

import asyncio
import json
import os
import re
import secrets
import signal
import socket
import sys
import time
from collections import deque
from contextlib import asynccontextmanager
from dataclasses import dataclass, field
from datetime import datetime, timezone
from io import BytesIO
from pathlib import Path
from typing import Deque, Dict, List, Optional
from urllib.parse import urlencode

from fastapi import FastAPI, File, Form, HTTPException, Request, Response, UploadFile
from fastapi.responses import FileResponse, RedirectResponse, StreamingResponse
from fastapi.staticfiles import StaticFiles
from fastapi.templating import Jinja2Templates

from auth_cookie import AUTH_LOGIN_URL, current_identity, login_redirect

BASE_DIR = Path(__file__).resolve().parent
INPUT_DIR = BASE_DIR / "managed-files" / "inputs"
OUTPUT_DIR = BASE_DIR / "managed-files" / "outputs"
# Lens detects the input type on its own: CTV lists route to the CTV
# pipeline, app IDs/packages to the store APIs, and websites to the fast
# crawler with an automatic deep-crawl retry for rows that fail. Users no
# longer pick a mode; legacy values are still accepted so jobs persisted by
# older versions keep loading (and can be re-queued by API clients).
DEFAULT_MODE = "auto"
LEGACY_MODES = {"direct", "deep", "firecrawl", "app", "ctv"}
ALLOWED_MODES = {DEFAULT_MODE} | LEGACY_MODES
JOBS_STATE_FILENAME = "_jobs.json"
# Per-item cost projected for the retry queue in the ETA. Single-session
# headless Chrome averages ~15s per site (container startup amortized); when
# the local Firecrawl service is up, most retries clear through its
# concurrent pass at a few seconds per item instead.
DEEP_RETRY_SECONDS_PER_ITEM = 15.0
FIRECRAWL_RETRY_SECONDS_PER_ITEM = 3.0
FIRECRAWL_PROBE_PORT = 3002
_FIRECRAWL_PROBE_TTL_SECONDS = 60.0
_firecrawl_probe_cache: tuple[float, bool] = (0.0, False)

# --- Classification model picker -------------------------------------------
# The dropdown is populated live from OpenRouter's /models endpoint. The
# ~vendor/…-latest aliases auto-track each vendor's current model, so the
# recommended default never goes stale the way a pinned ID does.
RECOMMENDED_MODEL = "~google/gemini-flash-latest"
# The research fallback needs a model with built-in web search; Sonar Pro is
# also what the CTV pipeline has used all along.
RECOMMENDED_RESEARCH_MODEL = "perplexity/sonar-pro"
OPENROUTER_MODELS_URL = "https://openrouter.ai/api/v1/models"
# Classification is a high-volume, structurally simple task: cap the list at
# workhorse pricing so a frontier-priced model can't be picked by accident.
# The caps are per-token (OpenRouter's native pricing unit).
MODEL_PROMPT_PRICE_CAP = 2.0e-6  # $2 per million input tokens
MODEL_COMPLETION_PRICE_CAP = 10.0e-6  # $10 per million output tokens
# Research models carry a web-search premium; sonar-pro sits at $3/$15.
RESEARCH_PROMPT_PRICE_CAP = 3.0e-6
RESEARCH_COMPLETION_PRICE_CAP = 15.0e-6
_MODEL_CATALOG_TTL_SECONDS = 3600.0
# After a failed fetch, don't retry for this long — without it, a box that
# can't reach OpenRouter would stall every dashboard load on the fetch
# timeout instead of rendering immediately with the recommended default.
_MODEL_CATALOG_RETRY_SECONDS = 60.0
_EMPTY_CATALOG: Dict[str, List[Dict[str, str]]] = {"classify": [], "research": []}
_model_catalog_cache: tuple[float, Dict[str, List[Dict[str, str]]]] = (
    0.0,
    _EMPTY_CATALOG,
)
_model_catalog_failed_at: float = 0.0


def _format_price_per_million(per_token: float) -> str:
    per_million = per_token * 1_000_000
    if per_million >= 1:
        return f"${per_million:.2f}".rstrip("0").rstrip(".")
    return f"${per_million:.3f}".rstrip("0").rstrip(".")


def _filter_model_options(
    models: List[Dict[str, object]],
    *,
    required_parameter: str,
    prompt_cap: float,
    completion_cap: float,
    recommended: str,
) -> List[Dict[str, str]]:
    """Filter the raw /models payload down to dropdown entries.

    Kept: models that support *required_parameter* and price under the caps.
    Free variants are dropped — their rate limits stall batch jobs.
    """
    options: List[Dict[str, str]] = []
    for model in models:
        model_id = str(model.get("id") or "")
        if not model_id:
            continue
        pricing = model.get("pricing") or {}
        try:
            prompt_price = float(pricing.get("prompt") or 0)
            completion_price = float(pricing.get("completion") or 0)
        except (TypeError, ValueError):
            continue
        supported = model.get("supported_parameters") or []
        if required_parameter not in supported:
            continue
        if prompt_price <= 0:  # free tier: rate-limited, unfit for batches
            continue
        if prompt_price > prompt_cap or completion_price > completion_cap:
            continue
        name = str(model.get("name") or model_id)
        label = (
            f"{name} · {_format_price_per_million(prompt_price)}/M in · "
            f"{_format_price_per_million(completion_price)}/M out"
        )
        options.append({"id": model_id, "label": label})

    # ~latest aliases first (they self-update), then everything else A→Z; the
    # recommended entry is pinned to the very top.
    options.sort(
        key=lambda item: (
            item["id"] != recommended,
            not item["id"].startswith("~"),
            item["label"].lower(),
        )
    )
    return options


def _build_model_options(
    models: List[Dict[str, object]],
) -> Dict[str, List[Dict[str, str]]]:
    """Both dropdowns from one payload: classification needs tool calling
    (function-calling structured output); research needs built-in web search."""
    return {
        "classify": _filter_model_options(
            models,
            required_parameter="tools",
            prompt_cap=MODEL_PROMPT_PRICE_CAP,
            completion_cap=MODEL_COMPLETION_PRICE_CAP,
            recommended=RECOMMENDED_MODEL,
        ),
        "research": _filter_model_options(
            models,
            required_parameter="web_search_options",
            prompt_cap=RESEARCH_PROMPT_PRICE_CAP,
            completion_cap=RESEARCH_COMPLETION_PRICE_CAP,
            recommended=RECOMMENDED_RESEARCH_MODEL,
        ),
    }


async def _get_model_catalog() -> Dict[str, List[Dict[str, str]]]:
    """Fetch (and cache) the price-capped model lists for both dropdowns.

    Returns empty lists when OpenRouter is unreachable; the UI then offers
    only the recommended defaults.
    """
    global _model_catalog_cache, _model_catalog_failed_at
    cached_at, cached = _model_catalog_cache
    now = time.monotonic()
    has_cached = any(cached.values())
    if has_cached and now - cached_at < _MODEL_CATALOG_TTL_SECONDS:
        return cached
    if not has_cached and now - _model_catalog_failed_at < _MODEL_CATALOG_RETRY_SECONDS:
        return _EMPTY_CATALOG

    try:
        import aiohttp

        headers = {}
        api_key = os.getenv("OPENROUTER_API_KEY")
        if api_key:
            headers["Authorization"] = f"Bearer {api_key}"
        async with (
            aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=10)) as session,
            session.get(OPENROUTER_MODELS_URL, headers=headers) as resp,
        ):
            if resp.status != 200:
                raise RuntimeError(f"HTTP {resp.status}")
            payload = await resp.json(content_type=None)
    except Exception:
        # Keep serving the stale catalog if we have one; otherwise degrade to
        # the recommended defaults only.
        if has_cached:
            _model_catalog_cache = (now, cached)
            return cached
        _model_catalog_failed_at = now
        return _EMPTY_CATALOG

    models = payload.get("data") if isinstance(payload, dict) else None
    catalog = _build_model_options(models if isinstance(models, list) else [])
    if any(catalog.values()):
        _model_catalog_cache = (now, catalog)
    return catalog


def _allowed_model_ids(kind: str = "classify") -> set:
    _, cached = _model_catalog_cache
    recommended = RECOMMENDED_MODEL if kind == "classify" else RECOMMENDED_RESEARCH_MODEL
    return {option["id"] for option in cached.get(kind, [])} | {recommended}


def _firecrawl_probably_up() -> bool:
    """Cheap cached TCP probe of the local Firecrawl port (ETA hint only)."""
    global _firecrawl_probe_cache
    now = time.monotonic()
    stamp, value = _firecrawl_probe_cache
    if now - stamp < _FIRECRAWL_PROBE_TTL_SECONDS:
        return value
    try:
        with socket.create_connection(("127.0.0.1", FIRECRAWL_PROBE_PORT), 0.25):
            value = True
    except OSError:
        value = False
    _firecrawl_probe_cache = (now, value)
    return value


ALLOWED_INPUT_EXTENSIONS = {".csv", ".xlsx"}
MAX_UPLOAD_BYTES = 100 * 1024 * 1024
XLSX_MAGIC = b"PK\x03\x04"


def _ensure_directories() -> None:
    INPUT_DIR.mkdir(parents=True, exist_ok=True)
    OUTPUT_DIR.mkdir(parents=True, exist_ok=True)


def _jobs_state_path() -> Path:
    # Derived per call (not a module constant) so OUTPUT_DIR overrides in
    # tests are honoured and state never lands outside the managed directory.
    return OUTPUT_DIR / JOBS_STATE_FILENAME


def _utc_now() -> str:
    return datetime.now(tz=timezone.utc).isoformat()


def _safe_filename(filename: str) -> str:
    if "/" in filename or "\\" in filename:
        raise ValueError("Filename must not contain path separators")

    cleaned = Path(filename).name.strip()
    if not cleaned:
        raise ValueError("Filename cannot be empty")

    # Sanitize rather than reject: real-world filenames routinely contain
    # characters outside a strict allowlist (copy suffixes like "(2)",
    # ampersands, accented characters). The path-separator and empty checks
    # above remain the security-critical guards.
    sanitized = re.sub(r"[^A-Za-z0-9._ ()-]", "_", cleaned)
    if not sanitized.strip("._ -"):
        raise ValueError("Filename cannot be empty")
    return sanitized


def _safe_filename_or_none(value: object) -> Optional[str]:
    """Sanitize a filename from persisted state; None if missing or unsafe."""
    if not value or not isinstance(value, str):
        return None
    try:
        return _safe_filename(value)
    except ValueError:
        return None


def _is_protected_output(destination: str, filename: str) -> bool:
    """The job-state file lives in OUTPUT_DIR; never expose it to file ops."""
    return destination == "outputs" and filename == JOBS_STATE_FILENAME


def _validate_input_filename(filename: str) -> str:
    cleaned = _safe_filename(filename)
    suffix = Path(cleaned).suffix.lower()
    if suffix not in ALLOWED_INPUT_EXTENSIONS:
        raise ValueError("Input files must be .csv or .xlsx")
    return cleaned


def _verify_input_payload(filename: str, payload: bytes) -> None:
    """Sanity-check upload bytes against the extension before persisting."""
    suffix = Path(filename).suffix.lower()
    if suffix == ".xlsx":
        if not payload.startswith(XLSX_MAGIC):
            raise ValueError("File does not look like a valid .xlsx workbook (wrong file contents)")
    elif suffix == ".csv":
        # CSV has no magic bytes, but a NUL byte in the leading sample is a
        # reliable signal of a binary file mislabeled as CSV.
        if b"\x00" in payload[:4096]:
            raise ValueError("File does not look like a valid CSV (binary contents detected)")


def _normalize_job_name(raw: Optional[str]) -> Optional[str]:
    if raw is None:
        return None
    collapsed = " ".join(str(raw).split()).strip()
    if not collapsed:
        return None
    return collapsed[:120]


def _tail_text(path: Path, max_lines: int = 40) -> List[str]:
    if not path.exists() or not path.is_file():
        return []

    lines = path.read_text(encoding="utf-8", errors="ignore").splitlines()
    return lines[-max_lines:]


def _stamp_retry_rate(progress: Dict[str, object]) -> Dict[str, object]:
    """Tell the client what a queued retry item is likely to cost."""
    progress["retry_seconds_per_item"] = (
        FIRECRAWL_RETRY_SECONDS_PER_ITEM
        if _firecrawl_probably_up()
        else DEEP_RETRY_SECONDS_PER_ITEM
    )
    return progress


def _read_progress(path: Path) -> Dict[str, object]:
    if not path.exists() or not path.is_file():
        return {}

    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        return {}

    if not isinstance(payload, dict):
        return {}

    summary = payload.get("summary")
    if isinstance(summary, dict):
        return _stamp_retry_rate(dict(summary))

    # The progress JSON written by ProgressTracker stores raw counters with
    # different key names than the summary dict the UI expects.  Derive the
    # normalised summary fields so the frontend receives correct values.
    out: Dict[str, object] = {}

    # total_domains is stored directly
    total = payload.get("total_domains", 0)
    out["total_domains"] = total

    # processed = terminal entries only; retry_pending items are still in
    # flight (the deep-crawl pass will redo them), so the bar must not treat
    # them as done.
    processed_domains = payload.get("processed_domains")
    retrying = 0
    if isinstance(processed_domains, dict):
        retrying = sum(
            1
            for data in processed_domains.values()
            if isinstance(data, dict) and data.get("status") == "retry_pending"
        )
        processed = len(processed_domains) - retrying
    else:
        processed = payload.get("processed", 0)
    out["processed"] = processed
    out["retrying"] = retrying

    # successful / errors are stored as successful_count / error_count
    out["successful"] = payload.get("successful_count", payload.get("successful", 0))
    out["errors"] = payload.get("error_count", payload.get("errors", 0))

    # remaining and completion_percentage are derived
    try:
        total_n = int(str(total)) if total is not None else 0
        processed_n = int(str(processed)) if processed is not None else 0
    except (TypeError, ValueError):
        total_n = 0
        processed_n = 0

    out["remaining"] = max(0, total_n - processed_n)
    out["completion_percentage"] = (processed_n / total_n * 100) if total_n > 0 else 0.0

    # Pass through timing fields when available
    if "start_time" in payload:
        out["start_time"] = payload["start_time"]
    if "last_update" in payload:
        out["last_update"] = payload["last_update"]

    return _stamp_retry_rate(out)


@dataclass
class Job:
    id: str
    input_file: str
    mode: str
    name: Optional[str] = None
    llm_model: Optional[str] = None  # None = server default
    research_fallback: bool = True
    research_model: Optional[str] = None  # None = server default
    status: str = "queued"
    created_at: str = field(default_factory=_utc_now)
    started_at: Optional[str] = None
    finished_at: Optional[str] = None
    output_file: Optional[str] = None
    progress_file: Optional[str] = None
    log_file: Optional[str] = None
    return_code: Optional[int] = None
    error: Optional[str] = None
    process: Optional[asyncio.subprocess.Process] = None


class JobManager:
    def __init__(self) -> None:
        self.jobs: Dict[str, Job] = {}
        self.queue: Deque[str] = deque()
        self.current_job_id: Optional[str] = None
        self.lock = asyncio.Lock()
        self.queue_event = asyncio.Event()

    def _save_jobs(self) -> None:
        OUTPUT_DIR.mkdir(parents=True, exist_ok=True)
        payload = {
            "saved_at": _utc_now(),
            "jobs": [
                {
                    "id": job.id,
                    "input_file": job.input_file,
                    "mode": job.mode,
                    "name": job.name,
                    "llm_model": job.llm_model,
                    "research_fallback": job.research_fallback,
                    "research_model": job.research_model,
                    "status": job.status,
                    "created_at": job.created_at,
                    "started_at": job.started_at,
                    "finished_at": job.finished_at,
                    "output_file": job.output_file,
                    "progress_file": job.progress_file,
                    "log_file": job.log_file,
                    "return_code": job.return_code,
                    "error": job.error,
                }
                for job in sorted(self.jobs.values(), key=lambda item: item.created_at)
            ],
        }
        _jobs_state_path().write_text(json.dumps(payload, indent=2), encoding="utf-8")

    def _load_jobs(self) -> None:
        self.jobs = {}
        self.queue = deque()
        self.current_job_id = None
        self.queue_event.clear()

        state_path = _jobs_state_path()
        if not state_path.exists() or not state_path.is_file():
            return

        try:
            payload = json.loads(state_path.read_text(encoding="utf-8"))
        except json.JSONDecodeError:
            return

        raw_jobs = payload.get("jobs") if isinstance(payload, dict) else []
        if not isinstance(raw_jobs, list):
            return

        interrupted = False
        for raw in raw_jobs:
            if not isinstance(raw, dict):
                continue
            job_id = str(raw.get("id") or "").strip()
            mode = str(raw.get("mode") or "").strip()
            # State on disk is trusted-ish (we wrote it), but it is the one
            # input to path construction here — re-validate every filename so
            # a tampered or corrupted _jobs.json can never traverse paths.
            # job_id feeds output filenames in _run_job, so it gets the same
            # treatment.
            input_file = _safe_filename_or_none(raw.get("input_file"))
            if (
                not job_id
                or not re.fullmatch(r"[A-Za-z0-9._-]+", job_id)
                or not input_file
                or mode not in ALLOWED_MODES
            ):
                continue

            status = str(raw.get("status") or "queued").strip().lower()
            created_at = str(raw.get("created_at") or _utc_now())
            name_raw = raw.get("name")
            name = str(name_raw).strip() if name_raw is not None else None
            if name == "":
                name = None

            def _clean_model(value: object) -> Optional[str]:
                # Model IDs feed subprocess argv (safe from shell injection,
                # but keep obvious garbage out of persisted state).
                model = str(value).strip() if value else None
                if model and not re.fullmatch(r"[~A-Za-z0-9/._:-]+", model):
                    return None
                return model

            llm_model = _clean_model(raw.get("llm_model"))
            research_model = _clean_model(raw.get("research_model"))

            job = Job(
                id=job_id,
                input_file=input_file,
                mode=mode,
                name=name,
                llm_model=llm_model,
                research_fallback=bool(raw.get("research_fallback", True)),
                research_model=research_model,
                status=status,
                created_at=created_at,
                started_at=raw.get("started_at"),
                finished_at=raw.get("finished_at"),
                output_file=_safe_filename_or_none(raw.get("output_file")),
                progress_file=_safe_filename_or_none(raw.get("progress_file")),
                log_file=_safe_filename_or_none(raw.get("log_file")),
                return_code=raw.get("return_code"),
                error=raw.get("error"),
                process=None,
            )

            if job.status in {"running", "cancelling"}:
                job.status = "failed"
                job.error = "Interrupted by server restart"
                if not job.finished_at:
                    job.finished_at = _utc_now()
                interrupted = True

            self.jobs[job.id] = job

        queued_jobs = sorted(
            [job for job in self.jobs.values() if job.status == "queued"],
            key=lambda item: item.created_at,
        )
        self.queue = deque(job.id for job in queued_jobs)
        if self.queue:
            self.queue_event.set()

        if interrupted:
            self._save_jobs()

    async def hydrate_from_disk(self) -> None:
        async with self.lock:
            self._load_jobs()

    async def create_job(
        self,
        input_file: str,
        mode: str,
        name: Optional[str] = None,
        llm_model: Optional[str] = None,
        research_fallback: bool = True,
        research_model: Optional[str] = None,
    ) -> Job:
        if mode not in ALLOWED_MODES:
            raise ValueError(f"Invalid mode: {mode}")

        job_id = datetime.now(tz=timezone.utc).strftime("%Y%m%d%H%M%S") + "-" + secrets.token_hex(3)
        cleaned_name = (name or "").strip() or None
        job = Job(
            id=job_id,
            input_file=input_file,
            mode=mode,
            name=cleaned_name,
            llm_model=(llm_model or "").strip() or None,
            research_fallback=research_fallback,
            research_model=(research_model or "").strip() or None,
        )

        async with self.lock:
            self.jobs[job_id] = job
            self.queue.append(job_id)
            self.queue_event.set()
            self._save_jobs()
        return job

    async def list_jobs(self) -> List[Job]:
        async with self.lock:
            return sorted(self.jobs.values(), key=lambda item: item.created_at, reverse=True)

    async def get_job(self, job_id: str) -> Optional[Job]:
        async with self.lock:
            return self.jobs.get(job_id)

    async def get_current_job_id(self) -> Optional[str]:
        async with self.lock:
            return self.current_job_id

    async def cancel_job(self, job_id: str) -> None:
        async with self.lock:
            job = self.jobs.get(job_id)
            if not job:
                raise KeyError(job_id)

            if job.status == "queued":
                try:
                    self.queue.remove(job_id)
                except ValueError:
                    pass
                job.status = "cancelled"
                job.finished_at = _utc_now()
                self._save_jobs()
                return

            if job.status == "running" and job.process is not None:
                job.status = "cancelling"
                try:
                    os.killpg(job.process.pid, signal.SIGINT)
                except ProcessLookupError:
                    pass
                self._save_jobs()
                return

            raise ValueError(f"Job {job_id} is not cancellable")

    async def worker_loop(self) -> None:
        while True:
            await self.queue_event.wait()

            next_job_id: Optional[str] = None
            async with self.lock:
                if self.current_job_id is None and self.queue:
                    next_job_id = self.queue.popleft()
                    self.current_job_id = next_job_id
                if not self.queue:
                    self.queue_event.clear()

            if not next_job_id:
                await asyncio.sleep(0.1)
                continue

            await self._run_job(next_job_id)

            async with self.lock:
                self.current_job_id = None
                if self.queue:
                    self.queue_event.set()

    async def _run_job(self, job_id: str) -> None:
        async with self.lock:
            job = self.jobs[job_id]
            job.status = "running"
            job.started_at = _utc_now()
            self._save_jobs()

        input_path = INPUT_DIR / job.input_file
        output_file = f"{job.id}_output.csv"
        progress_file = f"{job.id}_progress.json"
        log_file = f"{job.id}.log"

        output_path = OUTPUT_DIR / output_file
        progress_path = OUTPUT_DIR / progress_file
        log_path = OUTPUT_DIR / log_file

        cmd = [
            sys.executable,
            "main.py",
            "--input-csv",
            str(input_path),
            "--output-csv",
            str(output_path),
            "--progress-file",
            str(progress_path),
            "--log-file",
            str(log_path),
        ]

        if job.mode == "ctv":
            cmd.append("--ctv")
        else:
            # "app" was a legacy alias for the direct scraper; every other
            # allowed mode maps straight onto a main.py --scrape-mode value.
            scrape_mode = "direct" if job.mode == "app" else job.mode
            cmd.extend(["--scrape-mode", scrape_mode])

        if job.llm_model:
            cmd.extend(["--llm-model", job.llm_model])
        if not job.research_fallback:
            cmd.extend(["--research-fallback", "off"])
        if job.research_model:
            cmd.extend(["--research-model", job.research_model])

        try:
            process = await asyncio.create_subprocess_exec(
                *cmd,
                cwd=str(BASE_DIR),
                stdout=asyncio.subprocess.DEVNULL,
                stderr=asyncio.subprocess.DEVNULL,
                start_new_session=True,
            )
            async with self.lock:
                job.process = process
                job.output_file = output_file
                job.progress_file = progress_file
                job.log_file = log_file
                self._save_jobs()

            return_code = await process.wait()

            async with self.lock:
                job.return_code = return_code
                if job.status == "cancelling":
                    job.status = "cancelled"
                elif return_code == 0:
                    job.status = "completed"
                    job.error = None
                else:
                    job.status = "failed"
                    job.error = f"Analyzer exited with code {return_code}"
                job.finished_at = _utc_now()
                job.process = None
                self._save_jobs()
        except Exception as exc:
            async with self.lock:
                job.status = "failed"
                job.finished_at = _utc_now()
                job.error = str(exc)
                job.process = None
                self._save_jobs()

    async def delete_job(self, job_id: str) -> None:
        async with self.lock:
            job = self.jobs.get(job_id)
            if not job:
                raise KeyError(job_id)

            if job.status not in {"completed", "failed", "cancelled"}:
                raise ValueError(f"Job {job_id} is not deletable")

            for filename in [job.output_file, job.progress_file, job.log_file]:
                if not filename:
                    continue
                path = OUTPUT_DIR / filename
                if path.exists() and path.is_file():
                    path.unlink()

            self.jobs.pop(job_id, None)
            try:
                self.queue.remove(job_id)
            except ValueError:
                pass
            if self.current_job_id == job_id:
                self.current_job_id = None
            self._save_jobs()


manager = JobManager()
worker_task: Optional[asyncio.Task] = None


@asynccontextmanager
async def _lifespan(app: FastAPI):
    global worker_task
    _ensure_directories()
    await manager.hydrate_from_disk()
    worker_task = asyncio.create_task(manager.worker_loop())
    try:
        yield
    finally:
        if worker_task:
            worker_task.cancel()
            await asyncio.gather(worker_task, return_exceptions=True)
            worker_task = None


app = FastAPI(title="Lens Internal Service", version="1.0.0", lifespan=_lifespan)
app.mount("/static", StaticFiles(directory=str(BASE_DIR / "static")), name="static")
templates = Jinja2Templates(directory=str(BASE_DIR / "templates"))


def _static_version() -> str:
    """Content hash of everything under static/, used to version asset URLs.

    Browsers heuristically cache /static responses, so a deploy can pair
    fresh HTML with a stale main.js whose selectors no longer match — the
    live monitor then sits at "No progress data yet." forever. Hashing the
    actual bytes (not mtimes) means the version changes exactly when any
    asset's content changes, no matter how the files got on disk.
    """
    import hashlib

    digest = hashlib.sha256()
    static_root = BASE_DIR / "static"
    for path in sorted(static_root.rglob("*")):
        if not path.is_file():
            continue
        try:
            digest.update(str(path.relative_to(static_root)).encode())
            digest.update(path.read_bytes())
        except OSError:
            continue
    return digest.hexdigest()[:12]


STATIC_VERSION = _static_version()


def static_url(path: str) -> str:
    """Versioned URL for a static asset; use for every /static reference."""
    return f"/static/{path.lstrip('/')}?v={STATIC_VERSION}"


# Every template resolves assets through this helper, so no reference can
# be left unversioned by accident.
templates.env.globals["static_url"] = static_url


@app.middleware("http")
async def _cache_policy(request: Request, call_next):
    """Foolproof cache headers to back the versioned URLs.

    - versioned /static URLs (?v=<content-hash>): immutable, cache forever —
      a new deploy changes the hash, which is a brand-new URL
    - unversioned /static URLs: always revalidate (defends direct links)
    - everything else (HTML, API): never store, so the page that carries the
      asset versions is itself always fresh
    """
    response = await call_next(request)
    if request.url.path.startswith("/static"):
        if request.query_params.get("v") == STATIC_VERSION:
            response.headers["Cache-Control"] = "public, max-age=31536000, immutable"
        else:
            response.headers["Cache-Control"] = "no-cache"
    else:
        response.headers["Cache-Control"] = "no-store"
    return response


def _require_auth(request: Request) -> None:
    # API/POST routes: 401 when the elcano_auth cookie is missing/invalid.
    # Verified against the auth service's Ed25519 public key (auth_cookie.py).
    if current_identity(request) is None:
        raise HTTPException(status_code=401)


# Where per-file content breakdowns are cached. A subdirectory (not a file)
# so _list_input_files()'s is_file() filter skips it automatically.
BREAKDOWN_CACHE_DIRNAME = ".breakdowns"


def _drop_empty_breakdown_columns(df):
    """Drop blank trailing spreadsheet columns (pandas 'Unnamed: N').

    Mirrors orchestration._drop_empty_columns so the single-column
    headerless heuristic below sees the same frame the pipeline does.
    """
    keep = []
    for col in df.columns:
        name = str(col).strip()
        is_artifact = not name or name.lower().startswith("unnamed:")
        if is_artifact and df[col].dropna().astype(str).str.strip().eq("").all():
            continue
        keep.append(col)
    return df[keep] if len(keep) != len(df.columns) else df


def _read_dataframe_for_breakdown(path: Path):
    """CSV/Excel loader for breakdown counting.

    Deliberately independent of orchestration._load_input_dataframe (so the
    web process doesn't pull in the scraper/selenium import chain) but mirrors
    its preprocessing: BOM strip, delimiter re-sniff, and empty-column drop.
    """
    import pandas as pd

    suffix = path.suffix.lower()
    if suffix == ".xlsx":
        return _drop_empty_breakdown_columns(pd.read_excel(path))

    try:
        # utf-8-sig strips an Excel BOM; reads identically for plain UTF-8.
        df = pd.read_csv(path, encoding="utf-8-sig")
    except UnicodeDecodeError:
        df = pd.read_csv(path, encoding="utf-8", encoding_errors="replace")

    # Semicolon/tab exports parse as one column whose header holds the real
    # delimiter — re-read with sniffing so column detection works.
    if len(df.columns) == 1:
        header = str(df.columns[0])
        if ";" in header or "\t" in header:
            try:
                sniffed = pd.read_csv(path, encoding="utf-8-sig", sep=None, engine="python")
                if len(sniffed.columns) > 1:
                    df = sniffed
            except Exception:
                pass

    return _drop_empty_breakdown_columns(df)


def _normalize_headerless_breakdown_df(df):
    """Recover the first row of a headerless single-column list.

    pandas reads the first line as the header, so a file that goes straight
    into a list of identifiers (domains, app IDs, Android packages — or a mix)
    loses its first entry: the off-by-one. We detect this generally by asking
    whether the "header" cell itself looks like processable data.
    """
    import pandas as pd

    from input_detector import CANDIDATE_COLUMNS, _looks_processable

    if len(df.columns) != 1 or df.empty:
        return df
    only_column = df.columns[0]
    if not isinstance(only_column, str):
        return df
    header_candidate = only_column.strip()
    if not header_candidate:
        return df
    # A recognised header name (e.g. "Domain", "pageURL") is a real header.
    if header_candidate.lower() in {c.lower() for c in CANDIDATE_COLUMNS}:
        return df
    # If the header cell looks like a domain / app ID / package, it's a lost
    # first data row, not a column name. (A real label like "identifier" is
    # not processable, so headed files are left alone.)
    if not _looks_processable(header_candidate):
        return df

    column_values = [
        value.strip() for value in df.iloc[:, 0].dropna().astype(str).tolist() if value.strip()
    ]
    # Recover the first row but keep every row — the breakdown reports what's
    # in the file, so we don't de-duplicate here.
    return pd.DataFrame({"Domain": [header_candidate, *column_values]})


def _compute_breakdown(path: Path) -> Dict[str, int]:
    """Count how many websites, iOS apps, Android apps, or CTV channels a
    file contains, using the same detection the pipeline runs on."""
    from input_detector import (
        detect_content_type,
        detect_input_column,
        detect_input_column_by_content,
        detect_type_column,
        is_ctv_input_file,
        parse_content_type_hint,
    )
    from shared_types import ContentType

    df = _read_dataframe_for_breakdown(path)
    if df is None or df.empty:
        return {}

    df = _normalize_headerless_breakdown_df(df)
    if df.empty:
        return {}

    # CTV is recognised by its column headers, not per-row, so the whole
    # file is CTV when it matches.
    if is_ctv_input_file(df):
        total = int(len(df))
        return {"ctv": total, "total": total}

    column = detect_input_column(df) or detect_input_column_by_content(df)
    if not column or column not in df.columns:
        return {}

    # Honor an explicit type column when present, exactly like the pipeline, so
    # the preview count matches how the file will actually be processed.
    type_column = detect_type_column(df, column)
    ids = df[column].astype(str)
    hints = df[type_column].astype(str) if type_column else None

    counts = {
        ContentType.WEBSITE: 0,
        ContentType.IOS_APP: 0,
        ContentType.ANDROID_APP: 0,
    }
    total = 0
    for idx in range(len(df)):
        value = ids.iloc[idx].strip()
        if not value or value.lower() == "nan":
            continue
        content_type = parse_content_type_hint(hints.iloc[idx]) if hints is not None else None
        if content_type is None:
            content_type = detect_content_type(value)
        if content_type in counts:
            counts[content_type] += 1
            total += 1

    if not total:
        return {}

    return {
        "websites": counts[ContentType.WEBSITE],
        "ios": counts[ContentType.IOS_APP],
        "android": counts[ContentType.ANDROID_APP],
        "total": total,
    }


def _get_breakdown(path: Path) -> Dict[str, int]:
    """Return the content breakdown for ``path``, cached in a sidecar keyed
    by mtime+size so the (potentially expensive) parse runs once per upload."""
    try:
        stat = path.stat()
    except OSError:
        return {}

    cache_dir = INPUT_DIR / BREAKDOWN_CACHE_DIRNAME
    cache_file = cache_dir / f"{path.name}.json"
    try:
        cached = json.loads(cache_file.read_text(encoding="utf-8"))
        if cached.get("mtime") == int(stat.st_mtime) and cached.get("size") == stat.st_size:
            return cached.get("breakdown", {}) or {}
    except Exception:
        pass

    try:
        breakdown = _compute_breakdown(path)
    except Exception:
        breakdown = {}

    try:
        cache_dir.mkdir(parents=True, exist_ok=True)
        cache_file.write_text(
            json.dumps(
                {
                    "mtime": int(stat.st_mtime),
                    "size": stat.st_size,
                    "breakdown": breakdown,
                }
            ),
            encoding="utf-8",
        )
    except Exception:
        pass

    return breakdown


# Content types in display order. ``seg`` is the CSS class that colours both
# the composition-bar segment and the legend dot; ``label`` is the short name
# shown beside counts in the Ready-to-analyze panel.
_BREAKDOWN_TYPES = [
    ("websites", "seg-site", "Websites"),
    ("ios", "seg-ios", "iOS"),
    ("android", "seg-android", "Android"),
    ("ctv", "seg-ctv", "CTV"),
]


def _breakdown_chips(breakdown: Dict[str, int]) -> List[Dict[str, object]]:
    """One entry per non-zero content type, in display order.

    Each is ``{"key", "seg", "label", "count", "pct"}`` — ``seg`` selects the
    colour, ``pct`` is the share of the total for the composition bar.
    """
    total = int(breakdown.get("total", 0) or 0)
    chips: List[Dict[str, object]] = []
    for key, seg, label in _BREAKDOWN_TYPES:
        count = int(breakdown.get(key, 0) or 0)
        if not count:
            continue
        pct = round(count / total * 100, 2) if total else 0
        chips.append({"key": key, "seg": seg, "label": label, "count": count, "pct": pct})
    return chips


def _breakdown_label(breakdown: Dict[str, int]) -> str:
    """Plain-text summary like '318 Websites · 64 iOS' (comp-bar aria-label)."""
    return " · ".join(f"{chip['count']:,} {chip['label']}" for chip in _breakdown_chips(breakdown))


def _list_input_files() -> List[Dict[str, object]]:
    now = datetime.now(tz=timezone.utc)
    files: List[Dict[str, object]] = []
    for path in sorted(
        (p for p in INPUT_DIR.iterdir() if p.is_file()),
        key=lambda p: p.stat().st_mtime,
        reverse=True,
    ):
        stat = path.stat()
        size_bytes = stat.st_size
        if size_bytes < 1024:
            size_str = f"{size_bytes} B"
        elif size_bytes < 1024 * 1024:
            size_str = f"{size_bytes // 1024} KB"
        else:
            size_str = f"{size_bytes / (1024 * 1024):.1f} MB"

        mtime = datetime.fromtimestamp(stat.st_mtime, tz=timezone.utc)
        delta = now - mtime
        if delta.days == 0:
            uploaded = f"today {mtime.strftime('%H:%M')}"
        elif delta.days == 1:
            uploaded = f"yesterday {mtime.strftime('%H:%M')}"
        else:
            uploaded = f"{mtime.strftime('%b')} {mtime.day}"

        breakdown = _get_breakdown(path)
        files.append(
            {
                "name": path.name,
                "size": size_str,
                "uploaded": uploaded,
                "breakdown": breakdown,
                "breakdown_total": int(breakdown.get("total", 0) or 0),
                "breakdown_label": _breakdown_label(breakdown),
                "breakdown_chips": _breakdown_chips(breakdown),
            }
        )
    return files


def _list_output_files() -> List[str]:
    return sorted(
        [
            path.name
            for path in OUTPUT_DIR.iterdir()
            if path.is_file() and path.name != JOBS_STATE_FILENAME
        ]
    )


def _job_settings_summary(job: Job) -> str:
    """Short provenance line for the Runs table: only non-default settings.

    Most jobs run with pure defaults and show nothing — the line exists so a
    run that used a different model or disabled the research fallback is
    distinguishable from its neighbors after the fact.
    """
    parts: List[str] = []
    if job.llm_model:
        parts.append(f"model: {job.llm_model}")
    if not job.research_fallback:
        parts.append("research fallback off")
    elif job.research_model:
        parts.append(f"research: {job.research_model}")
    return " · ".join(parts)


def _redirect_with_params(params: Dict[str, str]) -> RedirectResponse:
    query = urlencode(params)
    url = f"/?{query}" if query else "/"
    return RedirectResponse(url=url, status_code=303)


@app.get("/")
async def index(request: Request):
    if current_identity(request) is None:
        return login_redirect(request)

    jobs = await manager.list_jobs()

    def _format_created_at(created_at: str) -> str:
        raw = str(created_at or "").strip()
        if not raw:
            return "Unknown date"

        try:
            dt = datetime.fromisoformat(raw.replace("Z", "+00:00"))
            return dt.strftime("%b %d, %Y %H:%M UTC")
        except ValueError:
            pass

        compact_match = re.search(r"(\d{14})", raw)
        if compact_match:
            try:
                dt = datetime.strptime(compact_match.group(1), "%Y%m%d%H%M%S")
                return dt.strftime("%b %d, %Y %H:%M UTC")
            except ValueError:
                pass

        return raw

    def _job_id_from_output_filename(filename: str) -> Optional[str]:
        for suffix in ("_output.csv", "_progress.json", ".log"):
            if filename.endswith(suffix):
                return filename[: -len(suffix)]
        return None

    def _format_job_id_timestamp(job_id: str, fallback_iso: Optional[str]) -> str:
        match = re.match(r"^(\d{14})-", job_id)
        if match:
            try:
                dt = datetime.strptime(match.group(1), "%Y%m%d%H%M%S").replace(tzinfo=timezone.utc)
                return dt.strftime("%b %d, %Y %H:%M UTC")
            except ValueError:
                pass
        if fallback_iso:
            return _format_created_at(fallback_iso)
        return "Unknown date"

    def _job_id_timestamp_iso(job_id: str, fallback_iso: Optional[str]) -> Optional[str]:
        match = re.match(r"^(\d{14})-", job_id)
        if match:
            try:
                dt = datetime.strptime(match.group(1), "%Y%m%d%H%M%S").replace(tzinfo=timezone.utc)
                return dt.isoformat()
            except ValueError:
                pass

        if fallback_iso:
            return fallback_iso
        return None

    def _eta_text_for(job: Job, progress: Dict[str, object]) -> str:
        if job.status in {"completed", "failed", "cancelled"}:
            return "Done"
        if job.status == "queued":
            return "Queued"

        def _as_count(key: str) -> int:
            try:
                return max(0, int(str(progress.get(key) or 0)))
            except (TypeError, ValueError):
                return 0

        total = _as_count("total_domains")
        processed = _as_count("processed")
        retrying = _as_count("retrying")
        # Every item with any status has been through the fast crawler once.
        attempts = processed + retrying

        if total <= 0 or attempts <= 0 or not job.started_at:
            return "Estimating..."

        try:
            started_dt = datetime.fromisoformat(job.started_at.replace("Z", "+00:00"))
        except ValueError:
            return "Estimating..."

        elapsed_seconds = (datetime.now(timezone.utc) - started_dt).total_seconds()
        if elapsed_seconds <= 5:
            return "Estimating..."

        # Two-rate model: items not yet attempted run at the measured fast
        # rate; items queued for a retry pass each cost a roughly constant
        # amount — cheap when the concurrent Firecrawl pass will take them,
        # expensive when only single-session headless Chrome remains.
        retry_seconds_per_item = (
            FIRECRAWL_RETRY_SECONDS_PER_ITEM
            if _firecrawl_probably_up()
            else DEEP_RETRY_SECONDS_PER_ITEM
        )
        fast_rate = elapsed_seconds / attempts
        remaining_fresh = max(0, total - attempts)
        remaining_seconds = remaining_fresh * fast_rate + retrying * retry_seconds_per_item

        if remaining_seconds < 60:
            return "< 1m remaining"
        if remaining_seconds < 3600:
            return f"~{int(round(remaining_seconds / 60.0))}m remaining"
        hours = int(remaining_seconds // 3600)
        minutes = int(round((remaining_seconds % 3600) / 60.0))
        return f"~{hours}h {minutes}m remaining"

    def _progress_view_for(job: Job) -> Dict[str, object]:
        progress: Dict[str, object] = {}
        if job.progress_file:
            progress = _read_progress(OUTPUT_DIR / job.progress_file)

        total = progress.get("total_domains")
        processed = progress.get("processed")
        successful = progress.get("successful")
        errors = progress.get("errors")
        try:
            total_n = int(str(total)) if total is not None else 0
            processed_n = int(str(processed)) if processed is not None else 0
            successful_n = int(str(successful)) if successful is not None else 0
            errors_n = int(str(errors)) if errors is not None else 0
        except (TypeError, ValueError):
            total_n = 0
            processed_n = 0
            successful_n = 0
            errors_n = 0

        pct_raw = progress.get("completion_percentage")
        try:
            pct = float(str(pct_raw)) if pct_raw is not None else 0.0
        except (TypeError, ValueError):
            pct = 0.0
        if pct <= 0 and total_n > 0:
            pct = (processed_n / total_n) * 100.0
        if job.status == "completed":
            pct = 100.0
        pct = max(0.0, min(100.0, pct))

        try:
            retrying_n = int(str(progress.get("retrying") or 0))
        except (TypeError, ValueError):
            retrying_n = 0

        if total_n > 0:
            text = f"{processed_n} / {total_n}"
            if retrying_n > 0:
                text += f" · {retrying_n} to retry"
        elif processed_n > 0:
            text = f"{processed_n} processed"
        elif job.status == "queued":
            text = "Waiting"
        elif job.status in {"running", "cancelling"}:
            text = "Starting"
        else:
            text = "—"

        def _seg(count: int) -> float:
            return round((count / total_n * 100.0), 2) if total_n > 0 else 0.0

        return {
            "percentage": round(pct, 1),
            "text": text,
            "eta": _eta_text_for(job, progress),
            "successful": successful_n,
            "errors": errors_n,
            "retrying": retrying_n,
            # Segment widths for the stacked status bar: green ok, amber
            # queued-for-retry, red failed; the empty remainder is unstarted.
            "ok_pct": _seg(successful_n),
            "retry_pct": _seg(retrying_n),
            "fail_pct": _seg(errors_n),
        }

    # Group persisted artifacts on disk by the job id encoded in their names.
    job_lookup = {job.id: job for job in jobs}
    history_groups: Dict[str, Dict[str, object]] = {}
    for file_name in _list_output_files():
        job_id = _job_id_from_output_filename(file_name)
        if not job_id:
            continue
        group = history_groups.setdefault(
            job_id,
            {
                "output_file": None,
                "progress_file": None,
                "log_file": None,
            },
        )
        if file_name.endswith("_output.csv"):
            group["output_file"] = file_name
        elif file_name.endswith("_progress.json"):
            group["progress_file"] = file_name
        elif file_name.endswith(".log"):
            group["log_file"] = file_name

    # One unified list: live/queued jobs and finished runs, newest first.
    # Artifacts whose job is no longer in state (e.g. pre-restart) appear as
    # archived runs so nothing on disk is orphaned from the UI.
    runs: List[Dict[str, object]] = []
    for job in jobs:
        group = history_groups.get(job.id, {})
        runs.append(
            {
                "id": job.id,
                "title": job.name or job.id,
                "has_name": bool(job.name),
                "input_file": job.input_file,
                "status": job.status,
                "is_active": job.status in {"queued", "running", "cancelling"},
                "is_archived": False,
                "progress": _progress_view_for(job),
                "output_file": job.output_file or group.get("output_file"),
                "progress_file": job.progress_file or group.get("progress_file"),
                "log_file": job.log_file or group.get("log_file"),
                "created_at_iso": job.created_at,
                "created_at_display": _format_created_at(job.created_at),
                "settings_summary": _job_settings_summary(job),
            }
        )

    for job_id, group in sorted(history_groups.items(), reverse=True):
        if job_id in job_lookup:
            continue
        progress: Dict[str, object] = {}
        progress_file = group.get("progress_file")
        if isinstance(progress_file, str):
            progress = _read_progress(OUTPUT_DIR / progress_file)

        def _as_int(value: object) -> int:
            try:
                return int(str(value or 0))
            except (TypeError, ValueError):
                return 0

        total = _as_int(progress.get("total_domains"))
        successful = _as_int(progress.get("successful"))
        errors = _as_int(progress.get("errors"))
        runs.append(
            {
                "id": job_id,
                "title": job_id,
                "has_name": False,
                "input_file": None,
                "status": "archived",
                "is_active": False,
                "is_archived": True,
                "progress": {
                    "percentage": 100.0 if total else 0.0,
                    "text": f"{successful} ok · {errors} failed" if total else "—",
                    "eta": "",
                    "successful": successful,
                    "errors": errors,
                    "retrying": 0,
                    "ok_pct": round(successful / total * 100.0, 2) if total else 0.0,
                    "retry_pct": 0.0,
                    "fail_pct": round(errors / total * 100.0, 2) if total else 0.0,
                },
                "output_file": group.get("output_file"),
                "progress_file": progress_file,
                "log_file": group.get("log_file"),
                "created_at_iso": _job_id_timestamp_iso(job_id, None),
                "created_at_display": _format_job_id_timestamp(job_id, None),
            }
        )

    runs.sort(key=lambda item: str(item.get("created_at_iso") or ""), reverse=True)

    return templates.TemplateResponse(
        request,
        "index.html",
        {
            "runs": runs,
            "input_files": _list_input_files(),
            "model_catalog": await _get_model_catalog(),
            "recommended_model": RECOMMENDED_MODEL,
            "recommended_research_model": RECOMMENDED_RESEARCH_MODEL,
            "current_job_id": manager.current_job_id,
            "error": request.query_params.get("error", ""),
            "message": request.query_params.get("message", ""),
            "files_error": request.query_params.get("files_error", ""),
            "files_message": request.query_params.get("files_message", ""),
            "files_scope": request.query_params.get("files_scope", ""),
        },
    )


@app.post("/logout")
async def logout(request: Request):
    # Logout is owned by the auth service — it clears the shared cookie. We
    # forward there; auth redirects back to its own login afterward.
    return RedirectResponse(url=f"{AUTH_LOGIN_URL}/logout", status_code=303)


@app.post("/jobs")
async def enqueue_job(
    request: Request,
    input_file: str = Form(...),
    mode: str = Form(default=DEFAULT_MODE),
    name: Optional[str] = Form(default=None),
    llm_model: Optional[str] = Form(default=None),
    research_fallback: Optional[str] = Form(default=None),
    research_model: Optional[str] = Form(default=None),
):
    _require_auth(request)
    try:
        safe_input = _safe_filename(input_file)
    except ValueError as exc:
        return _redirect_with_params({"error": str(exc)})

    if not (INPUT_DIR / safe_input).exists():
        return RedirectResponse(url="/?error=Input+file+does+not+exist", status_code=303)

    if mode not in ALLOWED_MODES:
        return RedirectResponse(url="/?error=Invalid+mode", status_code=303)

    model = (llm_model or "").strip() or None
    if model == RECOMMENDED_MODEL:
        # The default needs no override flag — the run picks up config/newer
        # defaults automatically.
        model = None
    elif model is not None:
        await _get_model_catalog()  # warm the catalog for validation
        if model not in _allowed_model_ids("classify"):
            return RedirectResponse(url="/?error=Unknown+model", status_code=303)

    res_model = (research_model or "").strip() or None
    if res_model == RECOMMENDED_RESEARCH_MODEL:
        res_model = None
    elif res_model is not None:
        await _get_model_catalog()
        if res_model not in _allowed_model_ids("research"):
            return RedirectResponse(url="/?error=Unknown+research+model", status_code=303)

    # The checkbox posts "on" when checked and is absent when unchecked; the
    # hidden companion field posts "default" so API clients that omit both
    # keep the server default (enabled).
    fallback_enabled = research_fallback != "off" if research_fallback else True

    await manager.create_job(
        safe_input,
        mode,
        _normalize_job_name(name),
        llm_model=model,
        research_fallback=fallback_enabled,
        research_model=res_model,
    )
    return RedirectResponse(url="/?message=Job+queued", status_code=303)


@app.post("/jobs/{job_id}/cancel")
async def cancel_job(request: Request, job_id: str):
    _require_auth(request)
    try:
        await manager.cancel_job(job_id)
    except KeyError:
        return RedirectResponse(url="/?error=Unknown+job", status_code=303)
    except ValueError as exc:
        return _redirect_with_params({"error": str(exc)})

    return RedirectResponse(url="/?message=Cancellation+requested", status_code=303)


@app.post("/jobs/{job_id}/delete")
async def delete_job(request: Request, job_id: str):
    _require_auth(request)
    try:
        await manager.delete_job(job_id)
    except KeyError:
        return RedirectResponse(url="/?error=Unknown+job", status_code=303)
    except ValueError as exc:
        return _redirect_with_params({"error": str(exc)})

    return RedirectResponse(url="/?message=Job+deleted", status_code=303)


@app.post("/files/upload")
async def upload_file(
    request: Request,
    destination: str = Form(...),
    file: List[UploadFile] = File(...),
):
    _require_auth(request)
    if destination != "inputs":
        return _redirect_with_params(
            {"files_error": "Only input uploads are allowed", "files_scope": "inputs"}
        )

    uploads = file if isinstance(file, list) else [file]
    if not uploads:
        return _redirect_with_params(
            {"files_error": "Select at least one file", "files_scope": "inputs"}
        )

    uploaded_files: List[str] = []
    failed_files: List[str] = []
    size_limit_mb = MAX_UPLOAD_BYTES // (1024 * 1024)
    for item in uploads:
        raw_name = item.filename or ""
        try:
            filename = _validate_input_filename(raw_name)
            declared_size = getattr(item, "size", None)
            if declared_size is not None and declared_size > MAX_UPLOAD_BYTES:
                raise ValueError(f"File exceeds {size_limit_mb} MB limit")
            payload = await item.read()
            if not payload:
                raise ValueError("Uploaded file is empty")
            if len(payload) > MAX_UPLOAD_BYTES:
                raise ValueError(f"File exceeds {size_limit_mb} MB limit")
            _verify_input_payload(filename, payload)
            # Off the event loop: payloads can be up to MAX_UPLOAD_BYTES.
            await asyncio.to_thread((INPUT_DIR / filename).write_bytes, payload)
            # Pre-compute the content breakdown now (also off the event loop)
            # so the file library renders its counts immediately on reload.
            await asyncio.to_thread(_get_breakdown, INPUT_DIR / filename)
            uploaded_files.append(filename)
        except ValueError as exc:
            failed_name = raw_name.strip() or "unnamed-file"
            failed_files.append(f"{failed_name} ({exc})")

    params: Dict[str, str] = {"files_scope": "inputs"}
    if uploaded_files:
        if len(uploaded_files) == 1:
            params["files_message"] = f"Uploaded {uploaded_files[0]}"
        else:
            params["files_message"] = f"Uploaded {len(uploaded_files)} files"
    if failed_files:
        params["files_error"] = "Skipped: " + "; ".join(failed_files[:3])
    if not uploaded_files:
        params["files_error"] = params.get("files_error", "No files uploaded")

    return _redirect_with_params(params)


@app.post("/files/rename")
async def rename_file(
    request: Request,
    destination: str = Form(...),
    filename: str = Form(...),
    new_name: str = Form(...),
):
    _require_auth(request)
    if destination != "inputs":
        return _redirect_with_params(
            {"files_error": "Only input files can be renamed", "files_scope": destination}
        )

    try:
        safe_old = _safe_filename(filename)
        cleaned_new = _safe_filename(new_name)
        # Keep the original extension if the new name doesn't carry one —
        # users renaming "Q1 list (3).csv" to "JPMC allowlist" shouldn't
        # have to remember the suffix.
        if not Path(cleaned_new).suffix:
            cleaned_new += Path(safe_old).suffix
        safe_new = _validate_input_filename(cleaned_new)
    except ValueError as exc:
        return _redirect_with_params({"files_error": str(exc), "files_scope": destination})

    old_path = INPUT_DIR / safe_old
    new_path = INPUT_DIR / safe_new
    if not old_path.exists() or not old_path.is_file():
        return _redirect_with_params({"files_error": "File not found", "files_scope": destination})
    if safe_new == safe_old:
        return _redirect_with_params(
            {"files_message": "Name unchanged", "files_scope": destination}
        )
    if new_path.exists():
        return _redirect_with_params(
            {
                "files_error": f"A file named {safe_new} already exists",
                "files_scope": destination,
            }
        )

    old_path.rename(new_path)
    return _redirect_with_params(
        {
            "files_message": f"Renamed to {safe_new}",
            "files_scope": destination,
        }
    )


@app.get("/files/download/{destination}/{filename}")
async def download_file(request: Request, destination: str, filename: str):
    _require_auth(request)
    if destination not in {"inputs", "outputs"}:
        raise HTTPException(status_code=404)

    try:
        safe_name = _safe_filename(filename)
    except ValueError:
        raise HTTPException(status_code=404) from None

    if _is_protected_output(destination, safe_name):
        raise HTTPException(status_code=404)

    path = (INPUT_DIR if destination == "inputs" else OUTPUT_DIR) / safe_name
    if not path.exists() or not path.is_file():
        raise HTTPException(status_code=404)

    return FileResponse(path, filename=safe_name)


@app.post("/files/delete")
async def delete_file(request: Request, destination: str = Form(...), filename: str = Form(...)):
    _require_auth(request)
    if destination not in {"inputs", "outputs"}:
        return _redirect_with_params(
            {"files_error": "Invalid destination", "files_scope": destination}
        )

    try:
        safe_name = _safe_filename(filename)
    except ValueError as exc:
        return _redirect_with_params({"files_error": str(exc), "files_scope": destination})

    if _is_protected_output(destination, safe_name):
        return _redirect_with_params({"files_error": "File not found", "files_scope": destination})

    path = (INPUT_DIR if destination == "inputs" else OUTPUT_DIR) / safe_name
    if not path.exists() or not path.is_file():
        return _redirect_with_params({"files_error": "File not found", "files_scope": destination})

    path.unlink()
    return _redirect_with_params(
        {
            "files_message": f"Deleted {safe_name}",
            "files_scope": destination,
        }
    )


@app.post("/files/batch-delete")
async def batch_delete_files(
    request: Request,
    destination: str = Form(...),
    filenames: List[str] = Form(default=[]),
):
    _require_auth(request)
    if destination not in {"inputs", "outputs"}:
        return _redirect_with_params(
            {"files_error": "Invalid destination", "files_scope": destination}
        )

    if not filenames:
        return _redirect_with_params(
            {"files_error": "No files selected", "files_scope": destination}
        )

    target_dir = INPUT_DIR if destination == "inputs" else OUTPUT_DIR
    deleted = 0
    for name in filenames:
        try:
            safe_name = _safe_filename(name)
        except ValueError:
            continue
        if _is_protected_output(destination, safe_name):
            continue
        path = target_dir / safe_name
        if path.exists() and path.is_file():
            path.unlink()
            deleted += 1

    return _redirect_with_params(
        {
            "files_message": f"Deleted {deleted} file(s)",
            "files_scope": destination,
        }
    )


@app.post("/files/batch-download")
async def batch_download_files(
    request: Request,
    destination: str = Form(...),
    filenames: List[str] = Form(default=[]),
):
    _require_auth(request)
    if destination not in {"inputs", "outputs"}:
        raise HTTPException(status_code=404)
    if not filenames:
        raise HTTPException(status_code=400, detail="No files selected")

    target_dir = INPUT_DIR if destination == "inputs" else OUTPUT_DIR
    selected: List[Path] = []
    for name in filenames:
        try:
            safe_name = _safe_filename(name)
        except ValueError:
            continue
        if _is_protected_output(destination, safe_name):
            continue
        path = target_dir / safe_name
        if path.exists() and path.is_file():
            selected.append(path)

    if not selected:
        raise HTTPException(status_code=404, detail="No valid files found")

    archive = BytesIO()
    from zipfile import ZIP_DEFLATED, ZipFile

    with ZipFile(archive, "w", compression=ZIP_DEFLATED) as bundle:
        for path in selected:
            try:
                bundle.write(path, arcname=path.name)
            except FileNotFoundError:
                # Deleted between selection and archiving; skip it.
                continue
    archive.seek(0)

    ts = datetime.now(tz=timezone.utc).strftime("%Y%m%d%H%M%S")
    prefix = "inputs" if destination == "inputs" else "outputs"
    headers = {"Content-Disposition": f'attachment; filename="lens_{prefix}_{ts}.zip"'}
    return StreamingResponse(archive, media_type="application/zip", headers=headers)


@app.get("/api/jobs/live")
async def live_job_status(request: Request, job_id: Optional[str] = None):
    _require_auth(request)

    resolved_job_id = job_id
    if not resolved_job_id:
        resolved_job_id = await manager.get_current_job_id()

    if not resolved_job_id:
        return {
            "job": None,
            "progress": {},
            "log_lines": [],
            "message": "No active job right now",
        }

    job = await manager.get_job(resolved_job_id)
    if not job:
        raise HTTPException(status_code=404, detail="Unknown job")

    progress = {}
    log_lines: List[str] = []
    if job.progress_file:
        progress = _read_progress(OUTPUT_DIR / job.progress_file)
    if job.log_file:
        log_lines = _tail_text(OUTPUT_DIR / job.log_file, max_lines=50)

    return {
        "job": {
            "id": job.id,
            "name": job.name,
            "status": job.status,
            "mode": job.mode,
            "input_file": job.input_file,
            "created_at": job.created_at,
            "started_at": job.started_at,
            "finished_at": job.finished_at,
            "error": job.error,
            "return_code": job.return_code,
        },
        "progress": progress,
        "log_lines": log_lines,
        "message": "ok",
    }


@app.get("/api/jobs/queue")
async def queue_job_status(request: Request):
    _require_auth(request)

    jobs = await manager.list_jobs()
    items: List[Dict[str, object]] = []
    for job in jobs:
        progress: Dict[str, object] = {}
        if job.progress_file:
            progress = _read_progress(OUTPUT_DIR / job.progress_file)

        items.append(
            {
                "id": job.id,
                "name": job.name,
                "status": job.status,
                "started_at": job.started_at,
                "finished_at": job.finished_at,
                "progress": progress,
            }
        )

    return {"jobs": items, "message": "ok"}


@app.get("/health")
async def health():
    return {"status": "ok", "time": _utc_now()}


@app.head("/health")
async def health_head() -> Response:
    return Response(status_code=200)
