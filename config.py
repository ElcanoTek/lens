# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Configuration management for Lens."""

import json
import os
from typing import Dict

from dotenv import load_dotenv


def _load_dotenv() -> None:
    """Load `.env` when this process can read it.

    systemd injects EnvironmentFile into the web process. Job subprocesses
    inherit that environment and also call this on import. A root-owned 0600
    `.env` (the usual result of `lens env edit`) raises PermissionError here
    and kills the job before it can log, while the dashboard stays up.
    """
    try:
        load_dotenv()
    except OSError:
        return


_load_dotenv()


# Keep this looking like a current browser: an ancient Chrome version is
# itself a bot signal to basic filters.
DEFAULT_USER_AGENT = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"
)

# Exact default strings shipped by older releases. A persisted config.json
# that still carries one of these meant "the default", not a deliberate
# choice — upgrade it so deployed boxes don't keep advertising a 2021 browser.
_LEGACY_USER_AGENTS = {
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
}

# Model IDs that OpenRouter now permanently 404s (deprecated/removed). A
# config.json still pinning one of these makes every run burn a failed
# request and silently swap to the fallback model — upgrade the pin to the
# current default instead. Only add entries here once the ID is dead for
# good; a merely-unfashionable model is a deliberate choice we must respect.
_DEAD_LLM_MODELS = {
    "x-ai/grok-4.1-fast",
}
# OpenRouter's moving alias for the GPT Luna family. It currently resolves
# to openai/gpt-5.6-luna ($0.20/$1.20 per million), inside the dashboard caps.
DEFAULT_LLM_MODEL = "~openai/gpt-luna-latest"
# Exact strings older releases wrote as the shipped default. A config.json
# that still has one of these accepted the default; it did not pick a model.
# Any other slug is left alone.
PREVIOUS_DEFAULT_LLM_MODELS = {
    "~google/gemini-flash-latest",
}
# Custom categories: OpenRouter's moving alias for TypeSafe's Jev decision model.
DEFAULT_DECISION_MODEL = "~typesafe/jev-latest"
# Search-grounded research for blocked websites and CTV apps. Sonar matched
# Sonar Pro's quality tiers on 96% of a production CTV sample at about a third
# of the cost; see docs/RESEARCH_MODELS.md.
DEFAULT_RESEARCH_MODEL = "perplexity/sonar"


class Config:
    """Configuration class that loads settings from environment variables and config file."""

    def __init__(self, config_file: str = "config.json"):
        self.config_file = config_file
        self._load_config()

    def _load_config(self):
        """Load configuration from environment variables and config file."""

        # Default configuration
        default_config = {
            "input_csv_path": "input.csv",
            "output_csv_path": "output.csv",
            "progress_file_path": "progress.json",
            "log_file_path": "site_analysis.log",
            "concurrent_sessions": 5,
            "llm_model": DEFAULT_LLM_MODEL,
            "llm_fallback_model": "~openai/gpt-mini-latest",
            "llm_temperature": 0.1,
            "llm_max_tokens": 1500,
            "llm_request_max_retries": 3,
            "llm_request_timeout": 60,
            "request_timeout": 30,
            "max_retries": 3,
            "retry_delay": 2,
            "session_timeout": 300,
            "user_agent": DEFAULT_USER_AGENT,
            "min_content_length": 500,
            # Full rescue ladder by default (direct → deep → research). The
            # deep rung is probed at runtime and skipped when Podman/Selenium
            # aren't available, so this degrades gracefully to direct +
            # research on a bare host. CI pins --scrape-mode direct.
            "scrape_mode": "auto",
            # The auto-mode deep retry pass exists to rescue hard sites, so it
            # follows cross-host redirects that the fast direct pass rejects.
            # Many publishers redirect a brand domain to its parent
            # (foxnewsdigital.com -> foxnews.com, abcnews.go.com -> abcnews.com);
            # classifying the destination's content is the useful result. Set
            # False to keep the strict same-host policy in every pass.
            # (Formerly firecrawl_follow_redirects, which is still honoured.)
            "retry_follow_redirects": True,
            # Once the direct pass shows a site is blocking this IP (anti-bot
            # challenge, 401/403/429), don't escalate it to the deep headless-Chrome
            # pass: that pass renders from the SAME datacenter IP, so it fails
            # identically while adding load and ban risk. It still runs for
            # sites that failed for other reasons (timeouts, service hiccups).
            "skip_deep_retry_on_block": True,
            # Research fallback: auto mode's final pass. Websites every scrape
            # backend failed on (typically hardened anti-bot publishers like
            # wsj.com or reuters.com) are classified from a web-search research
            # summary instead of being marked Failed. The target site is never
            # contacted from this host, so this rescues blocked sites with zero
            # ban risk. Domains the research model finds nothing about stay
            # Failed rather than being guessed at. Rows rescued this way carry
            # Scrape_Mode="research" so downstream consumers know provenance.
            "research_fallback_enabled": True,
            "research_model": DEFAULT_RESEARCH_MODEL,
            # Decision model for custom categories (OpenRouter Decisions API)
            "decision_model": DEFAULT_DECISION_MODEL,
            "research_temperature": 0.2,
            "research_max_tokens": 1500,
            "research_max_concurrent": 4,
            "deep_scrape_podman_binary": "podman",
            "deep_scrape_image": "docker.io/selenium/standalone-chrome:latest",
            "deep_scrape_container_name": "elcano-lens-chromedriver",
            "deep_scrape_port": 4444,
            "deep_scrape_vnc_port": 7900,
            "deep_scrape_extra_args": [],
            "deep_scrape_startup_timeout": 45,
            "deep_scrape_wait_after_load": 1.0,
            "deep_scrape_keep_container": False,
            "deep_scrape_pull_policy": "always",
            # Redirect handling
            "reject_redirects": True,  # Fail URLs that redirect to different hosts
            # iOS API settings (conservative to avoid rate limiting)
            "ios_request_timeout": 30,
            "ios_request_delay": 5.0,  # 5-second delay between requests
            "ios_max_concurrent": 1,  # Sequential processing only (no concurrency)
            "ios_max_retries": 3,
            "ios_retry_delay": 3.0,  # 3-second delay between retries
            # Android scraper settings (conservative to avoid blocking)
            "android_request_timeout": 30,
            "android_request_delay": 10.0,  # 10-second delay between requests
            "android_max_concurrent": 1,  # Sequential processing only (no concurrency)
            "android_max_retries": 3,
            "android_retry_delay": 5.0,  # 5-second delay between retries
            # CTV (Connected TV) processing settings
            "ctv_research_model": DEFAULT_RESEARCH_MODEL,
            "ctv_research_temperature": 0.3,  # Slightly higher for research creativity
            "ctv_research_max_tokens": 2000,  # More tokens for detailed research
            "ctv_classification_model": DEFAULT_LLM_MODEL,  # Fast model for classification
            "ctv_classification_fallback_model": "~openai/gpt-mini-latest",  # Cross-provider fallback if primary is unavailable
            "ctv_classification_temperature": 0.1,  # Low temperature for consistent classification
            "ctv_classification_max_tokens": 1500,  # Standard classification tokens
            "ctv_max_concurrent": 5,  # Concurrent CTV app processing
            "ctv_request_delay": 1.0,  # Delay between requests
            "ctv_max_retries": 3,  # Retry count for failed requests
            "ctv_retry_delay": 2.0,  # Delay between retries
        }

        # Load from config file if it exists
        file_config_keys: set = set()
        if os.path.exists(self.config_file):
            try:
                with open(self.config_file, "r") as f:
                    file_config = json.load(f)
                default_config.update(file_config)
                file_config_keys = set(file_config)
            except (json.JSONDecodeError, IOError) as e:
                print(f"Warning: Could not load config file {self.config_file}: {e}")

        # Firecrawl was removed: keep old config.json files working.
        if "firecrawl_follow_redirects" in default_config:
            legacy = default_config.pop("firecrawl_follow_redirects")
            if "retry_follow_redirects" not in file_config_keys:
                default_config["retry_follow_redirects"] = legacy
        for key in [k for k in default_config if k.startswith("firecrawl_")]:
            default_config.pop(key)
        if default_config.get("scrape_mode") == "firecrawl":
            print("Warning: scrape_mode 'firecrawl' was removed; using 'auto' instead")
            default_config["scrape_mode"] = "auto"

        if default_config.get("user_agent") in _LEGACY_USER_AGENTS:
            default_config["user_agent"] = DEFAULT_USER_AGENT

        for model_key in ("llm_model", "ctv_classification_model"):
            configured = default_config.get(model_key)
            if configured in _DEAD_LLM_MODELS or configured in PREVIOUS_DEFAULT_LLM_MODELS:
                reason = (
                    "is deprecated on OpenRouter"
                    if configured in _DEAD_LLM_MODELS
                    else "is a previous shipped default"
                )
                print(
                    f"Warning: configured {model_key} {configured!r} {reason}; "
                    f"using {DEFAULT_LLM_MODEL!r} instead"
                )
                default_config[model_key] = DEFAULT_LLM_MODEL

        # Set attributes from config
        for key, value in default_config.items():
            setattr(self, key.upper(), value)

        # API Keys (only required if integrations are enabled)
        self.OPENROUTER_API_KEY = os.getenv("OPENROUTER_API_KEY")

        if not self.OPENROUTER_API_KEY:
            raise ValueError(
                "OPENROUTER_API_KEY environment variable is required for classification"
            )

        # CSV fieldnames (unified format for websites and apps)
        self.CSV_FIELDNAMES = [
            "Domain",  # Identifier: domain, app ID, or package name
            "Type",  # Content type: WEBSITE, IOS, or ANDROID
            "App_Name",  # App name (empty for websites)
            "Developer",  # Developer/publisher (empty for websites)
            "Store_Category",  # App store category (empty for websites)
            "Rating",  # App rating (empty for websites)
            "Rating_Count",  # Number of ratings (empty for websites)
            "Downloads",  # Download count - Android only (empty for others)
            "Quality",  # Quality tier: Premium, Standard, Long Tail, Failed
            "Justification",  # Justification for quality rating
            "IAB Tier 1",  # IAB category tier 1
            "IAB Tier 2",  # IAB category tier 2
            "IAB Tier 3",  # IAB category tier 3
            "Description",  # One-sentence description
            "Language",  # Primary language of content
            "Political_Leaning",  # Political orientation: Far Left, Left, Center-Left, Center, Center-Right, Right, Far Right, Non-Political
            "Audience_Size",  # Audience size estimate using t-shirt sizing: XS, S, M, L, XL
            "Bot_Protection",  # Observed bot protection (websites only): None Detected, Moderate, Aggressive, Unknown
            "Content_Length",  # Content length in characters
            "Processing_Time",  # Processing time in seconds
            "Scrape_Mode",  # Scrape mode: direct, deep, ios_api, android_scrape
            "Classifier_Mode",  # Classifier mode: openrouter
            "Scraped_At",  # ISO timestamp of when data was fetched
            "Cost_USD",  # OpenRouter-reported spend for this row's model calls
        ]

        # CTV-specific CSV fieldnames
        self.CTV_CSV_FIELDNAMES = [
            "App_Name",  # CTV app name
            "Type",  # Content type (CTV)
            "Bundle_ID",  # App bundle identifier
            "SSP",  # Supply-side platform source
            "Publisher",  # Publisher/network name
            "Platform",  # CTV platform (Roku, Fire TV, Apple TV, etc.)
            "URL",  # Associated URL
            "Quality",  # Quality tier: Premium, Standard, Long Tail, Failed
            "Justification",  # Justification for quality rating
            "IAB Tier 1",  # IAB category tier 1
            "IAB Tier 2",  # IAB category tier 2
            "IAB Tier 3",  # IAB category tier 3
            "Description",  # One-sentence description
            "Target_Audience",  # Target audience description
            "Content_Type",  # Type of content (streaming, live TV, etc.)
            "Language",  # Primary language of content
            "Political_Leaning",  # Political orientation
            "Network_Affiliation",  # Network affiliation (e.g., NBC, CBS, ABC)
            "Audience_Size",  # Audience size estimate using t-shirt sizing: XS, S, M, L, XL
            "Research_Summary",  # Summary from research step
            "Processing_Time",  # Processing time in seconds
            "Research_Model",  # Model used for research
            "Classification_Model",  # Model used for classification
            "Processed_At",  # ISO timestamp of when data was processed
            "Cost_USD",  # OpenRouter-reported spend for this row's model calls
        ]

    def save_config(self):
        """Save current configuration to config file."""
        config_dict = {}
        for attr in dir(self):
            if not attr.startswith("_") and not callable(getattr(self, attr)):
                if attr != "OPENROUTER_API_KEY":
                    config_dict[attr.lower()] = getattr(self, attr)

        try:
            with open(self.config_file, "w") as f:
                json.dump(config_dict, f, indent=2)
        except IOError as e:
            print(f"Warning: Could not save config file {self.config_file}: {e}")

    def get_openrouter_headers(self) -> Dict[str, str]:
        """Get headers for OpenRouter API requests."""
        return {
            "Authorization": f"Bearer {self.OPENROUTER_API_KEY}",
            "HTTP-Referer": "https://github.com/ElcanoTek/lens",
            "X-Title": "Lens",
            "Content-Type": "application/json",
        }

    def __str__(self) -> str:
        """String representation of config (without sensitive data)."""
        safe_attrs = {}
        for attr in dir(self):
            if not attr.startswith("_") and not callable(getattr(self, attr)):
                if "KEY" not in attr and "ID" not in attr:
                    safe_attrs[attr] = getattr(self, attr)
        return json.dumps(safe_attrs, indent=2)


# Global config instance
config = Config()
