# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

import asyncio
import csv
import logging
import shutil
import signal
import time
from contextlib import AsyncExitStack
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Union

import pandas as pd
from config import config
from domain_processing import DomainProcessor
from shared_types import DomainWorkItem, WorkItem, ContentType, CTVWorkItem, is_antibot_error
from openrouter_client import OpenRouterClient
from progress_tracker import ProgressTracker
from reporting import TerminalReporter
from scraper_client import ScraperClient, firecrawl_service_ready

# App processing imports
from input_detector import (
    CANDIDATE_COLUMNS,
    InputDetector,
    detect_content_type,
    detect_type_column,
    parse_content_type_hint,
    is_ctv_input_file,
    parse_ctv_work_items,
    parse_ctv_input,
)
from ios_api_client import iOSAPIClient
from android_scraper import AndroidScraper
from app_processor import AppProcessor
from ctv_processor import CTVProcessor

logger = logging.getLogger(__name__)


def _drop_empty_columns(df: pd.DataFrame) -> pd.DataFrame:
    """Drop spreadsheet artifacts: unnamed/blank columns with no values.

    Excel exports routinely carry trailing ',,,' columns (pandas names them
    'Unnamed: N'). Left in place they defeat the headerless-input heuristic,
    which only fires for single-column frames.
    """
    keep = []
    for col in df.columns:
        name = str(col).strip()
        is_artifact = not name or name.lower().startswith("unnamed:")
        if is_artifact and df[col].dropna().astype(str).str.strip().eq("").all():
            continue
        keep.append(col)
    if len(keep) != len(df.columns):
        logger.info("Dropped %d empty spreadsheet columns", len(df.columns) - len(keep))
        return df[keep]
    return df


def _maybe_resniff_delimiter(df: pd.DataFrame, path: Path) -> pd.DataFrame:
    """Re-read with delimiter sniffing when a CSV wasn't comma-separated.

    European Excel exports use ';' (and some tools '\\t'); those parse as a
    single column whose header still contains the real delimiter.
    """
    if len(df.columns) != 1:
        return df
    header = str(df.columns[0])
    if ";" not in header and "\t" not in header:
        return df
    try:
        sniffed = pd.read_csv(path, encoding="utf-8-sig", sep=None, engine="python")
    except Exception:
        return df
    if len(sniffed.columns) > 1:
        logger.info(
            "Re-read %s with sniffed delimiter (%d columns)", path, len(sniffed.columns)
        )
        return sniffed
    return df


def _load_input_dataframe(input_path: str) -> pd.DataFrame:
    path = Path(input_path)
    suffix = path.suffix.lower()

    if suffix == ".xlsx":
        df = _drop_empty_columns(pd.read_excel(path))
        logger.info("Loaded %s rows from %s", len(df), input_path)
        return df

    if suffix != ".csv":
        raise ValueError("Input file must be a .csv or .xlsx file")

    try:
        # utf-8-sig strips a leading BOM (common in Excel-exported CSVs)
        # that would otherwise pollute the first column name; for plain
        # UTF-8/ASCII files it reads identically.
        df = pd.read_csv(path, encoding="utf-8-sig")
        df = _maybe_resniff_delimiter(df, path)
        df = _drop_empty_columns(df)
        logger.info("Loaded %s rows from %s", len(df), input_path)
        return df
    except UnicodeDecodeError as decode_error:
        logger.warning(
            "Failed to decode %s as UTF-8 (%s). Retrying with replacement characters.",
            input_path,
            decode_error,
        )
        df = pd.read_csv(path, encoding="utf-8", encoding_errors="replace")
        logger.info(
            "Loaded %s rows from %s with replacement characters for invalid bytes",
            len(df),
            input_path,
        )
        return df


def _normalize_headerless_input_dataframe(df: pd.DataFrame) -> pd.DataFrame:
    if len(df.columns) != 1 or df.empty:
        return df

    only_column = df.columns[0]
    if not isinstance(only_column, str):
        return df

    header_candidate = only_column.strip()
    if not header_candidate:
        return df

    normalized_header = header_candidate.lower()
    if normalized_header in {candidate.lower() for candidate in CANDIDATE_COLUMNS}:
        return df

    column_values = [
        value.strip()
        for value in df.iloc[:, 0].dropna().astype(str).tolist()
        if value.strip()
    ]
    if not column_values:
        return df

    website_like_rows = sum(
        1
        for value in column_values
        if detect_content_type(value) == ContentType.WEBSITE
    )
    if website_like_rows / len(column_values) < 0.8:
        return df

    values = [header_candidate]
    values.extend(column_values)
    deduped_values = list(dict.fromkeys(values))
    logger.info(
        "Detected headerless single-column input; treating all %d rows as domain values",
        len(deduped_values),
    )
    return pd.DataFrame({"Domain": deduped_values})


class SiteAnalysisOrchestrator:
    """Main orchestrator for the site analysis workflow.

    Supports processing of:
    - Websites (domains)
    - iOS apps (App Store IDs)
    - Android apps (package names)
    - CTV apps (Connected TV streaming apps/channels)
    """

    def __init__(
        self,
        *,
        quiet: bool = False,
        verbose: bool = False,
        jsonl_path: Optional[str] = None,
        scrape_mode: Optional[str] = None,
        reject_redirects: bool = True,
        ctv_mode: bool = False,
    ) -> None:
        self.progress_tracker = ProgressTracker(config.PROGRESS_FILE_PATH)
        self.scraper_client: Optional[ScraperClient] = None
        self.openrouter_client: Optional[OpenRouterClient] = None
        self.ios_client: Optional[iOSAPIClient] = None
        self.android_scraper: Optional[AndroidScraper] = None
        self.shutdown_requested = False
        self.running_tasks = set()
        self.results_writer = None
        self.output_file = None
        self.quiet = quiet
        self.verbose = verbose
        self.jsonl_path = jsonl_path
        self.scrape_mode = (scrape_mode or config.SCRAPE_MODE).lower()
        if self.scrape_mode not in {"auto", "direct", "deep", "firecrawl"}:
            raise ValueError(f"Unsupported scrape mode: {self.scrape_mode}")
        # Auto mode: fast crawl first, deep crawl only for websites that fail.
        self.auto_mode = self.scrape_mode == "auto"
        if self.auto_mode:
            self.scrape_mode = "direct"
        # Set per pass: fast-pass website failures defer to a later retry
        # pass (status retry_pending) only when that retry can actually run.
        self._defer_website_failures = False
        # Cached result of the one-time Firecrawl service health probe.
        self._firecrawl_service_available: Optional[bool] = None
        self.reject_redirects = reject_redirects
        self.ctv_mode = ctv_mode
        self.reporter: Optional[TerminalReporter] = None

        # Input detection state
        self.input_detector: Optional[InputDetector] = None
        self.has_ios_apps = False
        self.has_android_apps = False
        self.has_websites = False
        self.is_ctv_input = False
        self.has_ctv_apps = False
        self.ctv_items: List[CTVWorkItem] = []
        self.ctv_work_items: List[CTVWorkItem] = []

        # Setup signal handlers for graceful shutdown
        signal.signal(signal.SIGINT, self._signal_handler)
        signal.signal(signal.SIGTERM, self._signal_handler)

    def _signal_handler(self, signum, frame):
        """Handle shutdown signals gracefully."""
        logger.info(f"Received signal {signum}, initiating graceful shutdown...")
        self.shutdown_requested = True

    async def run(self):
        """Main entry point for the application."""

        run_start = time.perf_counter()
        reporter_started = False

        try:
            # Route to CTV workflow if in CTV mode
            if self.ctv_mode:
                await self._run_ctv_workflow()
                return

            logger.info("Starting Elcano Lens Site Analysis")
            logger.info(
                "Configuration: %s with %s concurrent sessions",
                config.LLM_MODEL,
                config.CONCURRENT_SESSIONS,
            )
            logger.info("Scrape mode: %s", self.scrape_mode)

            # Load input data (this will detect if it's a CTV file)
            domains_data = self._load_input_data()

            # If CTV input detected, use CTV workflow
            if self.is_ctv_input:
                await self._run_ctv_workflow()
                return

            summary_before = self.progress_tracker.get_summary()

            total_for_reporter = summary_before.get("total_domains")
            if not total_for_reporter:
                total_for_reporter = len(domains_data)

            self.reporter = TerminalReporter(
                total=total_for_reporter,
                completed=summary_before.get("processed", 0),
                successes=summary_before.get("successful", 0),
                failures=summary_before.get("errors", 0),
                quiet=self.quiet,
                verbose=self.verbose,
                jsonl_path=self.jsonl_path,
            )
            self.reporter.start()
            reporter_started = True

            if not domains_data:
                logger.error("No valid items to process")
                self.progress_tracker.print_summary()
                if self.reporter:
                    self.reporter.render_summary(
                        summary_before, time.perf_counter() - run_start
                    )
                return

            items_to_process = list(self._filter_unprocessed_items(domains_data))
            if not items_to_process:
                logger.info("All items have already been processed!")
                self.progress_tracker.print_summary()
                if self.reporter:
                    self.reporter.render_summary(
                        self.progress_tracker.get_summary(),
                        time.perf_counter() - run_start,
                    )
                return

            logger.info(
                "Processing %s items (skipping %s already processed)",
                len(items_to_process),
                len(domains_data) - len(items_to_process),
            )

            async with AsyncExitStack() as exit_stack:
                await self._initialize_clients(exit_stack)
                self._setup_output_file()

                try:
                    self._defer_website_failures = (
                        self.auto_mode
                        and self.has_websites
                        and (
                            await self._firecrawl_available()
                            or self._deep_crawl_available()
                            or self._research_fallback_available()
                        )
                    )
                    await self._process_items_concurrently(items_to_process)
                    if self.auto_mode and not self.shutdown_requested:
                        await self._retry_failed_websites_firecrawl(
                            exit_stack, items_to_process
                        )
                    if self.auto_mode and not self.shutdown_requested:
                        await self._retry_failed_websites_deep(
                            exit_stack, items_to_process
                        )
                    if self.auto_mode and not self.shutdown_requested:
                        await self._research_failed_websites(items_to_process)
                    if self.auto_mode:
                        # Anything still pending (deep crawler failed to
                        # start, run interrupted) becomes a terminal error so
                        # progress can reach 100%.
                        await self.progress_tracker.finalize_pending_retries()
                finally:
                    self._teardown_output_file()

            if self.auto_mode:
                self._dedupe_output_csv()

            self.progress_tracker.print_summary()
            if self.reporter:
                self.reporter.render_summary(
                    self.progress_tracker.get_summary(),
                    time.perf_counter() - run_start,
                )
            logger.info("Site analysis completed successfully!")

        except Exception as e:
            logger.error("Fatal error in main workflow: %s", e)
            raise
        finally:
            if reporter_started and self.reporter:
                self.reporter.stop()

    def _load_input_data(self) -> List[WorkItem]:
        """Load and validate input data with auto-detection of content types."""
        try:
            df = _load_input_dataframe(config.INPUT_CSV_PATH)
            df = _normalize_headerless_input_dataframe(df)

            if self.ctv_mode:
                logger.info("CTV mode enabled; treating input as a CTV app list")
                self.is_ctv_input = True
                self.ctv_items = parse_ctv_input(df)

                # Set total for progress tracking
                self.progress_tracker.set_total_domains(len(self.ctv_items))

                logger.info(
                    f"Prepared {len(self.ctv_items)} unique CTV apps for processing"
                )
                return []  # Return empty list since we use ctv_items instead

            if is_ctv_input_file(df):
                if self.auto_mode:
                    logger.info(
                        "Auto-detected a CTV app list; routing to the CTV workflow"
                    )
                    self.is_ctv_input = True
                    return []
                logger.warning(
                    "Input appears to be a CTV list. Re-run with --ctv to process CTV apps explicitly."
                )

            # Use InputDetector to find the correct column and detect content types
            self.input_detector = InputDetector(df)
            if not self.input_detector.analyze():
                # Fallback to legacy "Domain" column behavior
                if "Domain" not in df.columns:
                    raise ValueError(
                        "Input file must contain a recognizable column "
                        "(pageURL, domain, url, etc.) or a 'Domain' column"
                    )
                input_column = "Domain"
            else:
                input_column = self.input_detector.input_column

            logger.info("Using input column: %s", input_column)

            # Clean and prepare data
            df = df.dropna(subset=[input_column])
            df[input_column] = df[input_column].astype(str).str.strip()

            # Remove duplicates
            df = df.drop_duplicates(subset=[input_column])

            # Honor an explicit per-row type column when the file carries one
            # (e.g. a re-uploaded results export with a Type column). This beats
            # value-based guessing on ambiguous identifiers; rows without a
            # recognized hint fall back to detect_content_type().
            type_column = detect_type_column(df, input_column)
            hint_count = 0

            # Create WorkItems with content type detection
            work_items = []
            for value, hint_value in zip(
                df[input_column],
                df[type_column] if type_column else [None] * len(df),
            ):
                content_type = parse_content_type_hint(hint_value) if type_column else None
                if content_type is None:
                    content_type = detect_content_type(value)
                else:
                    hint_count += 1
                work_items.append(
                    WorkItem(
                        identifier=value,
                        content_type=content_type,
                        original_value=value,
                    )
                )

            if type_column:
                logger.info(
                    "Applied explicit '%s' column to %d of %d rows",
                    type_column,
                    hint_count,
                    len(work_items),
                )

            # Track what types of content we have
            self.has_ios_apps = any(item.is_ios_app for item in work_items)
            self.has_android_apps = any(item.is_android_app for item in work_items)
            self.has_websites = any(item.is_website for item in work_items)

            # Log content type breakdown
            ios_count = sum(1 for item in work_items if item.is_ios_app)
            android_count = sum(1 for item in work_items if item.is_android_app)
            website_count = sum(1 for item in work_items if item.is_website)

            logger.info(
                "Content type breakdown: %d iOS apps, %d Android apps, %d websites",
                ios_count,
                android_count,
                website_count,
            )

            # Set total for progress tracking
            self.progress_tracker.set_total_domains(len(work_items))

            logger.info(f"Prepared {len(work_items)} unique items for processing")
            return work_items

        except Exception as e:
            logger.error(f"Failed to load input data: {e}")
            raise

    def _filter_unprocessed_items(
        self, work_items: Iterable[WorkItem]
    ) -> Iterable[WorkItem]:
        """Filter out items that have already been successfully processed."""
        return (
            item
            for item in work_items
            if not self.progress_tracker.is_domain_processed(item.identifier)
        )

    async def _initialize_clients(self, exit_stack: AsyncExitStack) -> None:
        """Initialize scraping, app, and OpenRouter clients based on content types."""
        logger.info("Initializing API clients...")

        # Always initialize OpenRouter client (needed for all content types)
        openrouter = OpenRouterClient(
            api_key=getattr(config, "OPENROUTER_API_KEY", None),
            model=config.LLM_MODEL,
            temperature=config.LLM_TEMPERATURE,
            max_tokens=config.LLM_MAX_TOKENS,
            max_retries=getattr(config, "LLM_REQUEST_MAX_RETRIES", 3),
            timeout=getattr(config, "LLM_REQUEST_TIMEOUT", None),
            fallback_model=getattr(config, "LLM_FALLBACK_MODEL", None),
        )
        self.openrouter_client = await exit_stack.enter_async_context(openrouter)

        logger.info("Testing OpenRouter API connection...")
        if not await self.openrouter_client.test_connection():
            raise RuntimeError(
                "Failed to initialise OpenRouter client after retry attempts. "
                "Check network connectivity and OPENROUTER_API_KEY configuration."
            )

        # Initialize website scraper if we have websites
        if self.has_websites:
            logger.info("Initializing website scraper...")
            scraper = self._build_scraper_client(self.scrape_mode)
            self.scraper_client = await exit_stack.enter_async_context(scraper)
            pool_size = config.CONCURRENT_SESSIONS
            if self.scrape_mode == "firecrawl":
                pool_size = min(pool_size, config.FIRECRAWL_MAX_CONCURRENT)
            await self.scraper_client.create_session_pool(pool_size)

        # Initialize iOS API client if we have iOS apps
        if self.has_ios_apps:
            logger.info("Initializing iOS API client...")
            ios_client = iOSAPIClient(
                request_timeout=getattr(config, "IOS_REQUEST_TIMEOUT", 30),
                request_delay=getattr(config, "IOS_REQUEST_DELAY", 3.0),
                max_concurrent=getattr(config, "IOS_MAX_CONCURRENT", 3),
                max_retries=getattr(config, "IOS_MAX_RETRIES", 3),
                retry_delay=getattr(config, "IOS_RETRY_DELAY", 2.0),
            )
            self.ios_client = await exit_stack.enter_async_context(ios_client)

        # Initialize Android scraper if we have Android apps
        if self.has_android_apps:
            logger.info("Initializing Android scraper...")
            android_scraper = AndroidScraper(
                request_timeout=getattr(config, "ANDROID_REQUEST_TIMEOUT", 30),
                request_delay=getattr(config, "ANDROID_REQUEST_DELAY", 4.0),
                max_concurrent=getattr(config, "ANDROID_MAX_CONCURRENT", 3),
                max_retries=getattr(config, "ANDROID_MAX_RETRIES", 3),
                retry_delay=getattr(config, "ANDROID_RETRY_DELAY", 2.0),
                user_agent=config.USER_AGENT,
            )
            self.android_scraper = await exit_stack.enter_async_context(android_scraper)

        logger.info("✅ All required clients initialised successfully")

    def _setup_output_file(self):
        """Setup the output CSV file and writer."""
        # Create output file if it doesn't exist
        if not Path(config.OUTPUT_CSV_PATH).exists():
            with open(config.OUTPUT_CSV_PATH, "w", newline="") as f:
                writer = csv.DictWriter(f, fieldnames=config.CSV_FIELDNAMES)
                writer.writeheader()
        else:
            self._migrate_output_csv_schema()

        # Open file for appending
        self.output_file = open(config.OUTPUT_CSV_PATH, "a", newline="")
        self.results_writer = csv.DictWriter(
            self.output_file, fieldnames=config.CSV_FIELDNAMES
        )

    def _migrate_output_csv_schema(self) -> None:
        """Rewrite an output CSV left by an older version to the current schema.

        Resumed runs append to the existing file, so a header from before a
        column was added (e.g. Bot_Protection) would silently misalign every
        new row. Old rows get "" for columns they never had.
        """
        output_path = Path(config.OUTPUT_CSV_PATH)
        try:
            with open(output_path, newline="") as f:
                reader = csv.DictReader(f)
                if reader.fieldnames == config.CSV_FIELDNAMES:
                    return
                rows = list(reader)

            logger.info(
                "Output CSV uses an older column layout; rewriting %d row(s) "
                "to the current schema",
                len(rows),
            )
            with open(output_path, "w", newline="") as f:
                writer = csv.DictWriter(
                    f, fieldnames=config.CSV_FIELDNAMES, extrasaction="ignore"
                )
                writer.writeheader()
                writer.writerows(rows)
        except Exception as exc:
            logger.warning("Could not migrate output CSV schema: %s", exc)

    def _setup_ctv_output_file(self):
        """Setup the CTV-specific output CSV file and writer."""
        # Create output file if it doesn't exist
        if not Path(config.OUTPUT_CSV_PATH).exists():
            with open(config.OUTPUT_CSV_PATH, "w", newline="") as f:
                writer = csv.DictWriter(f, fieldnames=config.CTV_CSV_FIELDNAMES)
                writer.writeheader()

        # Open file for appending
        self.output_file = open(config.OUTPUT_CSV_PATH, "a", newline="")
        self.results_writer = csv.DictWriter(
            self.output_file, fieldnames=config.CTV_CSV_FIELDNAMES
        )

    def _teardown_output_file(self) -> None:
        """Close the output file and reset writer state."""
        if self.output_file:
            self.output_file.close()
        self.output_file = None
        self.results_writer = None

    async def _process_items_concurrently(
        self, items_to_process: List[WorkItem]
    ) -> None:
        """Process items concurrently with rate limiting and content type routing."""
        semaphore = asyncio.Semaphore(config.CONCURRENT_SESSIONS)

        # Create domain processor for websites
        domain_processor = None
        if self.has_websites and self.scraper_client:
            domain_processor = DomainProcessor(
                progress_tracker=self.progress_tracker,
                scraper_client=self.scraper_client,
                openrouter_client=self.openrouter_client,
                reporter=self.reporter,
                results_writer=self.results_writer,
                results_file=self.output_file,
                defer_failures=self._defer_website_failures,
            )

        # Create app processor for iOS and Android apps
        app_processor = None
        if self.has_ios_apps or self.has_android_apps:
            app_processor = AppProcessor(
                progress_tracker=self.progress_tracker,
                ios_client=self.ios_client,
                android_scraper=self.android_scraper,
                openrouter_client=self.openrouter_client,
                reporter=self.reporter,
                results_writer=self.results_writer,
                results_file=self.output_file,
            )

        async def process_single_item(item: WorkItem):
            try:
                async with semaphore:
                    if self.shutdown_requested:
                        return

                    # Route based on content type
                    if item.is_website:
                        if domain_processor:
                            # Convert WorkItem to DomainWorkItem for backward compatibility
                            domain_item = DomainWorkItem(domain=item.identifier)
                            coro = domain_processor.process_domain(domain_item)
                        else:
                            logger.error(
                                "Website scraper not initialized for %s",
                                item.identifier,
                            )
                            return
                    elif item.is_ios_app:
                        if app_processor:
                            coro = app_processor.process_ios_app(item)
                        else:
                            logger.error(
                                "iOS client not initialized for %s", item.identifier
                            )
                            return
                    elif item.is_android_app:
                        if app_processor:
                            coro = app_processor.process_android_app(item)
                        else:
                            logger.error(
                                "Android scraper not initialized for %s",
                                item.identifier,
                            )
                            return
                    else:
                        logger.error("Unknown content type for %s", item.identifier)
                        return

                    task = asyncio.create_task(coro)
                    self.running_tasks.add(task)

                    try:
                        await task
                    except asyncio.CancelledError:
                        task.cancel()
                        await asyncio.gather(task, return_exceptions=True)
                        raise
                    finally:
                        self.running_tasks.discard(task)
            except asyncio.CancelledError:
                return

        # Create tasks for all items
        task_set = {
            asyncio.create_task(process_single_item(item)) for item in items_to_process
        }

        if not task_set:
            return

        pending_tasks = set(task_set)
        task_iter = asyncio.as_completed(task_set)

        try:
            while pending_tasks:
                if self.shutdown_requested:
                    logger.info("Shutdown requested, cancelling remaining tasks...")
                    break

                try:
                    next_task = next(task_iter)
                except StopIteration:
                    break

                try:
                    await next_task
                except Exception as e:
                    logger.error(f"Task failed: {e}")
                finally:
                    pending_tasks.discard(next_task)
        finally:
            if self.shutdown_requested and pending_tasks:
                for task in pending_tasks:
                    task.cancel()
                await asyncio.gather(*pending_tasks, return_exceptions=True)

            if self.running_tasks:
                logger.info(
                    f"Waiting for {len(self.running_tasks)} remaining tasks to complete..."
                )
                if self.shutdown_requested:
                    for task in list(self.running_tasks):
                        if not task.done():
                            task.cancel()
                await asyncio.gather(*self.running_tasks, return_exceptions=True)

    def _build_scraper_client(
        self, mode: str, *, reject_redirects: Optional[bool] = None
    ) -> ScraperClient:
        """Construct a ScraperClient for the given mode from config.

        *reject_redirects* overrides the run-level policy for a single pass
        (the Firecrawl retry pass follows cross-host redirects so it can rescue
        publishers that redirect a brand domain to its parent).
        """
        if reject_redirects is None:
            reject_redirects = self.reject_redirects
        return ScraperClient(
            request_timeout=config.REQUEST_TIMEOUT,
            max_retries=config.MAX_RETRIES,
            retry_delay=config.RETRY_DELAY,
            user_agent=config.USER_AGENT,
            mode=mode,
            podman_binary=config.DEEP_SCRAPE_PODMAN_BINARY,
            chrome_image=config.DEEP_SCRAPE_IMAGE,
            chrome_container_name=config.DEEP_SCRAPE_CONTAINER_NAME,
            chrome_port=config.DEEP_SCRAPE_PORT,
            chrome_vnc_port=config.DEEP_SCRAPE_VNC_PORT,
            chrome_extra_args=config.DEEP_SCRAPE_EXTRA_ARGS,
            chrome_startup_timeout=config.DEEP_SCRAPE_STARTUP_TIMEOUT,
            chrome_wait_after_load=config.DEEP_SCRAPE_WAIT_AFTER_LOAD,
            keep_container=config.DEEP_SCRAPE_KEEP_CONTAINER,
            reject_redirects=reject_redirects,
            chrome_pull_policy=getattr(config, "DEEP_SCRAPE_PULL_POLICY", "always"),
            firecrawl_url=config.FIRECRAWL_URL,
            firecrawl_timeout=config.FIRECRAWL_TIMEOUT,
            firecrawl_wait_for=config.FIRECRAWL_WAIT_FOR,
            firecrawl_max_age_ms=config.FIRECRAWL_MAX_AGE_MS,
            firecrawl_proxy=config.FIRECRAWL_PROXY,
        )

    async def _firecrawl_available(self) -> bool:
        """Probe the local Firecrawl service once and cache the result."""
        if self._firecrawl_service_available is not None:
            return self._firecrawl_service_available

        available = False
        if config.FIRECRAWL_ENABLED:
            available = await firecrawl_service_ready(config.FIRECRAWL_URL)
            if not available:
                logger.info(
                    "Firecrawl service not reachable at %s; "
                    "auto mode will skip the Firecrawl retry pass",
                    config.FIRECRAWL_URL,
                )

        self._firecrawl_service_available = available
        return available

    def _collect_retry_items(self, items: List[WorkItem]) -> List[WorkItem]:
        """Websites whose previous pass failed (deferred or terminal)."""
        # Deferred failures are tracked as retry_pending; include plain
        # errors too for resumed runs with older state.
        failed = set(self.progress_tracker.get_retry_pending_domains())
        failed.update(self.progress_tracker.get_failed_domains())
        return [
            item for item in items if item.is_website and item.identifier in failed
        ]

    async def _run_retry_pass(
        self,
        exit_stack: AsyncExitStack,
        retry_items: List[WorkItem],
        *,
        mode: str,
        pool_size: int,
        defer_failures: bool,
        reject_redirects: Optional[bool] = None,
    ) -> None:
        """Re-crawl *retry_items* with the given backend (auto-mode ladder)."""
        logger.info(
            "Auto mode: retrying %d failed websites with the %s crawler",
            len(retry_items),
            mode,
        )

        try:
            scraper = self._build_scraper_client(
                mode, reject_redirects=reject_redirects
            )
            scraper = await exit_stack.enter_async_context(scraper)
            await scraper.create_session_pool(pool_size)
        except Exception as exc:
            logger.warning(
                "%s crawler failed to start (%s); skipping this pass", mode, exc
            )
            return

        self.scraper_client = scraper
        self.scrape_mode = mode
        self._defer_website_failures = defer_failures
        await self._process_items_concurrently(retry_items)

    async def _retry_failed_websites_firecrawl(
        self, exit_stack: AsyncExitStack, items: List[WorkItem]
    ) -> None:
        """Auto mode second pass: re-crawl failed websites via local Firecrawl."""
        retry_items = self._collect_retry_items(items)
        if not retry_items:
            return

        if not await self._firecrawl_available():
            return

        await self._run_retry_pass(
            exit_stack,
            retry_items,
            mode="firecrawl",
            # Stay within the service's configured rendering capacity —
            # oversubscribing just queues requests against the client timeout.
            pool_size=min(
                config.CONCURRENT_SESSIONS, config.FIRECRAWL_MAX_CONCURRENT
            ),
            # Firecrawl failures stay retryable only while a later pass (deep
            # crawl or the research fallback) can still run after this one.
            defer_failures=(
                self._deep_crawl_available() or self._research_fallback_available()
            ),
            # Follow cross-host redirects here (unless disabled): many
            # publishers redirect a brand domain to its parent, and the
            # destination's content is the result we actually want.
            reject_redirects=not config.FIRECRAWL_FOLLOW_REDIRECTS,
        )

    def _deep_crawl_available(self) -> bool:
        """Check whether the deep (headless Chrome) crawler can run on this host."""
        podman = getattr(config, "DEEP_SCRAPE_PODMAN_BINARY", "podman")
        if shutil.which(podman) is None:
            return False
        try:
            import selenium  # noqa: F401
        except ImportError:
            return False
        return True

    def _is_antibot_failure(self, domain: str) -> bool:
        # A second headless-Chrome pass from the same datacenter IP can't
        # beat an anti-bot block — it just re-hammers the site.
        return is_antibot_error(self.progress_tracker.get_domain_error(domain))

    async def _retry_failed_websites_deep(
        self, exit_stack: AsyncExitStack, items: List[WorkItem]
    ) -> None:
        """Auto mode last pass: re-crawl failed websites with headless Chrome."""
        retry_items = self._collect_retry_items(items)
        if not retry_items:
            return

        if not self._deep_crawl_available():
            logger.info(
                "%d websites still failed, but the deep crawler is "
                "unavailable on this host (podman/selenium missing) — keeping "
                "earlier results",
                len(retry_items),
            )
            return

        # Skip domains the Firecrawl pass already proved are blocking this IP.
        # Rendering them again from the same IP fails identically and only adds
        # load and ban risk; leave their earlier failure in place.
        if getattr(config, "SKIP_DEEP_RETRY_ON_BLOCK", True):
            blocked = [item for item in retry_items if self._is_antibot_failure(item.identifier)]
            if blocked:
                logger.info(
                    "Skipping the deep crawler for %d anti-bot-blocked site(s) "
                    "(same datacenter IP would be blocked again): %s",
                    len(blocked),
                    ", ".join(sorted(item.identifier for item in blocked)),
                )
                retry_items = [
                    item for item in retry_items if not self._is_antibot_failure(item.identifier)
                ]
            if not retry_items:
                logger.info(
                    "All remaining failures are anti-bot blocks — nothing the "
                    "deep crawler can rescue. The research fallback will "
                    "classify them from public sources; for direct scraping, "
                    "consider a residential proxy (PROXY_SERVER in "
                    "firecrawl/.env)."
                )
                return

        await self._run_retry_pass(
            exit_stack,
            retry_items,
            mode="deep",
            pool_size=1,
            # Deep-pass failures stay retryable only while the research
            # fallback can still run after this pass.
            defer_failures=self._research_fallback_available(),
            # Same policy as the Firecrawl pass: retry passes follow
            # cross-host redirects so publishers that redirect a brand domain
            # to a parent site classify on the destination's content.
            reject_redirects=not config.FIRECRAWL_FOLLOW_REDIRECTS,
        )

    def _research_fallback_available(self) -> bool:
        """Whether the research-based classification fallback may run."""
        return bool(getattr(config, "RESEARCH_FALLBACK_ENABLED", True))

    async def _research_failed_websites(self, items: List[WorkItem]) -> None:
        """Auto mode final pass: classify still-failed websites from research.

        Every scrape backend has had its shot by now; what's left is mostly
        hardened anti-bot publishers this host's IP cannot fetch. A
        web-search-augmented model researches each domain (never touching the
        site from here — zero ban risk) and the regular classifier runs on
        that summary. Domains with no meaningful public footprint stay Failed.
        """
        retry_items = self._collect_retry_items(items)
        if not retry_items:
            return

        if not self._research_fallback_available():
            logger.info(
                "%d websites still failed and the research fallback is "
                "disabled — keeping earlier results",
                len(retry_items),
            )
            return

        logger.info(
            "Auto mode: classifying %d unscrapeable website(s) from external "
            "research (%s)",
            len(retry_items),
            getattr(config, "RESEARCH_MODEL", "perplexity/sonar-pro"),
        )

        self._defer_website_failures = False
        processor = DomainProcessor(
            progress_tracker=self.progress_tracker,
            scraper_client=self.scraper_client,
            openrouter_client=self.openrouter_client,
            reporter=self.reporter,
            results_writer=self.results_writer,
            results_file=self.output_file,
            defer_failures=False,
        )

        concurrency = max(
            1,
            min(
                config.CONCURRENT_SESSIONS,
                getattr(config, "RESEARCH_MAX_CONCURRENT", 4),
            ),
        )
        semaphore = asyncio.Semaphore(concurrency)

        async def research_one(item: WorkItem) -> None:
            async with semaphore:
                if self.shutdown_requested:
                    return
                await processor.process_domain_research(
                    DomainWorkItem(domain=item.identifier)
                )

        results = await asyncio.gather(
            *(research_one(item) for item in retry_items),
            return_exceptions=True,
        )
        for item, result in zip(retry_items, results):
            if isinstance(result, Exception):
                logger.error(
                    "Research fallback task failed for %s: %s",
                    item.identifier,
                    result,
                )

    def _dedupe_output_csv(self) -> None:
        """Collapse retried rows: keep one row per identifier, preferring success.

        The deep-crawl retry appends a second row for each domain it rescues;
        the original Failed row is dropped here so downstream consumers see a
        single, best-effort result per identifier.
        """
        output_path = Path(config.OUTPUT_CSV_PATH)
        if not output_path.exists():
            return

        try:
            with open(output_path, newline="") as f:
                reader = csv.DictReader(f)
                fieldnames = reader.fieldnames
                if not fieldnames or "Domain" not in fieldnames:
                    return
                rows = list(reader)

            best: Dict[str, Dict[str, str]] = {}
            order: List[str] = []
            for row in rows:
                key = row.get("Domain", "")
                if key not in best:
                    order.append(key)
                    best[key] = row
                    continue
                # A later row wins unless it is a failure replacing a success.
                if row.get("Quality") == "Failed" and best[key].get("Quality") != "Failed":
                    continue
                best[key] = row

            if len(best) == len(rows):
                return

            with open(output_path, "w", newline="") as f:
                writer = csv.DictWriter(f, fieldnames=fieldnames)
                writer.writeheader()
                for key in order:
                    writer.writerow(best[key])
            logger.info(
                "Deduplicated output CSV: %d rows -> %d unique identifiers",
                len(rows),
                len(best),
            )
        except Exception as exc:
            logger.warning("Could not dedupe output CSV: %s", exc)

    async def _run_ctv_workflow(self) -> None:
        """Run the CTV (Connected TV) processing workflow."""
        run_start = time.perf_counter()
        reporter_started = False

        try:
            logger.info("Starting CTV (Connected TV) Analysis")
            logger.info(
                "Research model: %s, Classification model: %s",
                getattr(config, "CTV_RESEARCH_MODEL", "perplexity/sonar-pro"),
                getattr(config, "CTV_CLASSIFICATION_MODEL", "~google/gemini-flash-latest"),
            )
            logger.info("CTV concurrency: %s", getattr(config, "CTV_MAX_CONCURRENT", 5))

            # Load CTV input data
            ctv_items = self._load_ctv_input_data()
            summary_before = self.progress_tracker.get_summary()

            total_for_reporter = summary_before.get("total_domains")
            if not total_for_reporter:
                total_for_reporter = len(ctv_items)

            self.reporter = TerminalReporter(
                total=total_for_reporter,
                completed=summary_before.get("processed", 0),
                successes=summary_before.get("successful", 0),
                failures=summary_before.get("errors", 0),
                quiet=self.quiet,
                verbose=self.verbose,
                jsonl_path=self.jsonl_path,
            )
            self.reporter.start()
            reporter_started = True

            if not ctv_items:
                logger.error("No valid CTV items to process")
                self.progress_tracker.print_summary()
                if self.reporter:
                    self.reporter.render_summary(
                        summary_before, time.perf_counter() - run_start
                    )
                return

            # Filter out already processed items
            items_to_process = [
                item
                for item in ctv_items
                if not self.progress_tracker.is_domain_processed(item.identifier)
            ]

            if not items_to_process:
                logger.info("All CTV items have already been processed!")
                self.progress_tracker.print_summary()
                if self.reporter:
                    self.reporter.render_summary(
                        self.progress_tracker.get_summary(),
                        time.perf_counter() - run_start,
                    )
                return

            logger.info(
                "Processing %s CTV apps (skipping %s already processed)",
                len(items_to_process),
                len(ctv_items) - len(items_to_process),
            )

            async with AsyncExitStack() as exit_stack:
                await self._initialize_ctv_clients(exit_stack)
                self._setup_ctv_output_file()

                try:
                    await self._process_ctv_items_concurrently(items_to_process)
                finally:
                    self._teardown_output_file()

            self.progress_tracker.print_summary()
            if self.reporter:
                self.reporter.render_summary(
                    self.progress_tracker.get_summary(),
                    time.perf_counter() - run_start,
                )
            logger.info("CTV analysis completed successfully!")

        except Exception as e:
            logger.error("Fatal error in CTV workflow: %s", e)
            raise
        finally:
            if reporter_started and self.reporter:
                self.reporter.stop()

    def _load_ctv_input_data(self) -> List[CTVWorkItem]:
        """Load and parse CTV input data."""
        try:
            df = _load_input_dataframe(config.INPUT_CSV_PATH)
            df = _normalize_headerless_input_dataframe(df)

            # Check if this looks like a CTV file
            if not is_ctv_input_file(df):
                logger.warning(
                    "Input file does not appear to be a CTV file. "
                    "Expected columns like 'app_name', 'bundle_id', 'platform', etc."
                )

            # Parse CTV work items
            self.ctv_work_items = parse_ctv_work_items(df)
            self.has_ctv_apps = len(self.ctv_work_items) > 0

            # Set total for progress tracking
            self.progress_tracker.set_total_domains(len(self.ctv_work_items))

            logger.info(f"Prepared {len(self.ctv_work_items)} CTV apps for processing")
            return self.ctv_work_items

        except Exception as e:
            logger.error(f"Failed to load CTV input data: {e}")
            raise

    async def _initialize_ctv_clients(self, exit_stack: AsyncExitStack) -> None:
        """Initialize OpenRouter client for CTV processing."""
        logger.info("Initializing CTV API clients...")

        # Initialize OpenRouter client (needed for CTV processing)
        openrouter = OpenRouterClient(
            api_key=getattr(config, "OPENROUTER_API_KEY", None),
            model=getattr(config, "CTV_CLASSIFICATION_MODEL", "~google/gemini-flash-latest"),
            temperature=getattr(config, "CTV_CLASSIFICATION_TEMPERATURE", 0.1),
            max_tokens=getattr(config, "CTV_CLASSIFICATION_MAX_TOKENS", 1500),
            max_retries=getattr(config, "LLM_REQUEST_MAX_RETRIES", 3),
            timeout=getattr(config, "LLM_REQUEST_TIMEOUT", None),
            fallback_model=getattr(config, "CTV_CLASSIFICATION_FALLBACK_MODEL", None),
        )
        self.openrouter_client = await exit_stack.enter_async_context(openrouter)

        logger.info("Testing OpenRouter API connection...")
        if not await self.openrouter_client.test_connection():
            raise RuntimeError(
                "Failed to initialise OpenRouter client. "
                "Check network connectivity and OPENROUTER_API_KEY configuration."
            )

        logger.info("CTV clients initialised successfully")

    async def _process_ctv_items_concurrently(
        self, items_to_process: List[CTVWorkItem]
    ) -> None:
        """Process CTV items concurrently with rate limiting."""
        ctv_concurrency = getattr(config, "CTV_MAX_CONCURRENT", 5)
        semaphore = asyncio.Semaphore(ctv_concurrency)

        # Create CTV processor
        ctv_processor = CTVProcessor(
            progress_tracker=self.progress_tracker,
            openrouter_client=self.openrouter_client,
            reporter=self.reporter,
            results_writer=self.results_writer,
            results_file=self.output_file,
            research_model=getattr(
                config, "CTV_RESEARCH_MODEL", "perplexity/sonar-pro"
            ),
            research_temperature=getattr(config, "CTV_RESEARCH_TEMPERATURE", 0.3),
            research_max_tokens=getattr(config, "CTV_RESEARCH_MAX_TOKENS", 2000),
            classification_model=getattr(
                config, "CTV_CLASSIFICATION_MODEL", "~google/gemini-flash-latest"
            ),
            classification_temperature=getattr(
                config, "CTV_CLASSIFICATION_TEMPERATURE", 0.1
            ),
            classification_max_tokens=getattr(
                config, "CTV_CLASSIFICATION_MAX_TOKENS", 1500
            ),
            request_delay=getattr(config, "CTV_REQUEST_DELAY", 2.0),
        )

        async def process_single_ctv_item(item: CTVWorkItem):
            try:
                async with semaphore:
                    if self.shutdown_requested:
                        return

                    task = asyncio.create_task(ctv_processor.process_ctv_app(item))
                    self.running_tasks.add(task)

                    try:
                        await task
                    except asyncio.CancelledError:
                        task.cancel()
                        await asyncio.gather(task, return_exceptions=True)
                        raise
                    finally:
                        self.running_tasks.discard(task)
            except asyncio.CancelledError:
                return

        # Create tasks for all CTV items
        task_set = {
            asyncio.create_task(process_single_ctv_item(item))
            for item in items_to_process
        }

        if not task_set:
            return

        pending_tasks = set(task_set)
        task_iter = asyncio.as_completed(task_set)

        try:
            while pending_tasks:
                if self.shutdown_requested:
                    logger.info("Shutdown requested, cancelling remaining CTV tasks...")
                    break

                try:
                    next_task = next(task_iter)
                except StopIteration:
                    break

                try:
                    await next_task
                except Exception as e:
                    logger.error(f"CTV task failed: {e}")
                finally:
                    pending_tasks.discard(next_task)
        finally:
            if self.shutdown_requested and pending_tasks:
                for task in pending_tasks:
                    task.cancel()
                await asyncio.gather(*pending_tasks, return_exceptions=True)

            if self.running_tasks:
                logger.info(
                    f"Waiting for {len(self.running_tasks)} remaining CTV tasks to complete..."
                )
                if self.shutdown_requested:
                    for task in list(self.running_tasks):
                        if not task.done():
                            task.cancel()
                await asyncio.gather(*self.running_tasks, return_exceptions=True)
