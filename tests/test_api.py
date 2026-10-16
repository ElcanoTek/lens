# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""The programmatic seam: scoped tokens, the REST API, MCP and signed downloads."""

import base64
import csv
import io
import json
import os
import sqlite3
import time
from unittest.mock import AsyncMock

import pytest
from fastapi.testclient import TestClient

import api_tokens
import lens_api
import lens_mcp
import web_service

LENS_HOST = "lens.example.test"


@pytest.fixture
def client(monkeypatch):
    monkeypatch.setattr(web_service, "manager", web_service.JobManager())
    monkeypatch.setattr(web_service.manager, "worker_loop", AsyncMock())
    monkeypatch.setattr(
        web_service,
        "_model_catalog_cache",
        (1e18, {"classify": [], "research": [], "decision": []}),
    )
    monkeypatch.setenv("LENS_PUBLIC_URL", f"https://{LENS_HOST}")
    with TestClient(web_service.app) as test_client:
        yield test_client


def _token(*scopes):
    _, raw = api_tokens.create_token("fleet-test", scopes or api_tokens.SCOPES)
    return raw


def _auth(raw):
    return {"Authorization": f"Bearer {raw}"}


def _rpc(client, raw, method, params=None, msg_id=1):
    body = {"jsonrpc": "2.0", "id": msg_id, "method": method}
    if params is not None:
        body["params"] = params
    return client.post("/mcp", json=body, headers=_auth(raw))


def _call(client, raw, name, arguments=None):
    response = _rpc(client, raw, "tools/call", {"name": name, "arguments": arguments or {}})
    assert response.status_code == 200, response.text
    result = response.json()["result"]
    assert json.loads(result["content"][0]["text"]) == result["structuredContent"]
    return result


def _finish(job, rows):
    """Pretend the pipeline ran: write an output CSV and progress for ``job``."""
    job.status = "completed"
    job.output_file = f"{job.id}_output.csv"
    job.progress_file = f"{job.id}_progress.json"
    job.log_file = f"{job.id}.log"
    with open(web_service.OUTPUT_DIR / job.output_file, "w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=["Domain", "Type", "Quality", "IAB Tier 1"])
        writer.writeheader()
        writer.writerows(rows)
    (web_service.OUTPUT_DIR / job.progress_file).write_text(
        json.dumps(
            {
                "total_domains": len(rows),
                "processed_domains": {r["Domain"]: {} for r in rows},
                "successful_count": len(rows),
                "error_count": 0,
                "cost": {"usd": "0.0123", "calls": 4},
            }
        )
    )
    (web_service.OUTPUT_DIR / job.log_file).write_text("started\nfinished\n")


def _domains(csv_text):
    return [row["Domain"] for row in csv.DictReader(io.StringIO(csv_text))]


ROWS = [
    {"Domain": "example.com", "Type": "WEBSITE", "Quality": "Premium", "IAB Tier 1": "News"},
    {"Domain": "example.org", "Type": "WEBSITE", "Quality": "Standard", "IAB Tier 1": "News"},
    {"Domain": "example.net", "Type": "WEBSITE", "Quality": "Premium", "IAB Tier 1": "Technology"},
]


# --- tokens ------------------------------------------------------------------


def test_tokens_store_only_a_hash_and_revocation_is_immediate():
    token, raw = api_tokens.create_token("fleet-elcano", ["jobs:read"])
    assert raw.startswith("lens_") and len(raw) > 40
    with sqlite3.connect(os.environ["LENS_API_TOKEN_DB"]) as db:
        stored = db.execute("SELECT token_hash, prefix FROM api_tokens").fetchone()
    assert raw not in stored and stored[1] == raw[:12]
    assert api_tokens.verify_token(raw).scopes == ("jobs:read",)
    assert api_tokens.revoke_token(token.id)
    with pytest.raises(api_tokens.TokenError, match="revoked"):
        api_tokens.verify_token(raw)
    with pytest.raises(api_tokens.TokenError):
        api_tokens.verify_token("lens_not-a-real-token")


def test_token_scopes_and_labels_are_validated():
    with pytest.raises(ValueError, match="unknown scope"):
        api_tokens.create_token("x", ["admin"])
    with pytest.raises(ValueError, match="label"):
        api_tokens.create_token("", ["jobs:read"])


def test_unauthenticated_lookups_do_not_create_the_token_database():
    with pytest.raises(api_tokens.TokenError):
        api_tokens.verify_token("lens_anything")
    assert not os.path.exists(os.environ["LENS_API_TOKEN_DB"])


def test_token_cli_create_list_revoke(capsys):
    assert api_tokens.main(["create", "--label", "fleet-a", "--json"]) == 0
    created = json.loads(capsys.readouterr().out)
    assert created["scopes"] == ["jobs:read", "jobs:run"]
    assert api_tokens.main(["list"]) == 0
    listing = capsys.readouterr().out
    assert "fleet-a" in listing and created["token"] not in listing
    assert api_tokens.main(["revoke", str(created["id"])]) == 0
    assert api_tokens.main(["revoke", str(created["id"])]) == 1


# --- REST ----------------------------------------------------------------------


def test_rest_rejects_missing_bad_and_browser_credentials(client):
    response = client.get("/api/v1/jobs")
    assert response.status_code == 401
    assert response.headers["www-authenticate"].startswith("Bearer")
    assert response.json()["code"] == "unauthorized"
    assert client.get("/api/v1/jobs", headers=_auth("lens_wrong")).status_code == 401


def test_rest_scope_is_enforced(client):
    reader = _token("jobs:read")
    response = client.post("/api/v1/jobs", json={"items": ["example.com"]}, headers=_auth(reader))
    assert response.status_code == 403
    assert "jobs:run" in response.json()["error"]
    assert client.get("/api/v1/jobs", headers=_auth(reader)).status_code == 200
    assert client.get("/api/v1/whoami", headers=_auth(reader)).json()["scopes"] == ["jobs:read"]


def test_rest_create_from_items_queues_a_tagged_job(client):
    raw = _token()
    response = client.post(
        "/api/v1/jobs",
        json={
            "name": "Q4 review",
            "items": ["example.com", "Example.com", " example.org "],
            "deep_retry": False,
        },
        headers=_auth(raw),
    )
    assert response.status_code == 201, response.text
    body = response.json()
    assert body["status"] == "queued" and body["submitted_by"] == "fleet-test"
    assert body["breakdown"]["total"] == 2  # trimmed and de-duplicated
    assert body["dashboard_url"] == f"https://{LENS_HOST}/"
    job = web_service.manager.jobs[body["id"]]
    assert job.deep_retry is False and job.research_fallback is True
    assert job.input_file.startswith("Q4 review ") and job.input_file.endswith(".csv")
    assert "via API: fleet-test" in web_service._job_settings_summary(job)
    web_service.manager._load_jobs()  # survives a restart
    assert web_service.manager.jobs[body["id"]].submitted_by == "fleet-test"


def test_rest_multipart_upload(client):
    raw = _token()
    response = client.post(
        "/api/v1/jobs",
        files={"file": ("sites.csv", b"Domain\nexample.com\nexample.org\n", "text/csv")},
        data={"name": "upload", "options": json.dumps({"research_fallback": False})},
        headers=_auth(raw),
    )
    assert response.status_code == 201, response.text
    job = web_service.manager.jobs[response.json()["id"]]
    assert job.input_file == "sites.csv" and job.research_fallback is False


@pytest.mark.parametrize(
    ("payload", "code"),
    [
        ({}, "invalid_argument"),
        ({"items": ["a.com"], "input_file": "x.csv"}, "invalid_argument"),
        ({"input_file": "missing.csv"}, "input_not_found"),
        ({"content_base64": "Zm9v", "filename": "evil.exe"}, "invalid_file"),
        ({"content_base64": "not base64!!", "filename": "x.csv"}, "invalid_file"),
        ({"items": ["a.com"], "llm_model": "vendor/unlisted"}, "invalid_model"),
        (
            {"items": ["a.com"], "custom_categories": {"categories": []}},
            "invalid_custom_categories",
        ),
        ({"items": ["a.com"], "custom_categories": {}}, "invalid_custom_categories"),
        ({"items": ["a.com"], "deep_retry": "maybe"}, "invalid_argument"),
        ({"items": ["a.com"], "ctv": True}, "invalid_argument"),
        ({"items": ["x"] * (lens_api.MAX_INLINE_ITEMS + 1)}, "too_many_items"),
    ],
)
def test_rest_create_fails_fast_with_descriptive_errors(client, payload, code):
    response = client.post("/api/v1/jobs", json=payload, headers=_auth(_token()))
    assert response.status_code in {400, 404, 413}
    assert response.json()["code"] == code
    assert not web_service.manager.jobs


def test_rest_refuses_a_file_with_nothing_to_classify(client):
    content = base64.b64encode(b"Notes\n\n").decode()
    response = client.post(
        "/api/v1/jobs",
        json={"content_base64": content, "filename": "empty.csv"},
        headers=_auth(_token()),
    )
    assert response.status_code == 400 and response.json()["code"] == "no_identifiers"
    assert not web_service.manager.jobs


def test_rest_job_lookup_errors(client):
    raw = _token()
    assert client.get("/api/v1/jobs/nope", headers=_auth(raw)).json()["code"] == "invalid_job_id"
    missing = client.get("/api/v1/jobs/20260101000000-abcdef", headers=_auth(raw))
    assert missing.status_code == 404 and missing.json()["code"] == "job_not_found"


async def _queued(name="t"):
    (web_service.INPUT_DIR / "list.csv").write_text("Domain\nexample.com\n")
    return await web_service.manager.create_job("list.csv", "auto", name, submitted_by="fleet-test")


def test_rest_results_files_and_cancel(client):
    raw = _token()
    job = client.portal.call(_queued)
    early = client.get(f"/api/v1/jobs/{job.id}/results", headers=_auth(raw))
    assert early.status_code == 409 and early.json()["code"] == "not_started"
    detail = client.get(f"/api/v1/jobs/{job.id}", headers=_auth(raw)).json()
    assert detail["queue_position"] == 1 and "get_job" in detail["next_step"]

    _finish(job, ROWS)
    results = client.get(
        f"/api/v1/jobs/{job.id}/results?quality=premium&columns=Domain,Quality&limit=1",
        headers=_auth(raw),
    ).json()
    assert results["summary"]["quality"] == {"Premium": 2, "Standard": 1}
    assert results["summary"]["top_iab_tier_1"] == {"News": 2, "Technology": 1}
    assert results["matching_rows"] == 2 and results["rows"] == [
        {"Domain": "example.com", "Quality": "Premium"}
    ]
    assert "More rows remain" in results["note"]

    output = client.get(f"/api/v1/jobs/{job.id}/files/output", headers=_auth(raw))
    assert output.status_code == 200
    assert _domains(output.text) == ["example.com", "example.org", "example.net"]
    assert client.get(f"/api/v1/jobs/{job.id}/files/output").status_code == 401
    assert (
        client.get(f"/api/v1/jobs/{job.id}/files/categories", headers=_auth(raw)).status_code == 404
    )

    cancel = client.post(f"/api/v1/jobs/{job.id}/cancel", headers=_auth(raw))
    assert cancel.status_code == 409 and cancel.json()["code"] == "not_cancellable"


def test_cancel_a_queued_job(client):
    raw = _token()
    job = client.portal.call(_queued)
    response = client.post(f"/api/v1/jobs/{job.id}/cancel", headers=_auth(raw))
    assert response.status_code == 200 and response.json()["status"] == "cancelled"


# --- signed downloads ---------------------------------------------------------


def test_signed_download_link_is_bearer_free_scoped_and_expires(client, monkeypatch):
    raw = _token()
    job = client.portal.call(_queued)
    missing = client.post(f"/api/v1/jobs/{job.id}/files/output/link", headers=_auth(raw))
    assert missing.status_code == 404 and missing.json()["code"] == "file_missing"
    _finish(job, ROWS)
    link = client.post(f"/api/v1/jobs/{job.id}/files/output/link", headers=_auth(raw)).json()
    assert link["url"].startswith(f"https://{LENS_HOST}/api/v1/downloads/")
    path = link["url"].split(LENS_HOST, 1)[1]
    fetched = client.get(path)
    assert fetched.status_code == 200
    assert _domains(fetched.text) == ["example.com", "example.org", "example.net"]
    assert "attachment" in fetched.headers["content-disposition"]

    payload, signature = path.rsplit("/", 1)[1].split(".")
    tampered = json.loads(api_tokens._unb64(payload))
    tampered["k"] = "log"
    forged = api_tokens._b64(json.dumps(tampered, separators=(",", ":")).encode())
    assert client.get(f"/api/v1/downloads/{forged}.{signature}").status_code == 403

    real_time = time.time
    monkeypatch.setattr(api_tokens.time, "time", lambda: real_time() + 3600)
    expired = client.get(path)
    assert expired.status_code == 403 and "expired" in expired.json()["error"]
    monkeypatch.setattr(api_tokens.time, "time", real_time)

    token = api_tokens.verify_token(raw)
    api_tokens.revoke_token(token.id)
    assert client.get(path).status_code == 403


# --- MCP ------------------------------------------------------------------------


def test_mcp_requires_a_token(client):
    response = client.post("/mcp", json={"jsonrpc": "2.0", "id": 1, "method": "initialize"})
    assert response.status_code == 401
    assert client.get("/mcp").status_code == 405


def test_mcp_handshake(client):
    raw = _token()
    init = _rpc(
        client,
        raw,
        "initialize",
        {
            "protocolVersion": "2025-06-18",
            "capabilities": {},
            "clientInfo": {"name": "fleet", "version": "x"},
        },
    )
    result = init.json()["result"]
    assert result["protocolVersion"] == "2025-06-18"
    assert result["serverInfo"]["name"] == "lens" and "create_job" in result["instructions"]
    assert "mcp-session-id" not in init.headers
    unknown = _rpc(client, raw, "initialize", {"protocolVersion": "1999-01-01"})
    assert unknown.json()["result"]["protocolVersion"] == lens_mcp.SUPPORTED_PROTOCOL_VERSIONS[0]
    # The 2024 HTTP+SSE transport is not served, so it is never agreed to.
    legacy = _rpc(client, raw, "initialize", {"protocolVersion": "2024-11-05"})
    assert legacy.json()["result"]["protocolVersion"] != "2024-11-05"
    note = client.post(
        "/mcp", json={"jsonrpc": "2.0", "method": "notifications/initialized"}, headers=_auth(raw)
    )
    assert note.status_code == 202
    assert _rpc(client, raw, "ping").json()["result"] == {}
    assert _rpc(client, raw, "resources/list").json()["error"]["code"] == -32601
    batch = client.post(
        "/mcp", json=[{"jsonrpc": "2.0", "id": 1, "method": "ping"}], headers=_auth(raw)
    )
    assert batch.status_code == 400


def test_mcp_rejects_foreign_browser_origins(client):
    raw = _token()
    response = client.post(
        "/mcp",
        json={"jsonrpc": "2.0", "id": 1, "method": "ping"},
        headers={**_auth(raw), "Origin": "https://evil.example"},
    )
    assert response.status_code == 403


def test_mcp_tools_are_filtered_by_scope_and_rechecked_on_call(client):
    reader = _token("jobs:read")
    names = {t["name"] for t in _rpc(client, reader, "tools/list").json()["result"]["tools"]}
    assert names == {
        "get_job",
        "list_jobs",
        "get_job_results",
        "get_download_link",
        "list_input_files",
        "get_options",
    }
    refused = _rpc(
        client, reader, "tools/call", {"name": "create_job", "arguments": {"items": ["a.com"]}}
    )
    assert refused.json()["error"]["code"] == -32602
    full = {t["name"]: t for t in _rpc(client, _token(), "tools/list").json()["result"]["tools"]}
    assert set(full) == set(lens_mcp.TOOLS)
    upload = full["create_job"]["inputSchema"]["properties"]["content_base64"]
    # Fleet only offers workspace file references for exactly this annotation.
    assert upload["type"] == "string" and upload["contentEncoding"] == "base64"
    assert full["get_job"]["annotations"]["readOnlyHint"] is True
    assert full["cancel_job"]["annotations"]["destructiveHint"] is True


def test_mcp_end_to_end_job_flow(client):
    raw = _token()
    created = _call(
        client,
        raw,
        "create_job",
        {
            "name": "chat list",
            "content_base64": base64.b64encode(
                b"Domain\nexample.com\nexample.org\nexample.net\n"
            ).decode(),
            "filename": "chat list.csv",
        },
    )
    assert created["isError"] is False
    job_id = created["structuredContent"]["id"]
    assert created["structuredContent"]["breakdown"]["websites"] == 3

    listed = _call(client, raw, "list_jobs", {"status": "queued"})["structuredContent"]
    assert [j["id"] for j in listed["jobs"]] == [job_id]
    inputs = _call(client, raw, "list_input_files")["structuredContent"]["files"]
    assert inputs[0]["name"] == "chat list.csv"

    _finish(web_service.manager.jobs[job_id], ROWS)
    status = _call(client, raw, "get_job", {"job_id": job_id})["structuredContent"]
    assert status["status"] == "completed" and status["progress"]["percent"] == 100.0
    assert status["files"] == ["output", "log", "input"]
    assert status["cost_usd"] == pytest.approx(0.0123)

    results = _call(client, raw, "get_job_results", {"job_id": job_id, "limit": 0})
    assert results["structuredContent"]["rows"] == []
    assert results["structuredContent"]["total_rows"] == 3

    link = _call(client, raw, "get_download_link", {"job_id": job_id})["structuredContent"]
    rows = list(csv.DictReader(io.StringIO(client.get(link["url"].split(LENS_HOST, 1)[1]).text)))
    assert [r["Domain"] for r in rows] == ["example.com", "example.org", "example.net"]

    options = _call(client, raw, "get_options")["structuredContent"]
    assert options["defaults"]["llm_model"] == web_service.RECOMMENDED_MODEL


def test_mcp_tool_errors_are_results_not_protocol_failures(client):
    raw = _token()
    missing = _call(client, raw, "get_job", {"job_id": "20260101000000-abcdef"})
    assert missing["isError"] is True and missing["structuredContent"]["code"] == "job_not_found"
    extra = _call(client, raw, "get_job", {"job_id": "20260101000000-abcdef", "bogus": 1})
    assert "Unknown argument" in extra["structuredContent"]["error"]
    required = _call(client, raw, "get_job")
    assert "Missing required" in required["structuredContent"]["error"]


def test_rest_query_errors_use_the_api_error_shape(client):
    raw = _token()
    # Auth comes first, then the API's own validation (not FastAPI's 422).
    assert client.get("/api/v1/jobs?limit=abc").status_code == 401
    response = client.get("/api/v1/jobs?limit=abc", headers=_auth(raw))
    assert response.status_code == 400 and response.json()["code"] == "invalid_argument"
    job = client.portal.call(_queued)
    _finish(job, ROWS)
    bad = client.get(f"/api/v1/jobs/{job.id}/results?offset=-1", headers=_auth(raw))
    assert bad.status_code == 400 and bad.json()["code"] == "invalid_argument"


def test_library_file_without_identifiers_is_refused_before_queueing(client):
    (web_service.INPUT_DIR / "notes.csv").write_text("Notes\nhello\n")
    response = client.post(
        "/api/v1/jobs", json={"input_file": "notes.csv"}, headers=_auth(_token())
    )
    assert response.status_code == 400 and response.json()["code"] == "no_identifiers"
    assert not web_service.manager.jobs


def test_oversized_multipart_upload_is_refused(client, monkeypatch):
    monkeypatch.setattr(web_service, "MAX_UPLOAD_BYTES", 64)
    response = client.post(
        "/api/v1/jobs",
        files={"file": ("big.csv", b"Domain\n" + b"example.com\n" * 20, "text/csv")},
        headers=_auth(_token()),
    )
    assert response.status_code == 413 and response.json()["code"] == "file_too_large"
    assert not (web_service.INPUT_DIR / "big.csv").exists()


def test_input_names_are_claimed_atomically(client):
    first = web_service._store_unique_input("list.csv", b"Domain\na.com\n")
    same = web_service._store_unique_input("list.csv", b"Domain\na.com\n")
    other = web_service._store_unique_input("list.csv", b"Domain\nb.com\n")
    third = web_service._store_unique_input("list.csv", b"Domain\nc.com\n")
    assert (first, same, other, third) == ("list.csv", "list.csv", "list (2).csv", "list (3).csv")
    assert (web_service.INPUT_DIR / "list (2).csv").read_bytes() == b"Domain\nb.com\n"


def test_concurrent_uploads_with_one_name_keep_their_own_bytes(client):
    import concurrent.futures

    payloads = [f"Domain\nsite{i}.com\n".encode() for i in range(8)]
    with concurrent.futures.ThreadPoolExecutor(8) as pool:
        names = list(pool.map(lambda p: web_service._store_unique_input("same.csv", p), payloads))
    assert len(set(names)) == 8
    for name, payload in zip(names, payloads, strict=True):
        assert (web_service.INPUT_DIR / name).read_bytes() == payload


def _chunked(data: bytes, size: int = 1024):
    for start in range(0, len(data), size):
        yield data[start : start + size]


def test_chunked_bodies_are_capped_while_streaming(client, monkeypatch):
    raw = _token()
    monkeypatch.setattr(lens_mcp, "MAX_BODY_BYTES", 4096)
    big = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "ping", "pad": "x" * 10_000}).encode()
    # A generator body is sent with Transfer-Encoding: chunked (no Content-Length).
    response = client.post(
        "/mcp", content=_chunked(big), headers={**_auth(raw), "Content-Type": "application/json"}
    )
    assert response.status_code == 413
    monkeypatch.setattr(web_service, "MAX_UPLOAD_BYTES", 64)
    rest = client.post(
        "/api/v1/jobs",
        content=_chunked(json.dumps({"items": ["a.com"], "pad": "x" * 2_000_000}).encode(), 65536),
        headers={**_auth(raw), "Content-Type": "application/json"},
    )
    assert rest.status_code == 413 and rest.json()["code"] == "too_large"


def test_settings_report_what_the_run_uses_on_this_instance(client, monkeypatch):
    from config import config

    raw = _token()
    monkeypatch.setattr(config, "LLM_MODEL", "vendor/configured-classifier")
    monkeypatch.setattr(config, "DEEP_RETRY_ENABLED", False)
    created = client.post("/api/v1/jobs", json={"items": ["a.com"]}, headers=_auth(raw)).json()
    assert created["settings"]["llm_model"] == "vendor/configured-classifier"
    assert created["settings"]["deep_retry"] is False
    # An explicit request for the recommended model is kept and passed on.
    explicit = client.post(
        "/api/v1/jobs",
        json={"items": ["b.com"], "llm_model": web_service.RECOMMENDED_MODEL},
        headers=_auth(raw),
    ).json()
    assert web_service.manager.jobs[explicit["id"]].llm_model == web_service.RECOMMENDED_MODEL
    assert explicit["settings"]["llm_model"] == web_service.RECOMMENDED_MODEL
    # An explicit true this instance would ignore is refused, not misreported.
    refused = client.post(
        "/api/v1/jobs", json={"items": ["c.com"], "deep_retry": True}, headers=_auth(raw)
    )
    assert refused.status_code == 400 and "deep_retry" in refused.json()["error"]
    options = client.get("/api/v1/options", headers=_auth(raw)).json()
    assert options["defaults"]["llm_model"] == "vendor/configured-classifier"
    assert options["defaults"]["deep_retry"] is False


def test_paging_reuses_the_whole_file_summary(client, monkeypatch):
    raw = _token()
    job = client.portal.call(_queued)
    _finish(job, ROWS)
    lens_api._SUMMARY_CACHE.clear()
    scans = []
    real_open = open

    def counting_open(path, *args, **kwargs):
        if str(path).endswith("_output.csv"):
            scans.append(path)
        return real_open(path, *args, **kwargs)

    monkeypatch.setattr("builtins.open", counting_open)
    pages = [
        client.get(f"/api/v1/jobs/{job.id}/results?offset={i}&limit=1", headers=_auth(raw)).json()
        for i in range(3)
    ]
    assert [p["rows"][0]["Domain"] for p in pages] == ["example.com", "example.org", "example.net"]
    assert all(p["summary"]["quality"] == {"Premium": 2, "Standard": 1} for p in pages)
    # One summary scan, then one (early-stopping) page read per call.
    assert len(scans) == 1 + 3
    premium = client.get(
        f"/api/v1/jobs/{job.id}/results?quality=Premium&limit=0", headers=_auth(raw)
    ).json()
    assert premium["matching_rows"] == 2 and premium["rows"] == []


def test_chunked_multipart_is_capped_while_parsing(client, monkeypatch):
    monkeypatch.setattr(web_service, "MAX_UPLOAD_BYTES", 64)
    boundary = "lensboundary"
    body = (
        (
            f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="big.csv"\r\n'
            "Content-Type: text/csv\r\n\r\nDomain\n"
        ).encode()
        + b"example.com\n" * 200_000
        + f"\r\n--{boundary}--\r\n".encode()
    )
    response = client.post(
        "/api/v1/jobs",
        content=_chunked(body, 65536),
        headers={**_auth(_token()), "Content-Type": f"multipart/form-data; boundary={boundary}"},
    )
    assert response.status_code == 413 and response.json()["code"] == "too_large"
    assert not web_service.manager.jobs


def test_omitted_decision_model_inherits_this_instances_default(client, monkeypatch):
    from config import config

    monkeypatch.setattr(config, "DECISION_MODEL", "vendor/configured-decisions")
    spec = {"categories": [{"name": "Kids", "type": "boolean", "question": "For kids?"}]}
    created = client.post(
        "/api/v1/jobs",
        json={"items": ["a.com"], "custom_categories": spec},
        headers=_auth(_token()),
    ).json()
    assert web_service.manager.jobs[created["id"]].decision_model is None
    assert created["settings"]["decision_model"] == "vendor/configured-decisions"


def test_unknown_create_options_are_refused(client):
    response = client.post(
        "/api/v1/jobs", json={"items": ["a.com"], "deep_rety": False}, headers=_auth(_token())
    )
    assert response.status_code == 400 and "deep_rety" in response.json()["error"]
    assert not web_service.manager.jobs
    form = client.post(
        "/api/v1/jobs",
        files={"file": ("a.csv", b"Domain\na.com\n", "text/csv")},
        data={"options": json.dumps({"reseach_fallback": False})},
        headers=_auth(_token()),
    )
    assert form.status_code == 400 and "reseach_fallback" in form.json()["error"]


def test_overlong_upload_names_are_invalid_files_not_500s(client):
    response = client.post(
        "/api/v1/jobs",
        json={
            "content_base64": base64.b64encode(b"Domain\na.com\n").decode(),
            "filename": "x" * 300 + ".csv",
        },
        headers=_auth(_token()),
    )
    assert response.status_code == 400 and response.json()["code"] == "invalid_file"


def test_tool_schemas_use_only_ecmascript_regex():
    import re as _re

    def patterns(node):
        if isinstance(node, dict):
            for key, value in node.items():
                if key == "pattern":
                    yield value
                else:
                    yield from patterns(value)
        elif isinstance(node, list):
            for value in node:
                yield from patterns(value)

    found = list(patterns([t["inputSchema"] for t in lens_mcp.TOOLS.values()]))
    assert found
    for pattern in found:
        assert "(?" not in pattern, pattern  # inline flags are not ECMAScript
        _re.compile(pattern)
    upload = _re.compile(
        lens_mcp.TOOLS["create_job"]["inputSchema"]["properties"]["filename"]["pattern"]
    )
    assert upload.match("List.XLSX") and upload.match("a.csv") and not upload.match("a.exe")


def test_mcp_rejects_falsey_non_object_params(client):
    raw = _token()
    for bad in ([], False, 0):
        response = client.post(
            "/mcp",
            json={"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": bad},
            headers=_auth(raw),
        )
        assert response.json()["error"]["code"] == -32602
    response = _rpc(client, raw, "tools/call", {"name": "get_options", "arguments": []})
    assert response.json()["error"]["code"] == -32602


def test_malformed_multipart_gets_the_api_error_shape(client):
    response = client.post(
        "/api/v1/jobs",
        content=b"--x\r\nnot a real part",
        headers={**_auth(_token()), "Content-Type": "multipart/form-data; boundary=x"},
    )
    assert response.status_code in {400, 413}
    assert response.json()["ok"] is False and "code" in response.json()


def test_local_ipv6_hosts_get_http_links(monkeypatch):
    monkeypatch.delenv("LENS_PUBLIC_URL", raising=False)

    class FakeRequest:
        def __init__(self, host):
            self.headers = {"host": host}
            self.base_url = f"http://{host}/"

    assert lens_api.public_base_url(FakeRequest("[::1]:8808")) == "http://[::1]:8808"
    assert lens_api.public_base_url(FakeRequest("127.0.0.1:8808")) == "http://127.0.0.1:8808"
    assert lens_api.public_base_url(FakeRequest("lens.example.com")) == "https://lens.example.com"


def test_log_tail_reads_only_the_end(tmp_path):
    log = tmp_path / "big.log"
    log.write_text("".join(f"line {i}\n" for i in range(200_000)))
    assert lens_api._tail_lines(log, 3) == ["line 199997", "line 199998", "line 199999"]
    assert lens_api._tail_lines(tmp_path / "missing.log", 3) == []


def test_options_report_the_configured_decision_model(client, monkeypatch):
    from config import config

    monkeypatch.setattr(config, "DECISION_MODEL", "vendor/configured-decisions")
    options = client.get("/api/v1/options", headers=_auth(_token())).json()
    assert options["defaults"]["decision_model"] == "vendor/configured-decisions"
