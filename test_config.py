# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""
Test configuration for Lens that doesn't require real API keys.
"""

import json
import os
from typing import Dict


class TestConfig:
    """Test configuration class that uses dummy values."""

    # A helper, not a test case. The `Test` prefix makes pytest try to collect
    # it, which it then warns about because of the __init__ below.
    __test__ = False

    def __init__(self, config_file: str = "config.json"):
        self.config_file = config_file
        self._load_config()

    def _load_config(self):
        """Load configuration with test defaults."""
        # Use dummy API keys for testing
        self.OPENROUTER_API_KEY = "test_openrouter_key"

        # Default configuration
        default_config = {
            "input_csv_path": "input.csv",
            "output_csv_path": "output.csv",
            "progress_file_path": "progress.json",
            "log_file_path": "site_analysis.log",
            "concurrent_sessions": 5,
            "llm_model": "google/gemini-2.5-flash-lite",
            "llm_temperature": 0.1,
            "llm_max_tokens": 500,
            "request_timeout": 30,
            "max_retries": 3,
            "retry_delay": 2,
            "session_timeout": 300,
            "user_agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
        }

        # Load from config file if it exists
        if os.path.exists(self.config_file):
            try:
                with open(self.config_file, "r") as f:
                    file_config = json.load(f)
                default_config.update(file_config)
            except (json.JSONDecodeError, IOError):
                pass  # Use defaults if file is invalid

        # Set attributes from config
        for key, value in default_config.items():
            setattr(self, key.upper(), value)

        # CSV fieldnames
        self.CSV_FIELDNAMES = [
            "Domain",
            "Quality",
            "Justification",
            "IAB Tier 1",
            "IAB Tier 2",
            "IAB Tier 3",
            "Description",
            "Content_Length",
            "Processing_Time",
        ]

    def get_openrouter_headers(self) -> Dict[str, str]:
        """Get headers for OpenRouter API requests."""
        return {
            "Authorization": f"Bearer {self.OPENROUTER_API_KEY}",
            "HTTP-Referer": "https://github.com/ElcanoTek/lens",
            "X-Title": "Lens",
            "Content-Type": "application/json",
        }
