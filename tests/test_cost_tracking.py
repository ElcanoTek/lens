# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""Run and item spend come from OpenRouter's reported cost, never estimates."""

import asyncio
import csv
import io
import json
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock

import pytest
from openai.types import CompletionUsage

import cost_tracking
import web_service
from app_processor import AppProcessor
from config import config
from ctv_processor import CTVProcessor
from custom_categories import DecisionCategories
from domain_processing import DomainProcessor
from openrouter_client import OpenRouterClient
from progress_tracker import ProgressTracker

SPEC = {"categories": [{"name": "Educational", "type": "boolean", "question": "Teach?"}]}


def run_meter():
    meter = cost_tracking.CostMeter()
    cost_tracking.start_run(meter)
    return meter


def test_meter_snapshot_restores_and_rejects_bad_values():
    meter = cost_tracking.CostMeter()
    meter.add(0.25, "openai/gpt-6-luna")
    meter.add(0.5, "perplexity/sonar-pro")
    meter.add(None, "typesafe")
    snap = meter.snapshot()
    assert snap == {
        "usd": 0.75,
        "calls": 3,
        "unpriced_calls": 1,
        "by_model": {"openai/gpt-6-luna": 0.25, "perplexity/sonar-pro": 0.5},
    }
    restored = cost_tracking.CostMeter.restore(json.loads(json.dumps(snap)))
    restored.add(0.25, "openai/gpt-6-luna")
    assert restored.snapshot()["usd"] == 1.0 and restored.calls == 4
    junk = cost_tracking.CostMeter.restore({"usd": "nan", "calls": -1, "by_model": {"x": -2}})
    assert junk.snapshot() == {"usd": 0.0, "calls": 0, "unpriced_calls": 0, "by_model": {}}
    assert cost_tracking.CostMeter.restore("not a dict").calls == 0


@pytest.mark.parametrize(
    "usage,expected",
    [
        ({"cost": 0.0042}, 0.0042),
        ({"cost": "0.5"}, 0.5),
        ({"cost": -1}, None),
        ({"cost": True}, None),
        ({}, None),
        (None, None),
        (CompletionUsage(prompt_tokens=1, completion_tokens=1, total_tokens=2, cost=0.01), 0.01),
        (CompletionUsage(prompt_tokens=1, completion_tokens=1, total_tokens=2), None),
    ],
)
def test_cost_is_read_from_usage(usage, expected):
    assert cost_tracking.cost_from_usage(usage) == expected


async def test_items_are_isolated_and_nested_entry_points_share_a_meter():
    run = run_meter()
    seen = {}

    @cost_tracking.per_item
    async def research(name):
        cost_tracking.record(0.01, "sonar")

    @cost_tracking.per_item
    async def item(name, cost):
        cost_tracking.record(cost, "luna")
        await asyncio.sleep(0)
        await research(name)  # nested: adds to this item, not a new meter
        seen[name] = cost_tracking.item_cost_usd()

    await asyncio.gather(asyncio.create_task(item("a", 0.1)), asyncio.create_task(item("b", 0.2)))
    assert seen == {"a": "0.110000", "b": "0.210000"}
    assert run.snapshot()["usd"] == pytest.approx(0.32)
    assert cost_tracking.item_cost_usd() == ""  # no item open outside the processors


async def test_every_billed_openrouter_response_is_recorded_including_retried_ones(monkeypatch):
    monkeypatch.setattr("openrouter_client.asyncio.sleep", AsyncMock())
    run = run_meter()
    client = OpenRouterClient(api_key="k")
    bad = SimpleNamespace(usage={"cost": 0.002}, model="openai/gpt-6-luna")
    good = SimpleNamespace(usage={"cost": 0.003}, model="openai/gpt-6-luna")
    monkeypatch.setattr(
        client, "_extract_body_error", lambda r: "provider error 502" if r is bad else None
    )
    call = AsyncMock(side_effect=[bad, good])
    assert await client._call_api_with_retry(call, max_retries=2) is good
    assert run.snapshot() == {
        "usd": 0.005,
        "calls": 2,
        "unpriced_calls": 0,
        "by_model": {"openai/gpt-6-luna": 0.005},
    }


class Response:
    def __init__(self, status=200, data=None):
        self.status, self.data = status, data

    async def __aenter__(self):
        return self

    async def __aexit__(self, *args):
        pass

    async def json(self):
        return self.data


async def test_decision_cost_is_recorded_and_typesafe_fallback_counts_as_unpriced(monkeypatch):
    monkeypatch.setattr("custom_categories.asyncio.sleep", AsyncMock())
    answer = {"answers": {"c0": {"type": "noul", "noul": 0.8}}}
    via_openrouter = {**answer, "model": "typesafe/jev-1.13", "usage": {"cost": 0.0001}}
    direct = {**answer, "model": "jev-1.13.0"}
    run = run_meter()
    client = DecisionCategories(SPEC, api_key="k", typesafe_api_key="t")
    client.session = Mock()
    client.session.post.side_effect = [
        Response(data=via_openrouter),
        Response(402),
        Response(data=direct),
    ]
    for _ in range(2):
        result = await client.classify(identifier="x", content="Evidence", source="direct")
        assert result["Decision_Status"] == "success"
    snap = run.snapshot()
    assert snap["usd"] == 0.0001 and snap["calls"] == 2 and snap["unpriced_calls"] == 1


@pytest.mark.parametrize("processor_cls", [DomainProcessor, AppProcessor, CTVProcessor])
async def test_rows_carry_the_item_cost(processor_cls):
    run_meter()
    processor = object.__new__(processor_cls)
    out = io.StringIO()
    processor.results_writer = csv.DictWriter(out, fieldnames=["Domain", "Cost_USD"])
    processor.results_file = out
    processor.category_client = None

    @cost_tracking.per_item
    async def process():
        cost_tracking.record(0.0123, "openai/gpt-6-luna")
        processor._write_result({"Domain": "example.com"})

    await process()
    assert out.getvalue().strip() == "example.com,0.012300"
    assert "Cost_USD" in config.CSV_FIELDNAMES and "Cost_USD" in config.CTV_CSV_FIELDNAMES


async def test_progress_file_persists_the_run_total(tmp_path):
    path = tmp_path / "progress.json"
    tracker = ProgressTracker(str(path))
    tracker.cost_meter = cost_tracking.CostMeter()
    tracker.cost_meter.add(0.42, "openai/gpt-6-luna")
    await tracker.save_progress()
    saved = json.loads(path.read_text())["cost"]
    assert saved["usd"] == 0.42 and saved["calls"] == 1
    # A resumed run starts from the saved total.
    resumed = ProgressTracker(str(path))
    assert cost_tracking.CostMeter.restore(resumed.progress_data.get("cost")).usd == 0.42


@pytest.mark.parametrize(
    "cost,text,title_bit",
    [
        ({"usd": 0.1534, "calls": 95, "unpriced_calls": 0}, "$0.153", "95 model calls"),
        ({"usd": 12.5, "calls": 9000, "unpriced_calls": 2}, "$12.50+", "2 without a reported cost"),
        ({"usd": 0.0004, "calls": 3, "unpriced_calls": 0}, "<$0.001", "$0.0004"),
        (None, "", ""),
    ],
)
def test_runs_table_shows_run_cost(tmp_path, cost, text, title_bit):
    path = tmp_path / "p.json"
    payload = {"total_domains": 3, "processed_domains": {}}
    if cost is not None:
        payload["cost"] = cost
    path.write_text(json.dumps(payload))
    progress = web_service._read_progress(path)
    assert progress["cost_text"] == text
    assert title_bit in progress["cost_title"]
