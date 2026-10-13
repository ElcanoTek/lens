"""Per-run switch for auto mode's headless-Chrome retry pass."""

import asyncio
import json
import os
from unittest.mock import AsyncMock

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

import submit_job
import web_service
from config import config
from conftest import make_orchestrator


def test_switch_disables_the_deep_pass(monkeypatch, tmp_path):
    orchestrator = make_orchestrator(monkeypatch, tmp_path, "pageURL\nexample.com\n")
    monkeypatch.setattr(config, "DEEP_RETRY_ENABLED", False)
    assert orchestrator._deep_crawl_available() is False


def test_job_passes_deep_retry_off_and_persists_it(monkeypatch):
    async def scenario():
        manager = web_service.JobManager()
        process = AsyncMock()
        process.wait.return_value = 0
        spawn = AsyncMock(return_value=process)
        monkeypatch.setattr(web_service.asyncio, "create_subprocess_exec", spawn)

        off = await manager.create_job("list.csv", "auto", deep_retry=False)
        await manager._run_job(off.id)
        args = spawn.call_args.args
        assert args[args.index("--deep-retry") + 1] == "off"
        assert "headless Chrome retry off" in web_service._job_settings_summary(off)

        on = await manager.create_job("list.csv", "auto")
        await manager._run_job(on.id)
        assert "--deep-retry" not in spawn.call_args.args

        reloaded = web_service.JobManager()
        reloaded._load_jobs()
        assert reloaded.jobs[off.id].deep_retry is False
        assert reloaded.jobs[on.id].deep_retry is True

    asyncio.run(scenario())


def test_spool_request_carries_deep_retry(monkeypatch):
    manager = web_service.JobManager()
    monkeypatch.setattr(web_service, "manager", manager)
    entry = web_service.SPOOL_DIR / "submit-x"
    entry.mkdir(parents=True)
    (entry / "sites.csv").write_text("Domain\nexample.com\n")
    (entry / "request.json").write_text(
        json.dumps({"input_file": "sites.csv", "deep_retry": False})
    )
    asyncio.run(web_service._process_spool_once())
    job_id = json.loads((entry / "result.json").read_text())["job_id"]
    assert manager.jobs[job_id].deep_retry is False


def test_lens_submit_no_deep_flag(tmp_path, monkeypatch):
    entry = tmp_path / "submit-cli"
    entry.mkdir()
    (entry / "sites.csv").write_text("Domain\nexample.com\n")

    def fake_service(_seconds):
        req = json.loads((entry / "request.json").read_text())
        assert req["deep_retry"] is False
        (entry / "result.json").write_text(json.dumps({"job_id": "J", "input_file": "sites.csv"}))

    monkeypatch.setattr(submit_job.time, "sleep", fake_service)
    assert submit_job.main(["--entry", str(entry), "--input", "sites.csv", "--no-deep"]) == 0
