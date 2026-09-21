# Custom categories with TypeSafe

Lens can add your own labels alongside its standard quality and IAB analysis.
Enable **Custom categories → Add your own categories** in the dashboard's
**Ready to analyze** panel, then add up to 12 questions:

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

Set `TYPESAFE_API_KEY` in the server's `.env` (or environment) and restart Lens.

For website research fallback and CTV research, Lens also sends the custom
questions (including multiple-choice options) to the research model so its
summary can include relevant evidence. TypeSafe still makes the custom judgment
in a separate call. A research summary is indirect evidence: missing details are
not proof of absence, and these labels do not inspect images or video.

Fresh installs offer an optional, hidden-input TypeSafe key prompt during
bootstrap—press Enter to skip. Unattended bootstrap accepts `TYPESAFE_API_KEY`
from its environment and skips the feature when it is absent. Re-running
bootstrap preserves a key already in `.env`.

For an existing installation:

```bash
sudo lens env edit   # add TYPESAFE_API_KEY=your-key
sudo lens restart
```

The dashboard hides the custom-category options until the key is configured. The key
stays on the server and is never stored with jobs, in browser storage, or in CSVs.
The existing `OPENROUTER_API_KEY` is still required.

Lens calls `https://api.typesafe.ai/v1/systemone` with `jev-latest`, using the
existing async HTTP dependency. No additional package installation is needed.
The resolved model returned by TypeSafe is saved with each successful result.

## CLI

```bash
python main.py --input-csv inventory.csv \
  --output-csv custom-output.csv --progress-file custom-progress.json \
  --custom-categories examples/custom-categories.json
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
There is no screenshot or image analysis. TypeSafe receives the identifier,
title, source type, and up to 24,000 characters of evidence. All questions for
one item are batched into one request, with at most four requests in flight.

When enabled, CSVs gain:

| Column | Meaning |
|---|---|
| `Custom: <name>` | Yes/No or the selected option |
| `P(yes/choice): <name>` | Probability of **yes**, even for a No label; for choices, probability of the selected option |
| `TypeSafe_Status` | `success`, `error`, or `skipped` |
| `TypeSafe_Error` | Sanitized failure or skip reason |
| `TypeSafe_Model` | Actual model used |
| `TypeSafe_Answers` | JSON with answers, raw probabilities, full choice distributions and choice confidence |

Yes/no labels use a 0.5 cutoff. A value near 0.5 means uncertainty between yes
and no, not medium intensity. It has no separate confidence score. Validate your
questions and thresholds on examples representative of your inventory.

A TypeSafe failure leaves the successful quality/IAB result intact and the
custom labels **blank**, with `TypeSafe_Status=error`. It is never converted to
No or Unknown. Transient HTTP/connection errors retry at most twice with backoff;
authentication and validation errors do not retry. Primary-analysis failures
skip custom categorization. Standard progress counts still describe standard
analysis; inspect `TypeSafe_Status` for custom-category coverage.

The optional feature makes no TypeSafe calls and adds no columns when off.

## Resuming and rerunning

Lens saves definitions in the progress file and beside the CSV as
`<output.csv>.categories.json`. Resume using the same definitions. Changing,
enabling, or disabling categories on existing results is rejected rather than
mixing different meanings under the same columns. Use **new output and progress
paths** (or a new dashboard run) to change definitions or retry custom errors.
Standard successes remain processed even when their optional TypeSafe request
failed; a resume does not repeat them or charge for their primary analysis again.
