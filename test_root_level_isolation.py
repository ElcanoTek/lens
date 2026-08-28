# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Proof that the managed-files isolation reaches ROOT-LEVEL test modules.

pytest collects tests from ``tests/`` *and* from the repo root
(``test_script.py``, ``test_config.py``, and this file). A conftest only applies
to its own directory and below, so while the autouse ``isolate_managed_paths``
fixture lived in ``tests/conftest.py`` a root-level test ran with the real
``managed-files/`` paths — measured: a root-level ``JobManager`` create+cancel
replaced a seeded 3-job operator history with 1 synthetic job, and the test
passed green.

This module is the regression guard for that. It is deliberately at the root and
uses no explicit redirect: the assertions below only hold if the fixture is
defined in the root ``conftest.py``. Move it back down into ``tests/`` and these
tests fail instead of the gap silently re-opening.

It also needs no ``OPENROUTER_API_KEY`` stub or ``sys.path`` prelude — the root
conftest is loaded before any test module is imported, which is the other thing
that only works from the root.
"""

from __future__ import annotations

from pathlib import Path

import web_service
from config import config

REPO_ROOT = Path(web_service.__file__).resolve().parent


def test_root_level_test_gets_the_sandbox(tmp_path):
    for attribute in ("INPUT_DIR", "OUTPUT_DIR"):
        value = getattr(web_service, attribute)
        assert value.is_relative_to(tmp_path), (
            f"{attribute} escaped tmp_path in a root-level test module: {value}. "
            "The isolation fixture must be defined in the ROOT conftest.py."
        )
        assert not value.is_relative_to(REPO_ROOT), f"{attribute} points into the checkout"


def test_root_level_test_gets_isolated_config_paths(tmp_path):
    for attribute in ("INPUT_CSV_PATH", "OUTPUT_CSV_PATH", "PROGRESS_FILE_PATH", "LOG_FILE_PATH"):
        value = Path(getattr(config, attribute))
        assert value.is_relative_to(tmp_path), (
            f"config.{attribute} escaped tmp_path in a root-level test module: {value}"
        )


async def test_root_level_job_manager_stays_out_of_the_checkout(tmp_path):
    """The exact shape that caused the original damage, from the root this time."""
    manager = web_service.JobManager()
    job = await manager.create_job("sample.csv", "direct")
    await manager.cancel_job(job.id)

    state_path = web_service._jobs_state_path()
    assert state_path.exists(), "the job state was not written anywhere"
    assert state_path.is_relative_to(tmp_path)
    assert not state_path.is_relative_to(REPO_ROOT)
