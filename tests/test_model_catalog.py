# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Tests for the web UI's price-capped classification/research model catalog."""

import os

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")
os.environ.setdefault("AUTH_SIGNING_PUBKEY", "")

from web_service import (  # noqa: E402
    RECOMMENDED_MODEL,
    RECOMMENDED_RESEARCH_MODEL,
    _build_model_options,
)


def _model(model_id, prompt, completion, params=("tools", "temperature"), name=None):
    return {
        "id": model_id,
        "name": name or model_id,
        "pricing": {"prompt": str(prompt), "completion": str(completion)},
        "supported_parameters": list(params),
    }


def test_classify_catalog_filters_by_price_cap_and_tool_support():
    models = [
        _model(RECOMMENDED_MODEL, 1.5e-6, 9e-6),
        _model("cheap/model", 1e-7, 4e-7),
        _model("frontier/model", 5e-6, 2.5e-5),  # over both caps
        _model("pricey-output/model", 1e-6, 2e-5),  # over completion cap
        _model("no-tools/model", 1e-7, 4e-7, params=("temperature",)),
        _model("free/model:free", 0, 0),  # rate-limited free tier
    ]

    ids = [option["id"] for option in _build_model_options(models)["classify"]]

    assert RECOMMENDED_MODEL in ids
    assert "cheap/model" in ids
    assert "frontier/model" not in ids
    assert "pricey-output/model" not in ids
    assert "no-tools/model" not in ids
    assert "free/model:free" not in ids


def test_research_catalog_requires_web_search_support():
    websearch = ("web_search_options", "temperature")
    models = [
        _model(RECOMMENDED_RESEARCH_MODEL, 3e-6, 1.5e-5, params=websearch),
        _model("perplexity/sonar", 1e-6, 1e-6, params=websearch),
        _model("no-search/model", 1e-7, 4e-7),  # tools only
        _model("pricey-search/model", 5e-6, 2.5e-5, params=websearch),
    ]

    catalog = _build_model_options(models)
    research_ids = [option["id"] for option in catalog["research"]]

    assert research_ids == [RECOMMENDED_RESEARCH_MODEL, "perplexity/sonar"]
    # A web-search-only model must not leak into the classification list
    # (classification needs function calling).
    classify_ids = [option["id"] for option in catalog["classify"]]
    assert RECOMMENDED_RESEARCH_MODEL not in classify_ids
    assert "no-search/model" in classify_ids


def test_catalog_orders_recommended_then_aliases_then_alpha():
    models = [
        _model("zzz/model", 1e-7, 4e-7),
        _model("~vendor/other-latest", 1e-6, 5e-6),
        _model(RECOMMENDED_MODEL, 1.5e-6, 9e-6),
        _model("aaa/model", 1e-7, 4e-7),
    ]

    ids = [option["id"] for option in _build_model_options(models)["classify"]]

    assert ids == [
        RECOMMENDED_MODEL,
        "~vendor/other-latest",
        "aaa/model",
        "zzz/model",
    ]


def test_catalog_labels_include_pricing():
    models = [_model(RECOMMENDED_MODEL, 1.5e-6, 9e-6, name="Gemini Flash Latest")]
    option = _build_model_options(models)["classify"][0]
    assert "Gemini Flash Latest" in option["label"]
    assert "$1.5/M in" in option["label"]
    assert "$9/M out" in option["label"]


def test_catalog_tolerates_malformed_entries():
    models = [
        {"id": "", "pricing": {}},
        {"id": "bad/pricing", "pricing": {"prompt": "n/a", "completion": None}},
        {"no_id": True},
        _model("ok/model", 1e-7, 4e-7),
    ]
    ids = [option["id"] for option in _build_model_options(models)["classify"]]
    assert ids == ["ok/model"]
