# Custom categories

Lens can add your own labels alongside its standard quality and IAB analysis.
A *decision model* answers them: a model that returns typed answers with
probabilities instead of prose. Lens reaches every decision model through
OpenRouter, so no extra key is needed. TypeSafe's Jev is the default.

Enable **Custom categories → Add your own categories** in the dashboard's
**Ready to analyze** panel, pick a **Decision model**, then add up to 12
questions:

- **Yes / no:** an independent label, e.g. “Would a mainstream brand be
  comfortable appearing beside this content?” Several labels can apply at once.
- **Choose one category:** your question plus 2–12 options, one per line. For
  example, choose a primary purpose from News, Entertainment, Shopping,
  Education, Other and Unknown.

Define what the question means, including important exclusions. A **Brand
safe** label is more useful when it measures trust rather than topic: say that
honest journalism, health information, and edgy comedy can qualify, while
deception, harassment, hate, and content that exists mainly to shock do not.
Include an Other or Unknown option when a choice may not fit.

Definitions stay in this browser for reuse. The feature starts **off** when the
page reloads; turn it on for each run that needs it. Each queued run snapshots
its definitions, so later edits do not change a queued or completed run. The
Runs table lists its custom category names; **Categories** downloads the exact
JSON definitions for reuse in the CLI.

## Server setup

None beyond the `OPENROUTER_API_KEY` Lens already requires. Lens calls
OpenRouter's Decisions API (`POST https://openrouter.ai/api/alpha/decisions`)
with the existing async HTTP dependency.

The dashboard's **Decision model** dropdown lists OpenRouter's paid decision
models, refreshed hourly; free tiers are left out because their rate limits
stall batch runs. `~typesafe/jev-latest` is recommended and tracks Jev's newest
release. The CLI and `config.json` use `decision_model`, and `--decision-model`
overrides it for one run. Each dashboard run stores the model it was queued
with, and the model OpenRouter actually used is saved with every result.

Jev always returns a probability for every choice option and a confidence.
OpenRouter's schema makes both optional, so some other decision models return
only the selected option. Lens then leaves those columns blank rather than
inventing values.

### Optional TypeSafe fallback

The Decisions API is still alpha. For resilience, set `TYPESAFE_API_KEY` in the
server's `.env` and restart Lens. When the selected model is Jev
(`~typesafe/jev-latest` or a pinned `typesafe/jev-<version>`) and the OpenRouter
request fails after its retries, Lens sends the same question once to TypeSafe's
own endpoint (`https://api.typesafe.ai/v1/systemone`). Those rows show
`Decision_Provider=TypeSafe (direct)`. Other models never use the fallback.

```bash
sudo lens env edit   # add TYPESAFE_API_KEY=your-key
sudo lens restart
```

Bootstrap offers an optional, hidden-input prompt for this key; press Enter to
skip. Unattended bootstrap accepts `TYPESAFE_API_KEY` from its environment and
preserves a key already in `.env`. Keys stay on the server and are never stored
with jobs, in browser storage, or in CSVs.

For website research fallback and CTV research, Lens also sends the custom
questions (including multiple-choice options) to the research model so its
summary can include relevant evidence. The decision model still makes the
custom judgment in a separate call. A research summary is indirect evidence:
missing details are not proof of absence, and these labels do not inspect
images or video.

## CLI

```bash
python main.py --input-csv inventory.csv \
  --output-csv custom-output.csv --progress-file custom-progress.json \
  --custom-categories examples/custom-categories.json \
  --decision-model '~typesafe/jev-latest'   # optional; this is the default
```

The optional `--custom-categories JSON_FILE` flag accepts this format:

```json
{
  "categories": [
    {"name": "Educational", "type": "boolean", "question": "Does this teach useful skills?"},
    {"name": "Audience", "type": "choice", "question": "Who is the primary audience? Select Unknown if unclear.",
     "options": ["Children", "Adults", "All ages", "Unknown"]}
  ]
}
```

Names must be unique, start with a letter, and contain at most 48 ASCII letters,
digits, spaces, underscores or hyphens. Questions can be up to 1,000 characters;
choice labels up to 80. Definitions are limited to 32 KiB. Choice labels cannot
start with spreadsheet formula characters (`=`, `+`, `-`, `@`). Validation is the
same in the dashboard and CLI.

## Evidence and output

Custom categories run after a successful standard analysis, using the original
scraped text, store metadata, or research summary—not the standard classifier's
short description. Research-based answers are judgments about that summary.
There is no screenshot or image analysis. The decision model receives the identifier,
title, source type, and up to 24,000 characters of evidence. All questions for
one item are batched into one request, with at most four requests in flight.

When enabled, CSVs gain:

| Column | Meaning |
|---|---|
| `Custom: <name>` | Yes/No or the selected option. Filter this to keep one answer. |
| `P(yes/choice): <name>` | Probability of **yes**, even for a No label; for choices, probability of the selected option |
| `Confidence: <name>` | Choice only. How sure the selection is, separate from the option's probability |
| `P: <name> / <option>` | Choice only. That option's probability, one column per option, in definition order. Sort or filter these without opening the JSON |
| `Decision_Status` | `success`, `error`, or `skipped`. After the answer columns |
| `Decision_Error` | Sanitized failure or skip reason |
| `Decision_Model` | Actual model used, e.g. `typesafe/jev-1.13-20260917` |
| `Decision_Provider` | Who served it, as OpenRouter reports it, or `TypeSafe (direct)` for the fallback |
| `Decision_Answers` | JSON with the same answers, for anything a column does not already show |

Earlier releases named these columns `TypeSafe_*`. Resuming such a run renames
them in place and keeps their values.

Yes/no labels use a 0.5 cutoff. A value near 0.5 means uncertainty between yes
and no, not medium intensity. It has no separate confidence score. Validate your
questions and thresholds on examples representative of your inventory.

A decision-model failure leaves the successful quality/IAB result intact and
the custom labels **blank**, with `Decision_Status=error`. It is never converted to
No or Unknown. Transient HTTP/connection errors retry at most twice with backoff;
authentication and validation errors do not retry. Primary-analysis failures
skip custom categorization. Standard progress counts still describe standard
analysis; inspect `Decision_Status` for custom-category coverage.

The optional feature makes no decision-model calls and adds no columns when off.

## Resuming and rerunning

Lens saves definitions in the progress file and beside the CSV as
`<output.csv>.categories.json`. Resume using the same definitions. Changing,
enabling, or disabling categories on existing results is rejected rather than
mixing different meanings under the same columns. Use **new output and progress
paths** (or a new dashboard run) to change definitions or retry custom errors.
Standard successes remain processed even when their optional decision request
failed; a resume does not repeat them or charge for their primary analysis again.
