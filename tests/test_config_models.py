# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Classification-model default and upgrades of previously shipped defaults."""

import json
import os

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

from config import DEFAULT_LLM_MODEL, PREVIOUS_DEFAULT_LLM_MODELS, Config  # noqa: E402
from web_service import PREVIOUS_RECOMMENDED_MODELS, RECOMMENDED_MODEL  # noqa: E402


def test_recommended_model_matches_config_default():
    assert RECOMMENDED_MODEL == DEFAULT_LLM_MODEL == "~openai/gpt-luna-latest"
    assert set(PREVIOUS_RECOMMENDED_MODELS) == PREVIOUS_DEFAULT_LLM_MODELS


def test_previous_shipped_default_upgrades(tmp_path, capsys):
    path = tmp_path / "config.json"
    path.write_text(
        json.dumps(
            {
                "llm_model": "~google/gemini-flash-latest",
                "ctv_classification_model": "~google/gemini-flash-latest",
            }
        ),
        encoding="utf-8",
    )

    cfg = Config(str(path))

    assert cfg.LLM_MODEL == DEFAULT_LLM_MODEL
    assert cfg.CTV_CLASSIFICATION_MODEL == DEFAULT_LLM_MODEL
    assert "previous shipped default" in capsys.readouterr().out


def test_explicit_model_is_kept(tmp_path, capsys):
    path = tmp_path / "config.json"
    path.write_text(
        json.dumps({"llm_model": "openai/gpt-5.6-luna-pro"}),
        encoding="utf-8",
    )

    cfg = Config(str(path))

    assert cfg.LLM_MODEL == "openai/gpt-5.6-luna-pro"
    assert cfg.CTV_CLASSIFICATION_MODEL == DEFAULT_LLM_MODEL
    assert "previous shipped default" not in capsys.readouterr().out


def test_dead_model_upgrades_to_the_current_default(tmp_path, capsys):
    path = tmp_path / "config.json"
    path.write_text(json.dumps({"llm_model": "x-ai/grok-4.1-fast"}), encoding="utf-8")

    cfg = Config(str(path))

    assert cfg.LLM_MODEL == DEFAULT_LLM_MODEL
    assert "deprecated on OpenRouter" in capsys.readouterr().out
