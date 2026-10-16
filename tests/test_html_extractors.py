# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Golden tests for every HTML extractor the scrapers run.

``tests/fixtures/html/golden.json`` was recorded from these fixtures while the
extractors still parsed with BeautifulSoup 4.15.0 (``html.parser`` backend).
The extractors now run on the stdlib-based ``html_dom`` module, and this test
holds them to byte-identical output: the website scraper's title / meta
description / text, the Play Store parse result, and each ``_extract_*``
helper on its own (so a fallback branch that the full parse never reaches
because an earlier extractor already answered is still pinned).

To re-record after an *intended* change to extractor behaviour:

    python tests/test_html_extractors.py --regen
"""

import json
import os
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

import android_scraper
import html_dom
import scraper_client

FIXTURES = Path(__file__).resolve().parent / "fixtures" / "html"
GOLDEN = FIXTURES / "golden.json"

ANDROID_EXTRACTORS = (
    "_extract_app_name",
    "_extract_developer",
    "_extract_category",
    "_extract_rating",
    "_extract_downloads",
    "_extract_description",
    "_extract_last_updated",
    "_extract_content_rating",
)


def extract_everything(html: str) -> dict:
    android = android_scraper.AndroidScraper.__new__(android_scraper.AndroidScraper)
    site = scraper_client.ScraperClient(mode="direct")
    result = {
        "site": site._parse_html("https://example.test/", html),
        "android": android._parse_html("com.example.app", html, "2026-01-01T00:00:00"),
    }
    for name in ANDROID_EXTRACTORS:
        value = getattr(android, name)(html_dom.parse_html(html))
        result[name] = list(value) if isinstance(value, tuple) else value
    return result


def _fixture_names():
    return sorted(path.name for path in FIXTURES.glob("*.html"))


@pytest.fixture(scope="module")
def golden():
    return json.loads(GOLDEN.read_text(encoding="utf-8"))


def test_every_fixture_has_a_golden_record(golden):
    assert sorted(golden) == _fixture_names()


@pytest.mark.parametrize("name", _fixture_names())
def test_extractors_match_the_recorded_output(golden, name):
    html = (FIXTURES / name).read_text(encoding="utf-8")
    assert extract_everything(html) == golden[name]


if __name__ == "__main__" and "--regen" in sys.argv:
    records = {
        name: extract_everything((FIXTURES / name).read_text(encoding="utf-8"))
        for name in _fixture_names()
    }
    GOLDEN.write_text(
        json.dumps(records, indent=1, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
