#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""Lens CLI entry point — classify a list of websites, apps or CTV channels."""

import argparse
import asyncio
import logging
import os
import sys
from pathlib import Path
from typing import List, Optional

from config import config
from orchestration import SiteAnalysisOrchestrator

logger = logging.getLogger(__name__)


def _setup_logging(log_file_path: str) -> None:
    """Configure logging handlers for console and file output."""
    Path(log_file_path).parent.mkdir(parents=True, exist_ok=True)

    root_logger = logging.getLogger()
    for handler in list(root_logger.handlers):
        root_logger.removeHandler(handler)

    file_handler = logging.FileHandler(log_file_path)
    file_handler.setLevel(logging.INFO)

    stream_handler = logging.StreamHandler(sys.stdout)
    stream_handler.setLevel(logging.WARNING)

    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s - %(name)s - %(levelname)s - %(message)s",
        handlers=[file_handler, stream_handler],
    )


def parse_args(argv: Optional[List[str]] = None) -> argparse.Namespace:
    """Parse command-line arguments."""

    parser = argparse.ArgumentParser(
        description="Run Lens: classify websites, apps and CTV inventory "
        "into advertising-quality tiers."
    )
    parser.add_argument(
        "--quiet",
        action="store_true",
        help="Suppress per-domain lines; show only live metrics and summary.",
    )
    parser.add_argument(
        "--verbose",
        action="store_true",
        help="Show additional per-domain diagnostics (size, retries, cache status).",
    )
    parser.add_argument(
        "--jsonl",
        metavar="PATH",
        help="Write structured JSONL attempt logs to the specified file.",
    )
    parser.add_argument(
        "--scrape-mode",
        choices=["auto", "direct", "deep", "firecrawl"],
        help=(
            "Select the scraping backend (overrides config.json). "
            "'auto' detects the input type (websites, apps, CTV), starts with the "
            "fast crawler, then retries failed websites with the local Firecrawl "
            "service (if running) and finally the deep crawler. 'firecrawl' "
            "scrapes everything through the local Firecrawl service "
            "(scripts/firecrawl.sh up)."
        ),
    )
    parser.add_argument(
        "--deep-scrape",
        action="store_true",
        help="Convenience flag equivalent to --scrape-mode=deep.",
    )
    parser.add_argument(
        "--allow-redirects",
        action="store_true",
        help="Follow HTTP redirects instead of marking them as failed (default: reject redirects).",
    )
    parser.add_argument(
        "--ctv",
        action="store_true",
        help="Enable CTV (Connected TV) processing mode. Uses two-step AI pipeline with Perplexity for research and fast model for classification.",
    )
    parser.add_argument(
        "--llm-model",
        help=(
            "Override the classification model for this run (an OpenRouter "
            "model ID, e.g. '~google/gemini-flash-latest')."
        ),
    )
    parser.add_argument(
        "--research-fallback",
        choices=["on", "off"],
        help=(
            "Enable/disable the research fallback for this run (overrides "
            "config.json). When a website blocks every crawler, a web-search "
            "model researches the domain and classification runs on that "
            "summary instead of failing the site."
        ),
    )
    parser.add_argument(
        "--research-model",
        help=(
            "Override the research-fallback model for this run (an OpenRouter "
            "model ID with built-in web search, e.g. 'perplexity/sonar-pro')."
        ),
    )
    parser.add_argument(
        "--input-csv",
        help="Override input CSV path for this run.",
    )
    parser.add_argument(
        "--output-csv",
        help="Override output CSV path for this run.",
    )
    parser.add_argument(
        "--progress-file",
        help="Override progress JSON path for this run.",
    )
    parser.add_argument(
        "--log-file",
        help="Override log file path for this run.",
    )
    return parser.parse_args(argv)


async def main(argv: Optional[List[str]] = None):
    """Main entry point."""

    args = parse_args(argv)
    verbose_env = os.getenv("SCRAPER_VERBOSE", "")
    verbose_enabled = args.verbose or verbose_env.lower() in {"1", "true", "yes", "on"}

    if args.quiet:
        verbose_enabled = False

    scrape_mode = str(getattr(config, "SCRAPE_MODE", "direct")).lower()
    if args.scrape_mode:
        scrape_mode = args.scrape_mode
    if args.deep_scrape:
        scrape_mode = "deep"

    # Determine redirect handling: reject by default, allow if --allow-redirects is passed
    reject_redirects = bool(getattr(config, "REJECT_REDIRECTS", True)) and not args.allow_redirects

    if args.llm_model:
        config.LLM_MODEL = args.llm_model
        config.CTV_CLASSIFICATION_MODEL = args.llm_model
    if args.research_fallback:
        config.RESEARCH_FALLBACK_ENABLED = args.research_fallback == "on"
    if args.research_model:
        config.RESEARCH_MODEL = args.research_model
    if args.input_csv:
        config.INPUT_CSV_PATH = args.input_csv
    if args.output_csv:
        config.OUTPUT_CSV_PATH = args.output_csv
    if args.progress_file:
        config.PROGRESS_FILE_PATH = args.progress_file
    if args.log_file:
        config.LOG_FILE_PATH = args.log_file

    _setup_logging(str(getattr(config, "LOG_FILE_PATH", "site_analysis.log")))

    try:
        orchestrator = SiteAnalysisOrchestrator(
            quiet=args.quiet,
            verbose=verbose_enabled,
            jsonl_path=args.jsonl,
            scrape_mode=scrape_mode,
            reject_redirects=reject_redirects,
            ctv_mode=args.ctv,
        )
        await orchestrator.run()
    except KeyboardInterrupt:
        logger.info("Interrupted by user")
    except Exception as e:
        logger.error(f"Application failed: {e}")
        sys.exit(1)


if __name__ == "__main__":
    asyncio.run(main())
