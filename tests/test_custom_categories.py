# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""Custom rubrics: API contract, failure isolation, evidence routing and resume safety."""

import asyncio
import copy
import csv
import io
import json
from pathlib import Path
from unittest.mock import AsyncMock, Mock

import pytest

from app_processor import AppProcessor
from config import config
from ctv_processor import CTVProcessor
from custom_categories import (
    MAX_CONTENT_CHARS,
    TypeSafeCategories,
    category_columns,
    parse_categories,
    validate_categories,
)
from domain_processing import DomainProcessor
from main import parse_args
from orchestration import SiteAnalysisOrchestrator
from progress_tracker import ProgressTracker
from shared_types import CTVWorkItem, DomainWorkItem, WorkItem

SPEC = {
    "categories": [
        {"name": "Sexy", "type": "boolean", "question": "Does it contain erotic themes?"},
        {
            "name": "Purpose",
            "type": "choice",
            "question": "What is its primary purpose?",
            "options": ["Education", "Entertainment", "Unknown"],
        },
    ]
}
ANSWER = {
    "model": "jev-1.13.0",
    "answers": {
        "c0": {"type": "noul", "noul": 0.1},
        "c1": {
            "type": "choice",
            "choice": "Education",
            "confidence": 0.9,
            "probabilities": {"Education": 0.95, "Entertainment": 0.03, "Unknown": 0.02},
        },
    },
}
CLASSIFICATION = {
    "success": True,
    "quality": "Premium",
    "description": "A site",
    "justification": "Useful",
}


class Response:
    def __init__(self, status=200, data=None):
        self.status = status
        self.data = data if data is not None else ANSWER

    async def __aenter__(self):
        return self

    async def __aexit__(self, *args):
        pass

    async def json(self):
        return self.data


def client_with(*responses):
    client = TypeSafeCategories(SPEC, api_key="never-log-this-secret")
    client.session = Mock()
    client.session.post.side_effect = responses
    return client


async def test_batch_api_contract_preserves_raw_probabilities_and_bounds_content():
    client = client_with(Response())
    result = await client.classify(identifier="example.com", content="x" * 30000, source="direct")
    args, kwargs = client.session.post.call_args
    assert args == ("https://api.typesafe.ai/v1/systemone",)
    assert kwargs["headers"] == {"Authorization": "Bearer never-log-this-secret"}
    assert kwargs["allow_redirects"] is False
    payload = kwargs["json"]
    assert len(payload["state"]["content"]) == MAX_CONTENT_CHARS
    assert payload["state"]["truncated"] is True
    assert payload["questions"]["c0"]["type"] == "noul"
    assert payload["questions"]["c1"]["criteria"] == dict.fromkeys(SPEC["categories"][1]["options"])
    assert result["Custom: Sexy"] == "No"
    assert result["P(yes/choice): Sexy"] == 0.1  # P(yes), not confidence in No
    assert result["Custom: Purpose"] == "Education"
    assert result["P(yes/choice): Purpose"] == 0.95
    assert json.loads(result["TypeSafe_Answers"])["Purpose"]["confidence"] == 0.9


@pytest.mark.parametrize(
    "statuses,expected_calls,success",
    [([429, 529, 200], 3, True), ([503, 503, 503], 3, False), ([401], 1, False), ([422], 1, False)],
)
async def test_bounded_retries_and_secret_safe_errors(
    monkeypatch, caplog, statuses, expected_calls, success
):
    monkeypatch.setattr("custom_categories.asyncio.sleep", AsyncMock())
    client = client_with(
        *(
            Response(s, {"error": "never-log-this-secret"}) if s != 200 else Response()
            for s in statuses
        )
    )
    result = await client.classify(identifier="x", content="Evidence", source="research")
    assert client.session.post.call_count == expected_calls
    assert (result["TypeSafe_Status"] == "success") == success
    assert "never-log-this-secret" not in json.dumps(result) + caplog.text


@pytest.mark.parametrize(
    "mutate",
    [
        lambda d: d["answers"].pop("c0"),
        lambda d: d["answers"]["c0"].update(noul=float("nan")),
        lambda d: d["answers"]["c0"].update(noul=1.1),
        lambda d: d["answers"]["c1"].update(choice="Invented"),
        lambda d: d["answers"]["c1"].update(probabilities={"Education": 0.9}),
        lambda d: d["answers"]["c1"].update(
            probabilities=["Education", "Entertainment", "Unknown"]
        ),
    ],
)
async def test_malformed_response_never_becomes_a_false_label(mutate):
    data = copy.deepcopy(ANSWER)
    mutate(data)
    client = client_with(Response(data=data))
    result = await client.classify(identifier="x", content="Evidence", source="direct")
    assert result == {"TypeSafe_Status": "error", "TypeSafe_Error": "Invalid TypeSafe response"}


async def test_no_evidence_skips_network_and_cancellation_propagates():
    client = client_with(asyncio.CancelledError())
    assert (await client.classify(identifier="x", content="", source="direct"))[
        "TypeSafe_Status"
    ] == "skipped"
    client.session.post.assert_not_called()
    with pytest.raises(asyncio.CancelledError):
        await client.classify(identifier="x", content="Evidence", source="direct")


@pytest.mark.parametrize(
    "value",
    [
        {},
        {"categories": []},
        {"categories": SPEC["categories"] * 2},
        {"categories": [{"name": "=formula", "type": "boolean", "question": "Question?"}]},
        {
            "categories": [
                {"name": "Test", "type": "choice", "question": "Question?", "options": ["A", "a"]}
            ]
        },
        {
            "categories": [
                {
                    "name": "Test",
                    "type": "choice",
                    "question": "Question?",
                    "options": ["A", "=1+1"],
                }
            ]
        },
        {"categories": [{"name": "Test", "type": "boolean", "question": " "}]},
    ],
)
def test_invalid_specs_rejected(value):
    with pytest.raises(ValueError):
        validate_categories(value)


def test_json_limits_cli_and_disabled_key_requirement(monkeypatch):
    monkeypatch.delenv("TYPESAFE_API_KEY", raising=False)
    SiteAnalysisOrchestrator()  # optional feature requires no new key
    with pytest.raises(ValueError, match="TYPESAFE_API_KEY"):
        SiteAnalysisOrchestrator(custom_categories=SPEC)
    with pytest.raises(ValueError, match="too large"):
        parse_categories(" " * 32769)
    assert parse_args(["--custom-categories", "labels.json"]).custom_categories == "labels.json"


@pytest.mark.parametrize("ctv", [False, True])
async def test_csv_resume_definitions_and_disabled_run_are_isolated(monkeypatch, ctv):
    monkeypatch.setenv("TYPESAFE_API_KEY", "test-key")
    orchestrator = SiteAnalysisOrchestrator(custom_categories=SPEC)
    await orchestrator._prepare_custom_categories()
    setup = orchestrator._setup_ctv_output_file if ctv else orchestrator._setup_output_file
    setup()
    orchestrator.results_writer.writerow({"Quality": "Premium", "Custom: Sexy": "No"})
    orchestrator._teardown_output_file()
    original = Path(config.OUTPUT_CSV_PATH).read_bytes()
    resumed = SiteAnalysisOrchestrator(custom_categories=SPEC)
    await resumed._prepare_custom_categories()
    (resumed._setup_ctv_output_file if ctv else resumed._setup_output_file)()
    resumed._teardown_output_file()
    assert Path(config.OUTPUT_CSV_PATH).read_bytes() == original
    changed = copy.deepcopy(SPEC)
    changed["categories"][0]["question"] = "Different rubric?"
    for spec in (changed, None):
        with pytest.raises(ValueError, match="changed|different"):
            await SiteAnalysisOrchestrator(custom_categories=spec)._prepare_custom_categories()
    assert Path(config.OUTPUT_CSV_PATH).read_bytes() == original


@pytest.mark.parametrize("kind", ["website", "research", "ios", "android", "ctv"])
@pytest.mark.parametrize("custom_error", [False, True])
async def test_all_pipelines_use_original_evidence_and_keep_primary_results(
    tmp_path, kind, custom_error
):
    out = io.StringIO()
    fields = list(
        config.CTV_CSV_FIELDNAMES if kind == "ctv" else config.CSV_FIELDNAMES
    ) + category_columns(SPEC)
    writer = csv.DictWriter(out, fieldnames=fields)
    writer.writeheader()
    tracker = ProgressTracker(str(tmp_path / "progress.json"))
    custom = Mock()
    custom.classify = AsyncMock(
        return_value={"TypeSafe_Status": "error", "TypeSafe_Error": "TypeSafe HTTP 401"}
        if custom_error
        else TypeSafeCategories(SPEC, api_key="test")._decode(ANSWER)
    )
    router = Mock()
    router.classify_site = AsyncMock(return_value=CLASSIFICATION)
    router.classify_app = AsyncMock(return_value=CLASSIFICATION)
    router.classify_ctv_app = AsyncMock(return_value=CLASSIFICATION)
    evidence = "Original source evidence. " * 30
    router.research_website = AsyncMock(
        return_value={"success": True, "research_content": evidence}
    )
    router.research_ctv_app = AsyncMock(
        return_value={"success": True, "research_content": evidence}
    )
    common = {
        "progress_tracker": tracker,
        "openrouter_client": router,
        "reporter": None,
        "results_writer": writer,
        "results_file": out,
        "category_client": custom,
    }
    if kind in {"website", "research"}:
        scraper = Mock()
        scraper.get_available_session = AsyncMock(return_value=object())
        scraper.scrape_site = AsyncMock(
            return_value={"success": True, "content": evidence, "mode": "direct"}
        )
        processor = DomainProcessor(scraper_client=scraper, **common)
        method = (
            processor.process_domain if kind == "website" else processor.process_domain_research
        )
        await method(DomainWorkItem("example.com"))
    elif kind == "ctv":
        await CTVProcessor(request_delay=0, **common).process_ctv_app(
            CTVWorkItem(app_name="Demo TV")
        )
    else:
        store = Mock()
        store.fetch_app_metadata = AsyncMock(
            return_value={"success": True, "content_for_llm": evidence, "app_name": "Demo"}
        )
        processor = AppProcessor(ios_client=store, android_scraper=store, **common)
        item = (
            WorkItem.from_ios_app_id("123")
            if kind == "ios"
            else WorkItem.from_android_package("com.demo.app")
        )
        await processor.process_app(item)
    rows = list(csv.DictReader(io.StringIO(out.getvalue())))
    assert len(rows) == 1
    assert rows[0]["Quality"] == "Premium"
    assert rows[0]["TypeSafe_Status"] == ("error" if custom_error else "success")
    assert custom.classify.call_args.kwargs["content"] == evidence
    assert (
        custom.classify.call_args.kwargs["source"]
        == {
            "website": "direct",
            "research": "research",
            "ctv": "ctv_research",
            "ios": "ios_store",
            "android": "android_store",
        }[kind]
    )
    assert tracker.get_summary()["successful"] == 1


async def test_failed_primary_analysis_has_no_custom_call(tmp_path):
    custom = Mock(classify=AsyncMock())
    out = io.StringIO()
    writer = csv.DictWriter(out, fieldnames=list(config.CSV_FIELDNAMES) + category_columns(SPEC))
    writer.writeheader()
    router = Mock(
        research_website=AsyncMock(return_value={"success": False, "error": "No evidence"})
    )
    processor = DomainProcessor(
        progress_tracker=ProgressTracker(str(tmp_path / "progress.json")),
        scraper_client=None,
        openrouter_client=router,
        reporter=None,
        results_writer=writer,
        results_file=out,
        category_client=custom,
    )
    await processor.process_domain_research(DomainWorkItem("example.com"))
    custom.classify.assert_not_called()
    row = next(csv.DictReader(io.StringIO(out.getvalue())))
    assert row["TypeSafe_Status"] == "skipped"
    assert row["Custom: Sexy"] == ""


@pytest.mark.parametrize("ctv", [False, True])
async def test_full_orchestrator_run_wires_optional_client_and_resumes(monkeypatch, ctv):
    monkeypatch.setenv("TYPESAFE_API_KEY", "test-key")
    Path(config.INPUT_CSV_PATH).write_text(
        "app_name,bundle_id\nDemo TV,com.demo.tv\n" if ctv else "Domain\nexample.com\n123456789\n"
    )
    run = SiteAnalysisOrchestrator(
        quiet=True, scrape_mode="direct", ctv_mode=ctv, custom_categories=SPEC
    )
    result = run.category_client._decode(ANSWER)
    classify = AsyncMock(return_value=result)
    run.category_client.classify = classify
    router = Mock(
        classify_site=AsyncMock(return_value=CLASSIFICATION),
        classify_app=AsyncMock(return_value=CLASSIFICATION),
        classify_ctv_app=AsyncMock(return_value=CLASSIFICATION),
        research_ctv_app=AsyncMock(
            return_value={"success": True, "research_content": "CTV source text"}
        ),
    )

    async def init(stack):
        run.openrouter_client = router
        run.scraper_client = Mock(
            get_available_session=AsyncMock(return_value=object()),
            scrape_site=AsyncMock(
                return_value={
                    "success": True,
                    "content": "Useful source text. " * 100,
                    "mode": "direct",
                }
            ),
        )
        run.ios_client = Mock(
            fetch_app_metadata=AsyncMock(
                return_value={
                    "success": True,
                    "app_name": "Demo",
                    "content_for_llm": "Store source text",
                }
            )
        )

    monkeypatch.setattr(run, "_initialize_ctv_clients" if ctv else "_initialize_clients", init)
    await run.run()
    with open(config.OUTPUT_CSV_PATH) as file:
        rows = list(csv.DictReader(file))
    assert len(rows) == (1 if ctv else 2)
    assert classify.await_count == len(rows)
    assert all(row["Custom: Purpose"] == "Education" for row in rows)
    assert all(row["TypeSafe_Status"] == "success" for row in rows)
    assert run.category_client.session.closed
    before = Path(config.OUTPUT_CSV_PATH).read_bytes()
    # A new process using identical definitions skips completed items.
    await SiteAnalysisOrchestrator(
        quiet=True, scrape_mode="direct", ctv_mode=ctv, custom_categories=SPEC
    ).run()
    assert Path(config.OUTPUT_CSV_PATH).read_bytes() == before


async def test_enabling_categories_on_existing_plain_output_requires_new_paths(monkeypatch):
    monkeypatch.setenv("TYPESAFE_API_KEY", "test-key")
    original = "Domain,Quality\nexample.com,Premium\n"
    Path(config.OUTPUT_CSV_PATH).write_text(original)
    with pytest.raises(ValueError, match="different"):
        await SiteAnalysisOrchestrator(custom_categories=SPEC)._prepare_custom_categories()
    assert Path(config.OUTPUT_CSV_PATH).read_text() == original
