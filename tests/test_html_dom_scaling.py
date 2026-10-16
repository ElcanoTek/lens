# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Every html_dom operation the scrapers use must stay linear.

Three quadratic paths reached review one after another (closed-void
bookkeeping, list.remove() in decompose, slicing in find_next_sibling), each
fast on ordinary pages and seconds-long on a hostile one. A response body is
up to 5 MiB, so instead of a test per bug this harness times each operation
at N and 2N on flat (many siblings) and deep (many nested levels) markup and
fails when doubling the input costs much more than double the time.

Timings take the best of several runs, and a size pair is re-measured before
failing, so a busy runner does not flake: linear work doubles (ratio ~2),
quadratic work quadruples (ratio ~4), and the bound sits between them.
"""

import os
import re
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

import android_scraper
import scraper_client
from html_dom import Element, parse_html

N = 20_000  # nodes (or nesting levels) at the smaller size
MAX_RATIO = 3.0  # 2N vs N; linear is ~2, quadratic ~4
MAX_SECONDS = 10.0  # absolute bound for one operation at 2N
DEV_LINK = '<a href="/store/apps/dev?id=1">Dev</a>'


def flat(n: int) -> str:
    """Many siblings: scripts, styles, plain and developer-like divs, links."""
    unit = (
        '<div class="row">x</div><script>s()</script><style>p{}</style>'
        '<noscript>n</noscript><img src=x></img><a href="/store/apps/details">a</a>'
    )
    return "<body>" + unit * (n // 6) + DEV_LINK + "</body>"


def deep(n: int) -> str:
    """Many nested levels, with the one interesting node at the bottom."""
    return "<div><span>" * (n // 2) + DEV_LINK + "x"


def _decompose_noise(doc: Element) -> None:
    for element in doc(["script", "style", "noscript"]):
        element.decompose()


def _every_next_sibling(doc: Element) -> None:
    for element in doc.find_all():
        element.find_next_sibling()


OPERATIONS = {
    "parse": None,  # timed on the markup itself
    "find_regex_last": lambda d: d.find("a", href=re.compile(r"/store/apps/dev")),
    "find_attr_missing": lambda d: d.find(attrs={"data-g-id": "description"}),
    "find_all": lambda d: d.find_all("div"),
    "find_all_attr_present": lambda d: d.find_all(attrs={"href": True}),
    "get_text": lambda d: d.get_text(),
    "get_text_strip": lambda d: d.get_text(" ", strip=True),
    "decompose_noise": _decompose_noise,
    "every_next_sibling": _every_next_sibling,
    "title_string": lambda d: d.title and d.title.string,
    "serialise": str,
    "descendants": lambda d: sum(1 for _ in d.descendants()),
}

MUTATING = {"decompose_noise"}

SITE = scraper_client.ScraperClient(mode="direct")
PLAY = android_scraper.AndroidScraper.__new__(android_scraper.AndroidScraper)
EXTRACTORS = {
    "site_parse_html": lambda html: SITE._parse_html("https://example.test/", html),
    "play_parse_html": lambda html: PLAY._parse_html("com.example", html, "t"),
}


def _seconds(run, prepare, repeats: int = 3) -> float:
    best = float("inf")
    for _ in range(repeats):
        subject = prepare()
        start = time.perf_counter()
        run(subject)
        best = min(best, time.perf_counter() - start)
    return best


def _assert_linear(run, prepare_at) -> None:
    for _attempt in range(3):
        small = _seconds(run, lambda: prepare_at(N))
        large = _seconds(run, lambda: prepare_at(2 * N))
        assert large < MAX_SECONDS, f"{large:.2f}s at 2N"
        # Below ~5 ms the clock is noise; only a slow operation can be quadratic.
        if large < 0.005 or large < MAX_RATIO * small:
            return
    pytest.fail(
        f"doubling the input took {large / small:.1f}x as long ({small:.3f}s -> {large:.3f}s)"
    )


@pytest.mark.parametrize("shape", [flat, deep], ids=["flat", "deep"])
@pytest.mark.parametrize("operation", sorted(OPERATIONS))
def test_operation_scales_linearly(operation, shape):
    if operation == "parse":
        _assert_linear(parse_html, shape)
        return
    markup = {N: shape(N), 2 * N: shape(2 * N)}
    if operation in MUTATING:
        _assert_linear(OPERATIONS[operation], lambda n: parse_html(markup[n]))
        return
    parsed = {n: parse_html(m) for n, m in markup.items()}  # read-only: parse once
    _assert_linear(OPERATIONS[operation], parsed.__getitem__)


@pytest.mark.parametrize("extractor", sorted(EXTRACTORS))
def test_extractors_scale_linearly_on_flat_pages(extractor):
    # Deep markup is left out on purpose: the Play Store developer fallback
    # takes get_text() of every <div>, and on nested divs that output itself
    # grows quadratically (bs4 behaved the same). Flat pages are the shape a
    # real store or news page has, and they must stay linear.
    # An app name so the Play Store parse runs every extractor, and no
    # developer link, so its fallback (a find_next_sibling() per <div>) does.
    def page(n: int) -> str:
        return '<h1 itemprop="name"><span>App</span></h1>' + flat(n).replace(DEV_LINK, "")

    _assert_linear(EXTRACTORS[extractor], page)
