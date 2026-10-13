# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""Queue a dashboard job from the command line (`lens submit`).

deploy/lens-cli copies the input (and any categories file) into a fresh
spool entry, then runs this script as the service user. It validates what it
can, writes request.json, and waits for the running service to queue the job,
so the run shows up in the dashboard like an upload.
"""

import argparse
import json
import os
import re
import shutil
import sys
import time
from pathlib import Path

from custom_categories import load_categories

MODEL_RE = re.compile(r"[~A-Za-z0-9/._:-]+")


def _model(value: str) -> str:
    if not MODEL_RE.fullmatch(value):
        raise argparse.ArgumentTypeError(f"invalid model id: {value}")
    return value


def parse_args(argv):
    p = argparse.ArgumentParser(prog="lens submit", description=__doc__.splitlines()[0])
    p.add_argument("--entry", required=True, help=argparse.SUPPRESS)
    p.add_argument("--input", required=True, help=argparse.SUPPRESS)
    p.add_argument("--name", help="run name shown in the dashboard")
    p.add_argument("--categories", help="custom categories JSON file")
    p.add_argument("--decision-model", type=_model, help="decision model for custom categories")
    p.add_argument("--llm-model", type=_model, help="classification model override")
    p.add_argument("--research-model", type=_model, help="research model override")
    p.add_argument("--no-research-fallback", action="store_true", help="turn the research rung off")
    p.add_argument("--ctv", action="store_true", help="force the CTV workflow")
    p.add_argument("--timeout", type=float, default=30, help="seconds to wait for the service")
    return p.parse_args(argv)


def main(argv=None) -> int:
    args = parse_args(argv)
    entry = Path(args.entry)
    original = entry / args.input
    # Same character allowlist the dashboard applies to uploads.
    clean = re.sub(r"[^A-Za-z0-9._ ()-]", "_", Path(args.input).name).strip()
    if clean != args.input:
        original.rename(entry / clean)

    request = {
        "input_file": clean,
        "mode": "ctv" if args.ctv else "auto",
        "name": args.name,
        "llm_model": args.llm_model,
        "research_model": args.research_model,
        "decision_model": args.decision_model,
        "research_fallback": not args.no_research_fallback,
    }
    if args.categories:
        try:
            request["custom_categories"] = load_categories(args.categories)
        except ValueError as exc:
            print(f"lens submit: {args.categories}: {exc}", file=sys.stderr)
            shutil.rmtree(entry, ignore_errors=True)
            return 1
        Path(args.categories).unlink(missing_ok=True)

    tmp = entry / "request.json.tmp"
    tmp.write_text(json.dumps(request), encoding="utf-8")
    tmp.replace(entry / "request.json")

    result_path = entry / "result.json"
    deadline = time.monotonic() + args.timeout
    while not result_path.exists():
        if time.monotonic() > deadline:
            print(
                f"lens submit: request is waiting in {entry}; is lens.service running? "
                "It will be queued when the service picks it up.",
                file=sys.stderr,
            )
            return 2
        time.sleep(0.5)

    result = json.loads(result_path.read_text(encoding="utf-8"))
    shutil.rmtree(entry, ignore_errors=True)
    if "error" in result:
        print(f"lens submit: rejected: {result['error']}", file=sys.stderr)
        return 1
    where = os.environ.get("LENS_PUBLIC_URL", "").rstrip("/")
    print(
        f"Queued {result['input_file']} as job {result['job_id']}"
        + (f" — {where}/" if where else "")
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
