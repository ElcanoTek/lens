# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Unit tests for html_dom, the stdlib tree that replaced BeautifulSoup.

Expected values were taken from BeautifulSoup 4.15.0 (``html.parser``) on the
same markup before the dependency was removed; the extractor-level goldens are
in test_html_extractors.py.
"""

import re
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from html_dom import Element, parse_html


@pytest.mark.parametrize(
    "markup,expected",
    [
        ("a &amp; b", "a & b"),
        ("&copy 2026", "© 2026"),  # legacy entity without a semicolon
        ("&foo; &notit; x", "&foo &notit x"),  # unknown: kept, semicolon dropped
        ("&#150;&#x41;&#65;", "–AA"),  # 0x80-0x9F go through Windows-1252
        ("&#0;", "�"),
        ("&#xD800;", "�"),
        ("&#x110000;", "�"),
        ("&#x81;", "\x81"),  # undefined in Windows-1252: kept as-is
        ("&#12a;", "&#12a;"),
    ],
)
def test_character_references_resolve_like_bs4(markup, expected):
    assert parse_html(f"<p>{markup}</p>").get_text() == expected


def test_whitespace_only_strings_collapse_outside_pre_and_textarea():
    doc = parse_html("<div>a</div> \t <div>b</div>\n \n<pre>  \n </pre><textarea>   </textarea>")
    assert doc.get_text() == "a b\n  \n    "


def test_get_text_skips_script_style_template_ruby_and_comments():
    doc = parse_html(
        "<title>T</title><style>s{}</style><script>var a</script><template>tp</template>"
        "<!--c--><![CDATA[cd]]><?pi?><!DOCTYPE html><rt>r</rt><rp>(</rp><p>body</p>"
    )
    assert doc.get_text() == "Tcdbody"
    assert doc.find("script").get_text() == "var a"
    assert doc.find("rt").get_text() == "r"


def test_get_text_strip_and_separator():
    doc = parse_html("<div>  one <b> two </b>\n<i></i> three </div>")
    assert doc.find("div").get_text(" ", strip=True) == "one two three"
    assert doc.find("div").get_text(strip=True) == "onetwothree"


def test_end_tags_close_the_most_recent_match_and_strays_are_ignored():
    doc = parse_html("</div><p>one<p>two</span><b>bold <i>both</b> after</i>")
    first = doc.find("p")
    assert [child.name for child in first.children if isinstance(child, Element)] == ["p"]
    bold = doc.find("b")
    assert bold.get_text() == "bold both"
    assert doc.find("i").get_text() == "both"
    assert doc.get_text() == "onetwobold both after"


def test_void_elements_close_immediately_and_swallow_one_stray_end_tag():
    doc = parse_html("<div><br>text</br></br><img src=x>more</div>")
    div = doc.find("div")
    assert [c.name for c in div.children if isinstance(c, Element)] == ["br", "img"]
    assert div.get_text() == "textmore"
    # A non-void self-closing tag is an empty element, not an open one.
    assert parse_html("<p/>after").find("p").get_text() == ""


def test_find_and_find_all_filters():
    doc = parse_html(
        '<a href="/store/apps/dev?id=1"><span>Dev</span></a>'
        '<a href="/store/apps/category/GAME">Game</a>'
        '<div aria-label="">empty</div><div aria-label="x">x</div>'
        '<meta property="og:title" content="OG"><span itemprop="genre">G</span>'
    )
    assert doc.find("a", href=re.compile(r"/store/apps/category")).get_text() == "Game"
    assert doc.find("a", href=re.compile(r"/store/apps/dev")).find("span").get_text() == "Dev"
    assert [d.get_text() for d in doc.find_all(attrs={"aria-label": True})] == ["empty", "x"]
    assert doc.find("meta", property="og:title")["content"] == "OG"
    assert doc.find(itemprop="genre").get_text() == "G"
    assert doc.find("table") is None
    assert [e.name for e in doc(["span", "meta"])] == ["span", "meta", "span"]


def test_attributes_valueless_and_repeated():
    tag = parse_html('<div data-g-id="a" data-g-id="b" hidden></div>').find("div")
    assert tag.attrs == {"data-g-id": "b", "hidden": ""}
    assert tag.get("missing", "") == ""


def test_title_string_follows_single_children():
    assert parse_html("<title> Hi </title>").title.string == " Hi "
    assert parse_html("<title></title>").title.string is None
    assert parse_html("<div><b>x</b></div>").find("div").string == "x"
    assert parse_html("<div><b>x</b>y</div>").find("div").string is None
    assert parse_html("<p>x</p>").title is None


def test_decompose_removes_the_subtree():
    doc = parse_html("<p>keep</p><script>drop()</script><noscript>also <b>drop</b></noscript>")
    for element in doc(["script", "style", "noscript"]):
        element.decompose()
    assert doc.get_text(" ", strip=True) == "keep"
    assert doc.find("b") is None


def test_next_sibling_skips_strings_and_markup_includes_attributes():
    doc = parse_html('<div id="a">A</div> text <div class="Category-row">B</div>')
    first = doc.find("div")
    sibling = first.find_next_sibling()
    assert sibling.get_text() == "B"
    assert "category" in str(sibling).lower()
    assert sibling.find_next_sibling() is None


def test_deeply_nested_unclosed_markup_does_not_recurse():
    depth = 20_000
    doc = parse_html("<div>" * depth + "deep")
    assert doc.get_text() == "deep"
    assert len(doc.find_all("div")) == depth
    assert str(doc).startswith("<div><div>")


def test_stray_void_end_tag_keeps_the_string_open_like_bs4():
    # bs4 4.15.0 swallows the redundant </br> without ending the current
    # string, so "b" and "c" are one string: get_text gives "a bc", not
    # "a b c". Parity with the recorded extractor output wins here.
    doc = parse_html("<p>a<br>b</br>c</p>")
    assert doc.get_text(" ", strip=True) == "a bc"
    assert parse_html("<p>a<img>b</img>c</p>").get_text(" ", strip=True) == "a bc"
    # Each void element swallows at most one later end tag of its name.
    assert parse_html("<p>a<br>b</br>c</br>d</p>").get_text(" ", strip=True) == "a bc d"


def test_next_sibling_after_decompose():
    doc = parse_html("<div>A</div><script>x</script><div>B</div><div>C</div>")
    doc.find("script").decompose()
    first, second, third = doc.find_all("div")
    assert first.find_next_sibling() is second
    assert second.find_next_sibling() is third


@pytest.mark.parametrize("unit", ["<div><img></div>", "<br></br>x", "<div>"])
def test_hostile_repeats_parse_in_linear_time(unit):
    # A response body is capped at 5 MiB and is parsed in full. Each closed
    # void element used to be appended to a list that every end tag scanned,
    # so ~320 KB of "<div><img></div>" took seconds and a MiB took ~40 s
    # (bs4 has the same quadratic shape). Linear, a MiB takes about a second;
    # the bound sits far from both so a slow runner does not flake.
    markup = unit * (1024 * 1024 // len(unit))
    start = time.perf_counter()
    parse_html(markup)
    assert time.perf_counter() - start < 10
