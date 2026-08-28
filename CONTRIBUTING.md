# Contributing to Lens

Thanks for looking. Bug reports, small fixes and well-scoped features are all
welcome.

Before anything else, read [`AGENTS.md`](AGENTS.md). It is the architecture map
— module responsibilities, the auto-mode ladder, and the non-obvious gotchas
that will otherwise cost you an afternoon (the config singleton that crashes on
import without an API key, the CTV news-routing rule, why `read()` has to loop).

## Licensing of contributions

Lens is source-available under the **Business Source License 1.1** with
ElcanoTek, Inc. as Licensor. **Contributions are accepted under those same
terms.** Opening a pull request means you have the right to contribute the code
and you agree to it being released under BSL 1.1, converting to MIT on the
Change Date like the rest of the file. There is no separate CLA to sign.

Every new first-party source file starts with:

```python
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
```

After a shebang and any coding declaration, before the module docstring. See
[`docs/LICENSING.md`](docs/LICENSING.md) for what the licence does and does not
allow — note that it is non-production-only, so please don't open a PR that
assumes otherwise.

## Setup

Python 3.11 or 3.12. CI runs both (3.12 for the unit suite, 3.11 for the
classification smoke test).

With [uv](https://docs.astral.sh/uv/) — what the deploy scripts use:

```bash
git clone https://github.com/ElcanoTek/lens.git
cd lens
uv venv
uv pip install -r requirements.txt
uv pip install pytest httpx        # httpx backs FastAPI's TestClient
source .venv/bin/activate
```

Or plain venv:

```bash
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt pytest httpx
```

You do **not** need an OpenRouter key to run the tests — the suite stubs it.
You do need one to run the pipeline for real:

```bash
cp .env.example .env      # then fill in OPENROUTER_API_KEY
```

Remember that real runs cost money. `test-input/` has small synthetic fixtures
for cheap smoke tests.

## Running the tests

```bash
python -m pytest -q                        # the whole suite, ~5s
python -m pytest tests/test_input_detector.py -q
python -m pytest tests/test_ctv_functionality.py -k news_routing -v
```

`pytest.ini` sets `asyncio_mode = auto`, so async tests need no
`@pytest.mark.asyncio` decorator.

Two things to know about the suite:

- It is **hermetic**. `tests/conftest.py` (and each test module) calls
  `os.environ.setdefault("OPENROUTER_API_KEY", ...)` *before* importing project
  modules, because `config.py` builds its singleton at import and raises
  without the key. Tests that touch auth mint their own throwaway Ed25519
  keypair. No network, no secrets.
- Because of that ordering requirement, test modules legitimately have imports
  below the top of the file. Don't "fix" them.

`tests/test_integrations.py` contains one test that talks to OpenRouter for
real; it skips itself when no usable key is present.

## Linting

`black` and `flake8` are in `requirements.txt` and are useful locally, but they
are **advisory** — neither is configured or enforced in CI, and the existing
tree is not clean under either default profile.

```bash
python -m black --diff .          # see what it would change
python -m flake8 <files-you-touched>
```

Please keep formatting changes out of functional PRs: a `black` run across a
module you touched turns a five-line fix into a thousand-line diff nobody can
review. Match the surrounding style instead. **The test suite is the gate.**

## Branches and pull requests

Work on a branch off `main` and open a PR — `main` is protected and CI runs on
pull requests. The convention in this repo's history is a
`<type>/<short-description>` slug:

| Prefix | For |
|---|---|
| `feat/` | New capability |
| `fix/` | Bug fix |
| `ci/` | Workflow and pipeline changes |
| `chore/` | Dependencies, housekeeping, model-pin bumps |
| `docs/` | Documentation only |

Commit subjects are imperative and describe the *effect*, not the mechanism —
"Make Firecrawl fail fast on anti-bot and stop re-hammering blocked sites"
rather than "update scraper_client.py". Conventional-commit prefixes
(`chore(ci):`, `fix:`) appear in the history too and are fine.

A good PR:

- Does one thing.
- Adds or updates tests for behaviour it changes. This codebase is mocked
  heavily precisely so that scraper and LLM behaviour is testable offline.
- Updates the docs it invalidates — `AGENTS.md` for architecture and gotchas,
  `docs/DEPLOYMENT.md` for anything operational, `config.json.example` and
  `.env.example` for any new setting (the examples are meant to document
  *every* key the code reads).
- Leaves `python -m pytest -q` green.

### Things that need extra care

- **`config.py` defaults** — a new key belongs in `config.json.example` too,
  with a comment.
- **Scrape-ladder behaviour** — read the anti-bot section of `AGENTS.md`
  first. "Try harder" changes (more retries, stealth escalation) increase ban
  risk without helping, and have been deliberately removed before.
- **`deploy/` and `scripts/`** — these run as root on real hosts. Keep them
  re-runnable, and update `docs/DEPLOYMENT.md` in the same PR.
- **`content_taxonomy.tsv`** — third-party reference data (IAB Tech Lab).
  Don't hand-edit rows; replace the file from upstream.
- **Never commit** `config.json`, `output.csv`, `progress.json`, `.env`,
  `managed-files/`, or any real inventory list. All are git-ignored; keep it
  that way. Fixtures in `test-input/` must stay synthetic — see
  [`test-input/README.md`](test-input/README.md).

## Reporting bugs

Open an issue with the failing input shape (a couple of synthetic rows, not
your real list), the command or dashboard action, and the relevant log lines.
Scrub API keys and anything commercially sensitive first.

For anything security-related, do **not** open an issue — see
[`SECURITY.md`](SECURITY.md).

## Code of conduct

Participation is covered by [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).
