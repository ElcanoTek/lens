# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Configuration management for Lens."""

import json
import os
from typing import Dict

from dotenv import load_dotenv

# Load environment variables from .env file
load_dotenv()


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
_DEFAULT_LLM_MODEL = "~google/gemini-flash-latest"


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
            "llm_model": _DEFAULT_LLM_MODEL,
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
            # Full rescue ladder by default (direct → Firecrawl → deep →
            # research). Each rung is probed at runtime and skipped when its
            # backend isn't available, so this degrades gracefully to a plain
            # direct crawl on a bare host. CI pins --scrape-mode direct.
            "scrape_mode": "auto",
            # Firecrawl (self-hosted) settings. In auto mode, websites that
            # fail the direct crawl are retried through this service before
            # falling back to the deep (headless Chrome) crawler.
            "firecrawl_url": "http://127.0.0.1:3002",
            "firecrawl_timeout": 60,  # Per-page timeout in seconds
            "firecrawl_wait_for": 0,  # Extra ms to wait for JS after load
            "firecrawl_max_age_ms": None,  # None = use service default caching
            "firecrawl_enabled": True,  # Master switch for auto-mode retries
            # Match the stack's rendering capacity (MAX_CONCURRENT_PAGES in
            # firecrawl/docker-compose.yaml); more sessions would just queue.
            "firecrawl_max_concurrent": 3,
            # Proxy tier Firecrawl uses per scrape. "basic" makes ONE render
            # attempt and returns whatever it gets. "auto" escalates to a
            # "stealth" proxy whenever a site answers 401/403 — but a
            # self-hosted stack has no stealth proxy backend, so that
            # escalation just re-fetches a hardened site (Cloudflare/DataDome)
            # several more times over 30-90s, always failing with
            # `document_antibot`. "basic" fails those sites in ~1s instead and,
            # critically, stops re-hammering them from this one datacenter IP —
            # repeated hammering is what escalates a soft block into a ban.
            # To actually reach hardened sites, plug a residential proxy into
            # the Firecrawl stack (PROXY_SERVER in firecrawl/.env) rather than
            # setting this to "auto". See firecrawl/.env.example.
            "firecrawl_proxy": "basic",
            # The auto-mode retry passes exist to rescue hard sites, so they
            # follow cross-host redirects that the fast direct pass rejects.
            # Many publishers redirect a brand domain to its parent
            # (foxnewsdigital.com -> foxnews.com, abcnews.go.com -> abcnews.com,
            # msnbc.com -> its network home); classifying the destination's
            # content is the correct, useful result. Set False to keep the
            # strict same-host policy in every pass.
            "firecrawl_follow_redirects": True,
            # Once Firecrawl determines a site is blocking this IP (anti-bot
            # challenge, 401/403), don't escalate it to the deep headless-Chrome
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
            "research_model": "perplexity/sonar-pro",
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
            "ctv_research_model": "perplexity/sonar-pro",  # Perplexity Sonar Pro for research
            "ctv_research_temperature": 0.3,  # Slightly higher for research creativity
            "ctv_research_max_tokens": 2000,  # More tokens for detailed research
            "ctv_classification_model": _DEFAULT_LLM_MODEL,  # Fast model for classification
            "ctv_classification_fallback_model": "~openai/gpt-mini-latest",  # Cross-provider fallback if primary is unavailable
            "ctv_classification_temperature": 0.1,  # Low temperature for consistent classification
            "ctv_classification_max_tokens": 1500,  # Standard classification tokens
            "ctv_max_concurrent": 5,  # Concurrent CTV app processing
            "ctv_request_delay": 1.0,  # Delay between requests
            "ctv_max_retries": 3,  # Retry count for failed requests
            "ctv_retry_delay": 2.0,  # Delay between retries
        }

        # Load from config file if it exists
        if os.path.exists(self.config_file):
            try:
                with open(self.config_file, "r") as f:
                    file_config = json.load(f)
                default_config.update(file_config)
            except (json.JSONDecodeError, IOError) as e:
                print(f"Warning: Could not load config file {self.config_file}: {e}")

        if default_config.get("user_agent") in _LEGACY_USER_AGENTS:
            default_config["user_agent"] = DEFAULT_USER_AGENT

        for model_key in ("llm_model", "ctv_classification_model"):
            if default_config.get(model_key) in _DEAD_LLM_MODELS:
                print(
                    f"Warning: configured {model_key} "
                    f"{default_config[model_key]!r} is deprecated on OpenRouter; "
                    f"using {_DEFAULT_LLM_MODEL!r} instead"
                )
                default_config[model_key] = _DEFAULT_LLM_MODEL

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
