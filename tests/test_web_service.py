# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

import json
import os
from io import BytesIO

import pandas as pd
import pytest
from fastapi.testclient import TestClient

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

# Unified-auth test harness: generate a throwaway Ed25519 keypair, point the
# app at its public key, and mint elcano_auth cookies the same way auth-server
# does. Set before importing web_service so its verifier picks up the key.
import base64 as _b64
import time as _time

from cryptography.hazmat.primitives import serialization as _ser
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey as _Ed

_AUTH_PRIV = _Ed.generate()
os.environ["AUTH_SIGNING_PUBKEY"] = _b64.b64encode(
    _AUTH_PRIV.public_key().public_bytes(_ser.Encoding.Raw, _ser.PublicFormat.Raw)
).decode()


def _auth_cookie(email: str = "tester@elcanotek.com") -> str:
    payload = {
        "email": email,
        "tenant": "elcanotek.com",
        "iat": int(_time.time()),
        "exp": int(_time.time()) + 3600,
    }
    body = _b64.urlsafe_b64encode(json.dumps(payload).encode()).decode().rstrip("=")
    sig = _AUTH_PRIV.sign(body.encode())
    return f"{body}.{_b64.urlsafe_b64encode(sig).decode().rstrip('=')}"


import web_service


def test_safe_filename_rejects_path_traversal():
    with pytest.raises(ValueError):
        web_service._safe_filename("../../etc/passwd")


def test_safe_filename_allows_spaces():
    assert web_service._safe_filename("my file.csv") == "my file.csv"


def test_validate_input_filename_allows_xlsx():
    assert web_service._validate_input_filename("my file.xlsx") == "my file.xlsx"


def test_validate_input_filename_allows_parenthesized_copy_suffix():
    name = "JPMC Official Allowlist 12.16.25 (2).xlsx"
    assert web_service._validate_input_filename(name) == name


def test_safe_filename_sanitizes_disallowed_chars():
    assert web_service._safe_filename("AT&T sites.csv") == "AT_T sites.csv"
    assert web_service._safe_filename("John's list.csv") == "John_s list.csv"
    assert web_service._safe_filename("Q1, Q2.xlsx") == "Q1_ Q2.xlsx"
    # Non-ASCII characters (e.g. accented) are sanitized rather than rejected
    assert web_service._safe_filename("résumé.csv") == "r_sum_.csv"


def test_validate_input_filename_rejects_unsupported_extension():
    with pytest.raises(ValueError, match=r"\.csv or \.xlsx"):
        web_service._validate_input_filename("my file.txt")


def test_verify_input_payload_rejects_non_xlsx_bytes():
    with pytest.raises(ValueError, match="valid .xlsx"):
        web_service._verify_input_payload("workbook.xlsx", b"this is not a zip")


def test_verify_input_payload_accepts_xlsx_magic():
    web_service._verify_input_payload("workbook.xlsx", b"PK\x03\x04rest-of-zip")


def test_verify_input_payload_rejects_binary_csv():
    with pytest.raises(ValueError, match="valid CSV"):
        web_service._verify_input_payload("list.csv", b"\x00\x01binary garbage")


def test_verify_input_payload_accepts_plain_csv():
    web_service._verify_input_payload("list.csv", b"pageURL\nexample.com\n")


@pytest.mark.asyncio
async def test_cancel_queued_job_marks_cancelled(monkeypatch, tmp_path):
    # JobManager._save_jobs() writes to the module-level OUTPUT_DIR, so without
    # this redirect the test overwrites the operator's real job history.
    output_dir = tmp_path / "outputs"
    output_dir.mkdir(parents=True, exist_ok=True)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    manager = web_service.JobManager()
    job = await manager.create_job("sample.csv", "direct")

    await manager.cancel_job(job.id)

    jobs = await manager.list_jobs()
    assert jobs[0].status == "cancelled"


@pytest.mark.asyncio
async def test_run_job_success_updates_artifacts(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)
    (input_dir / "sample.csv").write_text("pageURL\nexample.com\n", encoding="utf-8")

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    class DummyProcess:
        pid = 98765

        async def wait(self):
            return 0

    async def fake_spawn(*args, **kwargs):
        return DummyProcess()

    monkeypatch.setattr(web_service.asyncio, "create_subprocess_exec", fake_spawn)

    manager = web_service.JobManager()
    job = await manager.create_job("sample.csv", "deep")

    await manager._run_job(job.id)

    jobs = await manager.list_jobs()
    assert jobs[0].status == "completed"
    assert jobs[0].output_file is not None
    assert jobs[0].log_file is not None


def test_live_endpoint_returns_progress_and_log(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)
    monkeypatch.setattr(web_service, "manager", web_service.JobManager())

    manager = web_service.manager

    (output_dir / "job-1_progress.json").write_text(
        json.dumps({"summary": {"processed": 3, "total_domains": 10}}),
        encoding="utf-8",
    )
    (output_dir / "job-1.log").write_text("line1\nline2\n", encoding="utf-8")

    with TestClient(web_service.app) as client:
        # TestClient triggers startup_event which resets manager state,
        # so set up the job *after* entering the context.
        job = web_service.Job(
            id="job-1",
            input_file="sample.csv",
            mode="direct",
            status="running",
            progress_file="job-1_progress.json",
            log_file="job-1.log",
        )
        manager.jobs[job.id] = job
        manager.current_job_id = job.id

        client.cookies.set("elcano_auth", _auth_cookie())

        response = client.get("/api/jobs/live")
        assert response.status_code == 200
        payload = response.json()
        assert payload["job"]["id"] == "job-1"
        assert payload["progress"]["processed"] == 3
        assert payload["log_lines"][-1] == "line2"


def test_batch_delete_and_download(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)
    (input_dir / "a.csv").write_text("a", encoding="utf-8")
    (input_dir / "b.csv").write_text("b", encoding="utf-8")

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())

        dl_response = client.post(
            "/files/batch-download",
            data={"destination": "inputs", "filenames": ["a.csv", "b.csv"]},
        )
        assert dl_response.status_code == 200
        assert dl_response.headers["content-type"].startswith("application/zip")

        del_response = client.post(
            "/files/batch-delete",
            data={"destination": "inputs", "filenames": ["a.csv", "b.csv"]},
            follow_redirects=False,
        )
        assert del_response.status_code == 303
        assert not (input_dir / "a.csv").exists()
        assert not (input_dir / "b.csv").exists()


def test_upload_accepts_filename_with_spaces(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())

        response = client.post(
            "/files/upload",
            data={"destination": "inputs"},
            files={"file": ("my file.csv", b"pageURL\nexample.com\n", "text/csv")},
            follow_redirects=False,
        )

        assert response.status_code == 303
        assert (input_dir / "my file.csv").exists()


def test_upload_accepts_xlsx_filename(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())

        xlsx_bytes = BytesIO()
        pd.DataFrame({"pageURL": ["example.com"]}).to_excel(xlsx_bytes, index=False)
        response = client.post(
            "/files/upload",
            data={"destination": "inputs"},
            files={
                "file": (
                    "my workbook.xlsx",
                    xlsx_bytes.getvalue(),
                    "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
                )
            },
            follow_redirects=False,
        )

        assert response.status_code == 303
        assert (input_dir / "my workbook.xlsx").exists()


def test_upload_sanitizes_unusual_filename(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())

        response = client.post(
            "/files/upload",
            data={"destination": "inputs"},
            files={"file": ("AT&T, John's list.csv", b"pageURL\nexample.com\n", "text/csv")},
            follow_redirects=False,
        )

        assert response.status_code == 303
        # `&`, `,`, and `'` are sanitized to underscores; spaces are preserved
        assert (input_dir / "AT_T_ John_s list.csv").exists()


def test_upload_rejects_corrupt_xlsx(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())

        response = client.post(
            "/files/upload",
            data={"destination": "inputs"},
            files={
                "file": ("broken.xlsx", b"not actually a zip archive", "application/octet-stream")
            },
            follow_redirects=False,
        )

        assert response.status_code == 303
        # File should NOT have been written
        assert not (input_dir / "broken.xlsx").exists()
        # Error should be surfaced via the redirect query string
        assert "files_error" in response.headers.get("location", "")


def test_upload_rejects_oversize_file(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)
    # Shrink the cap so we don't have to actually send 100 MB in the test
    monkeypatch.setattr(web_service, "MAX_UPLOAD_BYTES", 1024)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())

        response = client.post(
            "/files/upload",
            data={"destination": "inputs"},
            files={"file": ("big.csv", b"x" * 2048, "text/csv")},
            follow_redirects=False,
        )

        assert response.status_code == 303
        assert not (input_dir / "big.csv").exists()
        location = response.headers.get("location", "")
        assert "files_error" in location
        assert "exceeds" in location.lower() or "limit" in location.lower()


def test_orchestrator_loads_xlsx_input(monkeypatch, tmp_path):
    input_path = tmp_path / "input workbook.xlsx"
    progress_path = tmp_path / "progress.json"
    output_path = tmp_path / "output.csv"

    pd.DataFrame({"pageURL": ["example.com", "example.com", "github.com"]}).to_excel(
        input_path, index=False
    )

    monkeypatch.setattr(web_service, "INPUT_DIR", tmp_path)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", tmp_path)

    from config import config
    from orchestration import SiteAnalysisOrchestrator

    monkeypatch.setattr(config, "INPUT_CSV_PATH", str(input_path))
    monkeypatch.setattr(config, "PROGRESS_FILE_PATH", str(progress_path))
    monkeypatch.setattr(config, "OUTPUT_CSV_PATH", str(output_path))

    orchestrator = SiteAnalysisOrchestrator(quiet=True)
    work_items = orchestrator._load_input_data()

    assert [item.identifier for item in work_items] == ["example.com", "github.com"]


def test_orchestrator_loads_headerless_domain_list(monkeypatch, tmp_path):
    input_path = tmp_path / "domains only.csv"
    progress_path = tmp_path / "progress.json"
    output_path = tmp_path / "output.csv"
    input_path.write_text("example.com\ngithub.com\nexample.com\n", encoding="utf-8")

    from config import config
    from orchestration import SiteAnalysisOrchestrator

    monkeypatch.setattr(config, "INPUT_CSV_PATH", str(input_path))
    monkeypatch.setattr(config, "PROGRESS_FILE_PATH", str(progress_path))
    monkeypatch.setattr(config, "OUTPUT_CSV_PATH", str(output_path))

    orchestrator = SiteAnalysisOrchestrator(quiet=True)
    work_items = orchestrator._load_input_data()

    assert [item.identifier for item in work_items] == ["example.com", "github.com"]


def test_jobs_state_file_not_downloadable(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)
    (output_dir / web_service.JOBS_STATE_FILENAME).write_text("{}", encoding="utf-8")

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())

        response = client.get(f"/files/download/outputs/{web_service.JOBS_STATE_FILENAME}")
        assert response.status_code == 404


def test_jobs_state_file_not_deletable(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)
    state_file = output_dir / web_service.JOBS_STATE_FILENAME
    state_file.write_text("{}", encoding="utf-8")

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())

        response = client.post(
            "/files/delete",
            data={"destination": "outputs", "filename": web_service.JOBS_STATE_FILENAME},
            follow_redirects=False,
        )
        assert response.status_code == 303
        assert state_file.exists()

        batch = client.post(
            "/files/batch-delete",
            data={
                "destination": "outputs",
                "filenames": [web_service.JOBS_STATE_FILENAME],
            },
            follow_redirects=False,
        )
        assert batch.status_code == 303
        assert state_file.exists()


def test_download_invalid_filename_returns_404(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())

        # Backslash path separator is rejected by _safe_filename; must surface
        # as a 404, not an unhandled ValueError (500).
        response = client.get("/files/download/outputs/..%5Cpasswd")
        assert response.status_code == 404


def test_enqueue_error_redirect_is_url_encoded(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())

        response = client.post(
            "/jobs",
            data={"input_file": "bad\\name.csv", "mode": "direct"},
            follow_redirects=False,
        )
        assert response.status_code == 303
        location = response.headers["location"]
        # The ValueError message must be query-encoded — raw spaces in a
        # Location header are invalid and leak unencoded user input.
        assert " " not in location
        assert "error=" in location


def test_load_jobs_rejects_traversal_filenames(monkeypatch, tmp_path):
    output_dir = tmp_path / "outputs"
    output_dir.mkdir(parents=True, exist_ok=True)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    state = {
        "saved_at": "2026-01-01T00:00:00+00:00",
        "jobs": [
            {
                # Traversal in input_file: the whole job must be dropped.
                "id": "evil-1",
                "input_file": "../../etc/passwd",
                "mode": "direct",
                "status": "queued",
                "created_at": "2026-01-01T00:00:00+00:00",
            },
            {
                # Traversal in aux filenames: job kept, fields nulled.
                "id": "good-1",
                "input_file": "list.csv",
                "mode": "direct",
                "status": "completed",
                "created_at": "2026-01-01T00:00:01+00:00",
                "output_file": "good-1_output.csv",
                "progress_file": "../../../etc/shadow",
                "log_file": "good-1.log",
            },
        ],
    }
    (output_dir / web_service.JOBS_STATE_FILENAME).write_text(json.dumps(state), encoding="utf-8")

    manager = web_service.JobManager()
    manager._load_jobs()

    assert "evil-1" not in manager.jobs
    job = manager.jobs["good-1"]
    assert job.output_file == "good-1_output.csv"
    assert job.progress_file is None
    assert job.log_file == "good-1.log"


def test_enqueue_job_with_model_and_research_options(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)
    (input_dir / "list.csv").write_text("Domain\nexample.com\n", encoding="utf-8")

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)
    monkeypatch.setattr(
        web_service,
        "_model_catalog_cache",
        (
            1e18,
            {
                "classify": [{"id": "cheap/model", "label": "Cheap Model"}],
                "research": [{"id": "perplexity/sonar", "label": "Sonar"}],
            },
        ),
    )

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())

        # Checkbox pattern: hidden "off" + checked "on" → fallback enabled.
        response = client.post(
            "/jobs",
            data={
                "input_file": "list.csv",
                "llm_model": "cheap/model",
                "research_fallback": ["off", "on"],
            },
            follow_redirects=False,
        )
        assert response.status_code == 303
        assert "message=" in response.headers["location"]

        # Unchecked checkbox: only the hidden "off" arrives.
        response = client.post(
            "/jobs",
            data={"input_file": "list.csv", "research_fallback": "off"},
            follow_redirects=False,
        )
        assert response.status_code == 303

    jobs = sorted(web_service.manager.jobs.values(), key=lambda j: j.created_at)[-2:]
    assert jobs[0].llm_model == "cheap/model"
    assert jobs[0].research_fallback is True
    assert jobs[1].llm_model is None
    assert jobs[1].research_fallback is False


def test_enqueue_job_rejects_unknown_model(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)
    (input_dir / "list.csv").write_text("Domain\nexample.com\n", encoding="utf-8")

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)
    monkeypatch.setattr(
        web_service,
        "_model_catalog_cache",
        (
            1e18,
            {
                "classify": [{"id": "cheap/model", "label": "Cheap Model"}],
                "research": [{"id": "perplexity/sonar", "label": "Sonar"}],
            },
        ),
    )

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())
        response = client.post(
            "/jobs",
            data={"input_file": "list.csv", "llm_model": "evil/not-in-list"},
            follow_redirects=False,
        )
        assert response.status_code == 303
        assert "error=Unknown+model" in response.headers["location"]


def test_recommended_model_needs_no_override_flag(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)
    (input_dir / "list.csv").write_text("Domain\nexample.com\n", encoding="utf-8")

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())
        response = client.post(
            "/jobs",
            data={
                "input_file": "list.csv",
                "llm_model": web_service.RECOMMENDED_MODEL,
            },
            follow_redirects=False,
        )
        assert response.status_code == 303
        assert "message=" in response.headers["location"]

    job = sorted(web_service.manager.jobs.values(), key=lambda j: j.created_at)[-1]
    assert job.llm_model is None


def test_index_renders_advanced_model_picker(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)
    monkeypatch.setattr(
        web_service,
        "_model_catalog_cache",
        (
            1e18,
            {
                "classify": [
                    {"id": web_service.RECOMMENDED_MODEL, "label": "Gemini Flash Latest"},
                    {"id": "cheap/model", "label": "Cheap Model · $0.1/M in · $0.4/M out"},
                ],
                "research": [
                    {"id": web_service.RECOMMENDED_RESEARCH_MODEL, "label": "Sonar Pro"},
                    {"id": "perplexity/sonar", "label": "Sonar · $1/M in · $1/M out"},
                ],
            },
        ),
    )

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())
        response = client.get("/")
        assert response.status_code == 200
        assert 'id="llm-model-select"' in response.text
        assert "cheap/model" in response.text
        assert 'name="research_fallback"' in response.text


def test_enqueue_job_with_research_model(monkeypatch, tmp_path):
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)
    (input_dir / "list.csv").write_text("Domain\nexample.com\n", encoding="utf-8")

    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)
    monkeypatch.setattr(
        web_service,
        "_model_catalog_cache",
        (1e18, {"classify": [], "research": [{"id": "perplexity/sonar", "label": "Sonar"}]}),
    )

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())

        response = client.post(
            "/jobs",
            data={"input_file": "list.csv", "research_model": "perplexity/sonar"},
            follow_redirects=False,
        )
        assert response.status_code == 303
        assert "message=" in response.headers["location"]

        # The recommended research model needs no override.
        response = client.post(
            "/jobs",
            data={
                "input_file": "list.csv",
                "research_model": web_service.RECOMMENDED_RESEARCH_MODEL,
            },
            follow_redirects=False,
        )
        assert response.status_code == 303

        # Unknown research model is rejected.
        response = client.post(
            "/jobs",
            data={"input_file": "list.csv", "research_model": "evil/model"},
            follow_redirects=False,
        )
        assert "error=Unknown+research+model" in response.headers["location"]

    jobs = sorted(web_service.manager.jobs.values(), key=lambda j: j.created_at)[-2:]
    assert jobs[0].research_model == "perplexity/sonar"
    assert jobs[1].research_model is None
