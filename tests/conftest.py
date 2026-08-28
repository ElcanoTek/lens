# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Helpers shared by the tests under ``tests/``.

Suite-wide filesystem isolation is NOT here — it lives in the repo-root
``conftest.py`` so it also covers the test modules pytest collects from the
root (``test_script.py``, ``test_config.py``). pytest merges conftest layers
along the path, so the autouse ``isolate_managed_paths`` fixture, the
``real_managed_files`` opt-out marker and the path registry all apply to these
tests too, defined exactly once. Do not redefine them here: a same-named
fixture in this file would shadow the root one for ``tests/`` and quietly
re-open the gap for everything else.
"""

from __future__ import annotations


def make_orchestrator(monkeypatch, tmp_path, input_text, *, scrape_mode="auto"):
    """Build a SiteAnalysisOrchestrator against a throwaway input/output set.

    The root conftest already sandboxes these paths for every test; the
    explicit redirects here are deliberate defence in depth, and they also pin
    the input file this orchestrator reads.
    """
    from config import config
    from orchestration import SiteAnalysisOrchestrator

    input_path = tmp_path / "input.csv"
    input_path.write_text(input_text, encoding="utf-8")
    monkeypatch.setattr(config, "INPUT_CSV_PATH", str(input_path))
    monkeypatch.setattr(config, "PROGRESS_FILE_PATH", str(tmp_path / "progress.json"))
    monkeypatch.setattr(config, "OUTPUT_CSV_PATH", str(tmp_path / "output.csv"))
    return SiteAnalysisOrchestrator(quiet=True, scrape_mode=scrape_mode)
