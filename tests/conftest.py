# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Shared helpers for the Lens test suite."""

import os

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")


def make_orchestrator(monkeypatch, tmp_path, input_text, *, scrape_mode="auto"):
    """Build a SiteAnalysisOrchestrator against a throwaway input/output set."""
    from config import config
    from orchestration import SiteAnalysisOrchestrator

    input_path = tmp_path / "input.csv"
    input_path.write_text(input_text, encoding="utf-8")
    monkeypatch.setattr(config, "INPUT_CSV_PATH", str(input_path))
    monkeypatch.setattr(config, "PROGRESS_FILE_PATH", str(tmp_path / "progress.json"))
    monkeypatch.setattr(config, "OUTPUT_CSV_PATH", str(tmp_path / "output.csv"))
    return SiteAnalysisOrchestrator(quiet=True, scrape_mode=scrape_mode)
