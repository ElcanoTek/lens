"""LENS_MAX_PARALLEL_JOBS: the dashboard queue can run several jobs at once."""

import asyncio

import pytest

import web_service


@pytest.mark.parametrize(
    "value, expected",
    [(None, 1), ("1", 1), ("3", 3), ("0", 1), ("-2", 1), ("50", 8), ("lots", 1)],
)
def test_max_parallel_jobs_is_clamped(monkeypatch, value, expected):
    if value is None:
        monkeypatch.delenv("LENS_MAX_PARALLEL_JOBS", raising=False)
    else:
        monkeypatch.setenv("LENS_MAX_PARALLEL_JOBS", value)
    assert web_service._max_parallel_jobs() == expected


async def _drive(monkeypatch, slots, n_jobs):
    """Queue n_jobs whose runs block until released; return the manager,
    the release events and the worker task."""
    monkeypatch.setenv("LENS_MAX_PARALLEL_JOBS", str(slots))
    manager = web_service.JobManager()
    gates: dict = {}
    started: list = []

    async def fake_run(job_id):
        started.append(job_id)
        gates[job_id] = asyncio.Event()
        await gates[job_id].wait()

    monkeypatch.setattr(manager, "_run_job", fake_run)
    jobs = [await manager.create_job(input_file=f"f{i}.csv", mode="auto") for i in range(n_jobs)]
    worker = asyncio.create_task(manager.worker_loop())
    await asyncio.sleep(0.05)
    return manager, jobs, gates, started, worker


async def _stop(manager, gates, worker):
    for gate in gates.values():
        gate.set()
    await asyncio.sleep(0.05)
    worker.cancel()
    await asyncio.gather(worker, return_exceptions=True)
    await manager.shutdown()


def test_default_runs_one_job_at_a_time(monkeypatch):
    async def scenario():
        manager, jobs, gates, started, worker = await _drive(monkeypatch, 1, 3)
        assert started == [jobs[0].id] and manager.running == {jobs[0].id}
        assert manager.current_job_id == jobs[0].id
        await _stop(manager, gates, worker)

    asyncio.run(scenario())


def test_slots_fill_and_a_freed_slot_starts_the_next_job(monkeypatch):
    async def scenario():
        manager, jobs, gates, started, worker = await _drive(monkeypatch, 2, 3)
        assert started == [jobs[0].id, jobs[1].id]
        assert list(manager.queue) == [jobs[2].id]

        gates[jobs[0].id].set()
        await asyncio.sleep(0.05)
        assert started == [jobs[0].id, jobs[1].id, jobs[2].id]
        assert manager.running == {jobs[1].id, jobs[2].id}
        assert not manager.queue
        await _stop(manager, gates, worker)
        assert manager.running == set() and manager.current_job_id is None

    asyncio.run(scenario())


def test_jobs_queued_later_start_when_a_slot_is_free(monkeypatch):
    async def scenario():
        manager, jobs, gates, started, worker = await _drive(monkeypatch, 2, 1)
        assert started == [jobs[0].id]
        late = await manager.create_job(input_file="late.csv", mode="auto")
        await asyncio.sleep(0.05)
        assert started == [jobs[0].id, late.id]
        await _stop(manager, gates, worker)

    asyncio.run(scenario())
