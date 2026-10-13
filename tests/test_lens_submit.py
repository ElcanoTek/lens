"""`lens submit`: spool hand-off from the operator CLI to the dashboard queue."""

import asyncio
import json
import os
import time

import pytest

import submit_job
import web_service

SPEC = {"categories": [{"name": "Sports", "type": "boolean", "question": "Is it about sports?"}]}


@pytest.fixture
def manager(monkeypatch):
    fresh = web_service.JobManager()
    monkeypatch.setattr(web_service, "manager", fresh)
    return fresh


def _entry(name="submit-abc", filename="sites.csv", body=b"Domain\nexample.com\n", request=None):
    entry = web_service.SPOOL_DIR / name
    entry.mkdir(parents=True)
    (entry / filename).write_bytes(body)
    req = {"input_file": filename, "mode": "auto"} | (request or {})
    (entry / "request.json").write_text(json.dumps(req))
    return entry


def _result(entry):
    return json.loads((entry / "result.json").read_text())


def test_spool_request_becomes_a_dashboard_job(manager):
    entry = _entry(
        request={
            "name": "deal sites",
            "custom_categories": SPEC,
            "decision_model": "~typesafe/jev-latest",
            "research_fallback": False,
        }
    )
    asyncio.run(web_service._process_spool_once())

    result = _result(entry)
    job = manager.jobs[result["job_id"]]
    assert job.status == "queued" and list(manager.queue) == [job.id]
    assert job.name == "deal sites"
    assert job.custom_categories == SPEC
    assert job.decision_model == "~typesafe/jev-latest"
    assert job.research_fallback is False
    assert (web_service.INPUT_DIR / "sites.csv").read_bytes() == b"Domain\nexample.com\n"
    assert not (entry / "sites.csv").exists()
    # Persisted like any other job, so it survives a restart.
    saved = json.loads((web_service.OUTPUT_DIR / web_service.JOBS_STATE_FILENAME).read_text())
    assert saved["jobs"][0]["id"] == job.id


def test_spool_ingests_each_request_once(manager):
    _entry()
    asyncio.run(web_service._process_spool_once())
    asyncio.run(web_service._process_spool_once())
    assert len(manager.jobs) == 1


def test_name_clash_with_different_content_keeps_both_files(manager):
    (web_service.INPUT_DIR / "sites.csv").write_bytes(b"Domain\nother.com\n")
    entry = _entry()
    asyncio.run(web_service._process_spool_once())
    assert _result(entry)["input_file"] == "sites (2).csv"
    assert (web_service.INPUT_DIR / "sites.csv").read_bytes() == b"Domain\nother.com\n"


@pytest.mark.parametrize(
    "request_patch, message",
    [
        (
            {
                "custom_categories": {
                    "categories": [{"name": "1bad", "type": "boolean", "question": "q"}]
                }
            },
            "",
        ),
        ({"mode": "nope"}, "Invalid mode"),
        ({"llm_model": "bad model; rm -rf"}, "Invalid model id"),
        ({"input_file": "missing.csv"}, "missing"),
        ({"input_file": "../escape.csv"}, ""),
    ],
)
def test_invalid_requests_are_rejected_without_a_job(manager, request_patch, message):
    entry = _entry(request=request_patch)
    asyncio.run(web_service._process_spool_once())
    result = _result(entry)
    assert "error" in result and message in result["error"]
    assert manager.jobs == {}


def test_empty_input_is_rejected(manager):
    entry = _entry(body=b"")
    asyncio.run(web_service._process_spool_once())
    assert "empty" in _result(entry)["error"]


def test_incomplete_entry_is_left_alone(manager):
    entry = web_service.SPOOL_DIR / "submit-partial"
    entry.mkdir(parents=True)
    (entry / "sites.csv").write_bytes(b"Domain\nexample.com\n")
    asyncio.run(web_service._process_spool_once())
    assert not (entry / "result.json").exists() and manager.jobs == {}


def test_unread_results_are_swept_after_ttl(manager):
    entry = _entry()
    asyncio.run(web_service._process_spool_once())
    old = time.time() - web_service.SPOOL_RESULT_TTL_SECONDS - 5
    os.utime(entry / "result.json", (old, old))
    asyncio.run(web_service._process_spool_once())
    assert not entry.exists()


def _cli_entry(tmp_path, filename="my sites!.csv"):
    entry = tmp_path / "submit-cli"
    entry.mkdir()
    (entry / filename).write_text("Domain\nexample.com\n")
    return entry


def test_cli_writes_request_and_reports_the_job(tmp_path, capsys, monkeypatch):
    entry = _cli_entry(tmp_path)
    cats = entry / "categories.json"
    cats.write_text(json.dumps(SPEC))
    monkeypatch.setenv("LENS_PUBLIC_URL", "https://lens.example.com")

    def fake_service(_seconds):
        req = json.loads((entry / "request.json").read_text())
        assert req["input_file"] == "my sites_.csv" and (entry / "my sites_.csv").exists()
        assert (
            req["mode"] == "ctv" and req["custom_categories"] == SPEC and req["name"] == "CTV run"
        )
        (entry / "result.json").write_text(
            json.dumps({"job_id": "J1", "input_file": req["input_file"]})
        )

    monkeypatch.setattr(submit_job.time, "sleep", fake_service)
    code = submit_job.main(
        [
            "--entry",
            str(entry),
            "--input",
            "my sites!.csv",
            "--ctv",
            "--name",
            "CTV run",
            "--categories",
            str(cats),
        ]
    )
    assert code == 0
    assert "job J1" in capsys.readouterr().out
    assert not entry.exists()


def test_cli_reports_rejection(tmp_path, capsys, monkeypatch):
    entry = _cli_entry(tmp_path, "sites.csv")
    monkeypatch.setattr(
        submit_job.time,
        "sleep",
        lambda _s: (entry / "result.json").write_text('{"error": "bad file"}'),
    )
    assert submit_job.main(["--entry", str(entry), "--input", "sites.csv"]) == 1
    assert "bad file" in capsys.readouterr().err


def test_cli_rejects_invalid_categories_before_queueing(tmp_path):
    entry = _cli_entry(tmp_path, "sites.csv")
    cats = entry / "categories.json"
    cats.write_text("{not json")
    assert (
        submit_job.main(["--entry", str(entry), "--input", "sites.csv", "--categories", str(cats)])
        == 1
    )
    assert not entry.exists()


def test_cli_times_out_when_service_is_down(tmp_path):
    entry = _cli_entry(tmp_path, "sites.csv")
    assert submit_job.main(["--entry", str(entry), "--input", "sites.csv", "--timeout", "0.2"]) == 2
    assert (entry / "request.json").exists()  # still queued for when the service returns
