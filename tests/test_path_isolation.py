# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Guards for the suite-wide managed-files isolation in ``tests/conftest.py``.

Two jobs:

1. Prove the autouse ``isolate_managed_paths`` fixture actually works — that a
   test which forgets to redirect ``web_service.OUTPUT_DIR`` writes into its own
   ``tmp_path`` rather than the operator's real ``managed-files/`` tree. This is
   the sabotage experiment from the fix, encoded permanently.
2. Fail if a new module global or config attribute names a real on-disk location
   without being either isolated or explicitly recorded as read-only. The
   original bug survived by omission; the next one would too.
"""

from __future__ import annotations

import ast
import os
from pathlib import Path

import pytest

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

from conftest import (  # noqa: E402
    _CONFIG_PATH_ATTRS,
    _MODULE_PATH_GLOBALS,
    REAL_MANAGED_FILES_MARKER,
)

import web_service  # noqa: E402
from config import config  # noqa: E402

REPO_ROOT = Path(web_service.__file__).resolve().parent

# Module-level names that DO reference an on-disk location under the checkout
# but are deliberately not isolated, each with the reason. Every one of these is
# read-only or is an anchor other paths are derived from.
_READ_ONLY_PATH_GLOBALS: dict[tuple[str, str], str] = {
    ("web_service", "BASE_DIR"): (
        "Anchor for the checkout itself: reads static/ and templates/, and is "
        "the subprocess cwd. Nothing is written through it."
    ),
    ("web_service", "templates"): (
        "Jinja2Templates loader over the shipped templates/ directory; read-only."
    ),
    ("input_detector", "_IANA_TLDS_PATH"): (
        "Bundled read-only TLD snapshot shipped next to the module."
    ),
}

# Substrings in the source of a module-level assignment that mark it as naming a
# filesystem location worth a decision.
_PATH_SOURCE_MARKERS = ("Path(", "__file__", "managed-files")


def _module_level_path_assignments() -> list[tuple[str, str]]:
    """Statically find module-level assignments that name a filesystem location.

    Static (``ast``) rather than dynamic so it needs no import of every module
    and catches a new global whatever its runtime type.
    """
    found: list[tuple[str, str]] = []
    for source_file in sorted(REPO_ROOT.glob("*.py")):
        if source_file.name.startswith("test_"):
            continue
        module_name = source_file.stem
        source = source_file.read_text(encoding="utf-8")
        tree = ast.parse(source, filename=str(source_file))
        for node in tree.body:  # module level only
            if isinstance(node, ast.Assign):
                targets = node.targets
            elif isinstance(node, ast.AnnAssign):
                targets = [node.target]
            else:
                continue
            if node.value is None:
                continue
            value_source = ast.get_source_segment(source, node.value)
            if not value_source or not any(m in value_source for m in _PATH_SOURCE_MARKERS):
                continue
            for target in targets:
                if isinstance(target, ast.Name):
                    found.append((module_name, target.id))
    return found


def test_read_only_exemptions_each_carry_a_reason():
    for entry, reason in _READ_ONLY_PATH_GLOBALS.items():
        assert reason.strip(), f"{entry} is exempted from isolation with no reason given"


def test_every_module_path_global_is_isolated_or_recorded_read_only():
    isolated = {(module, attribute) for module, attribute, _ in _MODULE_PATH_GLOBALS}
    unaccounted = [
        entry
        for entry in _module_level_path_assignments()
        if entry not in isolated and entry not in _READ_ONLY_PATH_GLOBALS
    ]
    assert not unaccounted, (
        "These module globals name a filesystem location but are neither "
        "isolated by tests/conftest.py's _MODULE_PATH_GLOBALS nor recorded as "
        "read-only in _READ_ONLY_PATH_GLOBALS: "
        f"{unaccounted}. Add each to whichever is correct — a path global that "
        "is written to and not isolated is the bug this suite exists to prevent."
    )


def test_every_config_path_attribute_is_isolated():
    isolated = {attribute for attribute, _ in _CONFIG_PATH_ATTRS}
    candidates = {
        name
        for name in dir(config)
        if name.isupper()
        and ("_PATH" in name or "_FILE" in name)
        # Only string values name a location; the booleans that happen to match
        # (REJECT_REDIRECTS-style flags) do not.
        and isinstance(getattr(config, name), str)
    }
    assert candidates <= isolated, (
        "config attributes naming a filesystem location that tests/conftest.py "
        f"does not isolate: {sorted(candidates - isolated)}"
    )


def test_fixture_redirects_web_service_directories(isolate_managed_paths, tmp_path):
    for attribute in ("INPUT_DIR", "OUTPUT_DIR"):
        value = getattr(web_service, attribute)
        assert value.is_relative_to(tmp_path), f"{attribute} escaped tmp_path: {value}"
        assert not value.is_relative_to(REPO_ROOT), f"{attribute} points into the checkout"
        assert value.is_dir(), f"{attribute} was not created"


def test_fixture_redirects_config_paths(tmp_path):
    for attribute, _ in _CONFIG_PATH_ATTRS:
        value = Path(getattr(config, attribute))
        assert value.is_absolute(), f"config.{attribute} is still CWD-relative: {value}"
        assert value.is_relative_to(tmp_path), f"config.{attribute} escaped tmp_path: {value}"


@pytest.mark.asyncio
async def test_job_manager_without_an_explicit_redirect_stays_out_of_the_checkout(tmp_path):
    """The sabotage experiment, encoded.

    This test deliberately does NOT redirect ``web_service.OUTPUT_DIR``. Before
    the autouse fixture, ``create_job()`` + ``cancel_job()`` would rewrite the
    real ``managed-files/outputs/_jobs.json``, replacing an operator's job
    history with this synthetic job. With the fixture, the write lands in
    ``tmp_path`` and the checkout is untouched.
    """
    manager = web_service.JobManager()
    job = await manager.create_job("sample.csv", "direct")
    await manager.cancel_job(job.id)

    state_path = web_service._jobs_state_path()
    assert state_path.exists(), "the job state was not written anywhere"
    assert state_path.is_relative_to(tmp_path)
    assert not state_path.is_relative_to(REPO_ROOT)
    assert job.id in state_path.read_text(encoding="utf-8")


@pytest.mark.real_managed_files
def test_opt_out_marker_restores_the_real_paths():
    """The visible opt-out: this marker is how a test states it wants real paths.

    Asserting only that the marker is wired up — nothing in the suite needs the
    real tree, and nothing should start writing to it.
    """
    assert web_service.OUTPUT_DIR.is_relative_to(REPO_ROOT)
    assert web_service.INPUT_DIR.is_relative_to(REPO_ROOT)
    assert REAL_MANAGED_FILES_MARKER == "real_managed_files"
