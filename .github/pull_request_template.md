## What

<!-- One or two sentences: what changes and why. -->

## Checklist

- [ ] `ruff format --check . && ruff check .` pass
- [ ] `python -m pytest -q` passes, hermetically (no key, no network)
- [ ] `config.json.example` / `.env.example` / `docs/DEPLOYMENT.md` updated for any new setting
- [ ] Docs updated where behaviour changed
- [ ] New source files carry the SPDX header
- [ ] No secrets, real customer data, or internal hostnames in the diff

## Screenshots

<!-- For UI changes. -->
