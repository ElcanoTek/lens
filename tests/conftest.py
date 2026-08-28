# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Shared helpers and suite-wide filesystem isolation for the Lens test suite."""

from __future__ import annotations

import importlib
import os
from pathlib import Path
from typing import Iterator, Tuple

import pytest

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")


# ---------------------------------------------------------------------------
# Suite-wide filesystem isolation
# ---------------------------------------------------------------------------
#
# Lens keeps its runtime state — uploaded inputs, job outputs, per-job logs and
# progress files, and the `_jobs.json` job history — in a gitignored
# `managed-files/` tree inside the checkout. Two properties of that make test
# leakage into it uniquely dangerous:
#
#   * `managed-files/` is gitignored, so `git status` stays clean no matter what
#     a test wrote. Any git-based safety check is *structurally blind* to it.
#   * What lives there is runtime state nobody diffs, so a clobber is silent.
#
# That combination let one real bug live from the initial public release: a test
# constructed a `web_service.JobManager()` without redirecting the module-level
# `OUTPUT_DIR`. `JobManager._save_jobs()` reads that global at call time and
# mkdirs it, and both `create_job()` and `cancel_job()` call it — so running the
# suite replaced a real operator's job history with the test's synthetic job.
#
# The defence here is PREVENTION, not detection. An autouse fixture repoints
# every module global and config attribute that names a real on-disk location at
# a per-test `tmp_path` subdirectory, for every test, by default. A test that
# forgets to redirect therefore *cannot* touch the checkout, because nothing
# real points at it any more.
#
# Please do not "improve" this into a fixture that inspects `managed-files/`
# after the fact and fails the test that dirtied it. That was considered and
# rejected: it reports damage instead of preventing it, and it has a real
# false-positive mode — on a box where a local dev server is concurrently
# mutating job state, the run fails and blames the wrong test. A guard that
# cries wolf gets disabled, and then the whole class is open again.
#
# ADDING A NEW PATH GLOBAL: register it below. `tests/test_path_isolation.py`
# fails if a module global naming a path under the checkout is neither
# registered here nor listed there as deliberately read-only, so a new one
# cannot be added without a decision being recorded.

# Module globals holding a real on-disk location:
# (module name, attribute name, path relative to the per-test sandbox root).
_MODULE_PATH_GLOBALS: Tuple[Tuple[str, str, str], ...] = (
    ("web_service", "INPUT_DIR", "managed-files/inputs"),
    ("web_service", "OUTPUT_DIR", "managed-files/outputs"),
)

# Attributes on the `config` singleton holding a real on-disk location. Their
# shipped defaults are bare relative filenames, so they resolve against the
# process CWD — the repo root when pytest is run the usual way.
# (attribute name, path relative to the per-test sandbox root).
_CONFIG_PATH_ATTRS: Tuple[Tuple[str, str], ...] = (
    ("INPUT_CSV_PATH", "input.csv"),
    ("OUTPUT_CSV_PATH", "output.csv"),
    ("PROGRESS_FILE_PATH", "progress.json"),
    ("LOG_FILE_PATH", "site_analysis.log"),
)

#: Tests marked with this opt out of the isolation above and see the real paths.
REAL_MANAGED_FILES_MARKER = "real_managed_files"

#: `tmp_path` subdirectory the isolated tree is built under. A subdirectory
#: rather than `tmp_path` itself so a test that inspects its own `tmp_path`
#: is not surprised by files the fixture put there.
_SANDBOX_DIRNAME = "_lens_isolated"


def pytest_configure(config: pytest.Config) -> None:
    config.addinivalue_line(
        "markers",
        f"{REAL_MANAGED_FILES_MARKER}: opt out of the autouse managed-files "
        "isolation and run against the real repo-relative paths. State the "
        "reason in the test — this lets a test write into the checkout.",
    )


@pytest.fixture(autouse=True)
def isolate_managed_paths(
    request: pytest.FixtureRequest, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> Iterator[Path]:
    """Repoint every real on-disk path at a per-test sandbox.

    Autouse, so it applies to every test whether or not the test knows about
    it. A test needing the real paths must say so with
    ``@pytest.mark.real_managed_files``.

    Yields the sandbox root, so a test can assert against it if it wants to.
    """
    sandbox = tmp_path / _SANDBOX_DIRNAME

    if request.node.get_closest_marker(REAL_MANAGED_FILES_MARKER) is not None:
        yield sandbox
        return

    for module_name, attribute, relative in _MODULE_PATH_GLOBALS:
        module = importlib.import_module(module_name)
        # getattr without a default on purpose: if the global is renamed or
        # removed, this fixture must fail loudly rather than silently stop
        # protecting the checkout.
        current = getattr(module, attribute)
        assert isinstance(current, Path), (
            f"{module_name}.{attribute} is registered as a path global but is "
            f"{type(current).__name__}; update _MODULE_PATH_GLOBALS."
        )
        target = sandbox / relative
        target.mkdir(parents=True, exist_ok=True)
        monkeypatch.setattr(module, attribute, target)

    config_module = importlib.import_module("config")
    config_singleton = config_module.config
    for attribute, relative in _CONFIG_PATH_ATTRS:
        getattr(config_singleton, attribute)  # fail loudly on a rename
        target = sandbox / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        monkeypatch.setattr(config_singleton, attribute, str(target))

    yield sandbox


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
