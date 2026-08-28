# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Tests for input-format hardening and the file-management endpoints.

Shapes mirror real customer uploads: BOM+CRLF CSVs, headerless xlsx domain
dumps, semicolon-delimited European exports, trailing empty Excel columns,
and lists whose headers say nothing useful about their contents.
"""

import base64
import json
import os
import time

import pandas as pd

os.environ.setdefault("OPENROUTER_API_KEY", "test-key")

from cryptography.hazmat.primitives import serialization as _ser
from cryptography.hazmat.primitives.asymmetric.ed25519 import (
    Ed25519PrivateKey as _Ed,
)

_AUTH_PRIV = _Ed.generate()
_AUTH_PUB_B64 = base64.b64encode(
    _AUTH_PRIV.public_key().public_bytes(_ser.Encoding.Raw, _ser.PublicFormat.Raw)
).decode()


def _auth_cookie(email: str = "tester@elcanotek.com") -> str:
    payload = {
        "email": email,
        "tenant": "elcanotek.com",
        "iat": int(time.time()),
        "exp": int(time.time()) + 3600,
    }
    body = base64.urlsafe_b64encode(json.dumps(payload).encode()).decode().rstrip("=")
    sig = _AUTH_PRIV.sign(body.encode())
    return f"{body}.{base64.urlsafe_b64encode(sig).decode().rstrip('=')}"


os.environ["AUTH_SIGNING_PUBKEY"] = _AUTH_PUB_B64

from fastapi.testclient import TestClient  # noqa: E402

import web_service  # noqa: E402
from input_detector import InputDetector, detect_input_column_by_content  # noqa: E402
from orchestration import (  # noqa: E402
    _load_input_dataframe,
    _normalize_headerless_input_dataframe,
)


def _analyze(path):
    df = _normalize_headerless_input_dataframe(_load_input_dataframe(str(path)))
    detector = InputDetector(df)
    assert detector.analyze() is True
    return detector


# ── File-shape detection ─────────────────────────────────────────────────


def test_bom_crlf_csv_detects_domain_column(tmp_path):
    path = tmp_path / "rmi.csv"
    path.write_bytes(b"\xef\xbb\xbfDomain\r\nfoxnews.com\r\nfoxbusiness.com\r\n")
    detector = _analyze(path)
    assert detector.input_column == "Domain"
    assert len(detector.item_types) == 2


def test_headerless_xlsx_recovers_header_domain(tmp_path):
    path = tmp_path / "allowlist.xlsx"
    pd.DataFrame({"harborlight-news.com": ["cedarpost.com", "01net-example.com"]}).to_excel(
        path, index=False
    )
    detector = _analyze(path)
    assert detector.input_column == "Domain"
    values = [v for v, _ in detector.item_types]
    assert "harborlight-news.com" in values
    assert len(values) == 3


def test_semicolon_delimited_csv_is_sniffed(tmp_path):
    path = tmp_path / "euro.csv"
    path.write_text(
        "Domain;Tier;Notes\nlemonde.fr;Premium;bon\nspiegel.de;Standard;gut\n",
        encoding="utf-8",
    )
    detector = _analyze(path)
    assert detector.input_column == "Domain"
    assert [v for v, _ in detector.item_types] == ["lemonde.fr", "spiegel.de"]


def test_trailing_empty_columns_dropped(tmp_path):
    path = tmp_path / "trailing.csv"
    path.write_text("Domain,,,\nnytimes.com,,,\nwsj.com,,,\n", encoding="utf-8")
    detector = _analyze(path)
    assert detector.input_column == "Domain"
    assert len(detector.item_types) == 2


def test_content_fallback_finds_domain_column(tmp_path):
    path = tmp_path / "ranked.csv"
    path.write_text("Position,Address\n1,nytimes.com\n2,wsj.com\n3,cnn.com\n", encoding="utf-8")
    detector = _analyze(path)
    assert detector.input_column == "Address"
    assert len(detector.item_types) == 3


def test_content_fallback_ignores_rank_and_notes_columns():
    df = pd.DataFrame(
        {
            "Position": ["1", "2", "3"],
            "Comments": ["great site", "blocked last week", "n/a"],
            "Address": ["nytimes.com", "wsj.com", "cnn.com"],
        }
    )
    assert detect_input_column_by_content(df) == "Address"


def test_content_fallback_rejects_unclassifiable_files():
    df = pd.DataFrame({"a": ["hello", "world"], "b": ["1", "2"]})
    assert detect_input_column_by_content(df) is None


# ── File management endpoints ────────────────────────────────────────────


def _client_dirs(monkeypatch, tmp_path):
    monkeypatch.setenv("AUTH_SIGNING_PUBKEY", _AUTH_PUB_B64)
    input_dir = tmp_path / "inputs"
    output_dir = tmp_path / "outputs"
    input_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)
    monkeypatch.setattr(web_service, "INPUT_DIR", input_dir)
    monkeypatch.setattr(web_service, "OUTPUT_DIR", output_dir)
    return input_dir, output_dir


def test_rename_input_file(monkeypatch, tmp_path):
    input_dir, _ = _client_dirs(monkeypatch, tmp_path)
    (input_dir / "old name.csv").write_text("Domain\nfoo.com\n", encoding="utf-8")

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())
        response = client.post(
            "/files/rename",
            data={
                "destination": "inputs",
                "filename": "old name.csv",
                "new_name": "JPMC allowlist.csv",
            },
            follow_redirects=False,
        )

    assert response.status_code == 303
    assert not (input_dir / "old name.csv").exists()
    assert (input_dir / "JPMC allowlist.csv").exists()


def test_rename_preserves_extension_when_missing(monkeypatch, tmp_path):
    input_dir, _ = _client_dirs(monkeypatch, tmp_path)
    (input_dir / "list.xlsx").write_bytes(b"PK\x03\x04fake")

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())
        client.post(
            "/files/rename",
            data={
                "destination": "inputs",
                "filename": "list.xlsx",
                "new_name": "Q1 priorities",
            },
            follow_redirects=False,
        )

    assert (input_dir / "Q1 priorities.xlsx").exists()


def test_rename_rejects_existing_target(monkeypatch, tmp_path):
    input_dir, _ = _client_dirs(monkeypatch, tmp_path)
    (input_dir / "a.csv").write_text("x", encoding="utf-8")
    (input_dir / "b.csv").write_text("y", encoding="utf-8")

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())
        response = client.post(
            "/files/rename",
            data={"destination": "inputs", "filename": "a.csv", "new_name": "b.csv"},
            follow_redirects=False,
        )

    assert "files_error" in response.headers["location"]
    assert (input_dir / "a.csv").exists()
    assert (input_dir / "b.csv").read_text(encoding="utf-8") == "y"


def test_rename_rejects_traversal(monkeypatch, tmp_path):
    input_dir, _ = _client_dirs(monkeypatch, tmp_path)
    (input_dir / "a.csv").write_text("x", encoding="utf-8")

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())
        response = client.post(
            "/files/rename",
            data={
                "destination": "inputs",
                "filename": "a.csv",
                "new_name": "../escape.csv",
            },
            follow_redirects=False,
        )

    assert "files_error" in response.headers["location"]
    assert (input_dir / "a.csv").exists()
    assert not (tmp_path / "escape.csv").exists()


def test_rename_only_inputs(monkeypatch, tmp_path):
    _, output_dir = _client_dirs(monkeypatch, tmp_path)
    (output_dir / "run_output.csv").write_text("x", encoding="utf-8")

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())
        response = client.post(
            "/files/rename",
            data={
                "destination": "outputs",
                "filename": "run_output.csv",
                "new_name": "renamed.csv",
            },
            follow_redirects=False,
        )

    assert "files_error" in response.headers["location"]
    assert (output_dir / "run_output.csv").exists()


def test_static_assets_are_versioned_and_cached(monkeypatch, tmp_path):
    _client_dirs(monkeypatch, tmp_path)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())

        page = client.get("/")
        assert page.status_code == 200
        # Dynamic responses must never be cached.
        assert page.headers["cache-control"] == "no-store"
        # Every static reference carries the content-hash version.
        import re

        refs = re.findall(r'(?:href|src)="(/static/[^"]+)', page.text)
        assert refs, "expected static asset references in the page"
        for ref in refs:
            assert f"?v={web_service.STATIC_VERSION}" in ref, ref

        versioned = client.get(f"/static/main.js?v={web_service.STATIC_VERSION}")
        assert versioned.status_code == 200
        assert "immutable" in versioned.headers["cache-control"]

        unversioned = client.get("/static/main.js")
        assert unversioned.status_code == 200
        assert unversioned.headers["cache-control"] == "no-cache"

        stale = client.get("/static/main.js?v=oldhash123")
        assert stale.headers["cache-control"] == "no-cache"


def test_static_version_is_content_derived():
    version = web_service._static_version()
    assert version == web_service.STATIC_VERSION
    assert len(version) == 12
    int(version, 16)  # hex digest prefix


def test_every_static_reference_resolves(monkeypatch, tmp_path):
    """No dead <link>/<script>/<img> in the rendered page.

    A stylesheet or preload pointing at a deleted asset is invisible in the
    UI and costs a 404 on every page load — exactly the failure mode a font
    or design-system re-vendor introduces. Fetch every reference instead of
    only checking that it is versioned.
    """
    _client_dirs(monkeypatch, tmp_path)

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())
        page = client.get("/")
        assert page.status_code == 200

        import re

        refs = sorted(set(re.findall(r'(?:href|src)="(/static/[^"]+)', page.text)))
        assert refs, "expected static asset references in the page"
        for ref in refs:
            assert client.get(ref).status_code == 200, ref


def test_bundled_fonts_are_the_two_brand_faces():
    """Elcano ships exactly two typefaces, self-hosted, and nothing else.

    Guards three things at once: no third face creeps back into a @font-face
    rule (IBM Plex Sans, Share Tech Mono and VT323 all lived here once), no
    src url() outlives the file it points at, and the OFL / MIT licence text
    still travels with the binaries as both licences require.
    """
    import re

    static_root = web_service.BASE_DIR / "static"
    sheets = sorted(static_root.rglob("*.css"))
    assert sheets, "expected stylesheets under static/"

    families: set[str] = set()
    for sheet in sheets:
        text = sheet.read_text(encoding="utf-8")
        for block in re.findall(r"@font-face\s*\{(.*?)\}", text, re.DOTALL):
            family = re.search(r"font-family:\s*\"([^\"]+)\"", block)
            assert family, f"@font-face without a quoted family in {sheet}"
            families.add(family.group(1))
            for url in re.findall(r'url\(\s*"([^"]+)"', block):
                target = (sheet.parent / url).resolve()
                assert target.is_file(), f"{sheet}: src url() misses {url}"

    assert families == {"Nebula Sans", "Hack"}, families

    fonts_dir = static_root / "design-system" / "fonts"
    assert (fonts_dir / "nebula-sans" / "OFL.txt").is_file()
    assert (fonts_dir / "hack" / "LICENSE.md").is_file()

    # Self-hosted only: no CDN or Google Fonts request anywhere in the UI.
    hosts = ("fonts.googleapis.com", "fonts.gstatic.com", "cdn.jsdelivr.net", "unpkg.com")
    scanned = sheets + sorted((web_service.BASE_DIR / "templates").rglob("*.html"))
    scanned += sorted(static_root.rglob("*.js"))
    for path in scanned:
        text = path.read_text(encoding="utf-8")
        for host in hosts:
            assert host not in text, f"{path} loads from {host}"


def test_input_files_listed_newest_first(monkeypatch, tmp_path):
    input_dir, _ = _client_dirs(monkeypatch, tmp_path)
    old = input_dir / "old.csv"
    new = input_dir / "new.csv"
    old.write_text("x", encoding="utf-8")
    new.write_text("y", encoding="utf-8")
    past = time.time() - 3600
    os.utime(old, (past, past))

    files = web_service._list_input_files()
    assert [f["name"] for f in files] == ["new.csv", "old.csv"]

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())
        response = client.get("/")

    body = response.text
    # Newest file appears first in the library.
    assert body.index('value="new.csv"') < body.index('value="old.csv"')
    assert 'Uploaded files <span class="ws-count">(2)</span>' in body


def test_breakdown_counts_mixed_file(monkeypatch, tmp_path):
    """A mixed list reports per-type counts (websites / iOS / Android)."""
    input_dir, _ = _client_dirs(monkeypatch, tmp_path)
    (input_dir / "mixed.csv").write_text(
        "identifier\n"
        "example.com\n"
        "another-site.org\n"
        "https://third.io\n"
        "284882215\n"  # iOS App Store ID
        "com.example.app\n",  # Android package
        encoding="utf-8",
    )

    breakdown = web_service._get_breakdown(input_dir / "mixed.csv")
    assert breakdown == {"websites": 3, "ios": 1, "android": 1, "total": 5}
    assert web_service._breakdown_label(breakdown) == "3 Websites · 1 iOS · 1 Android"

    # Each non-zero type becomes one colour-coded segment/dot, in display order,
    # with its share of the total for the composition bar.
    chips = web_service._breakdown_chips(breakdown)
    assert chips == [
        {"key": "websites", "seg": "seg-site", "label": "Websites", "count": 3, "pct": 60.0},
        {"key": "ios", "seg": "seg-ios", "label": "iOS", "count": 1, "pct": 20.0},
        {"key": "android", "seg": "seg-android", "label": "Android", "count": 1, "pct": 20.0},
    ]

    files = web_service._list_input_files()
    assert files[0]["breakdown_chips"] == chips
    assert files[0]["breakdown_total"] == 5

    with TestClient(web_service.app) as client:
        client.cookies.set("elcano_auth", _auth_cookie())
        body = client.get("/").text
    # Composition bar segments, total row count, and labelled dot chips.
    assert '<span class="comp-seg seg-site" style="width: 60.0%;"></span>' in body
    assert "<strong>5</strong> rows" in body
    assert '<span class="tchip"><span class="tdot seg-site"></span>Websites <b>3</b></span>' in body
    assert '<span class="tchip"><span class="tdot seg-ios"></span>iOS <b>1</b></span>' in body


def test_breakdown_headerless_domain_list(monkeypatch, tmp_path):
    """A headerless list counts every row — the first line isn't eaten as a
    header (the off-by-one fix)."""
    input_dir, _ = _client_dirs(monkeypatch, tmp_path)
    # No header row: the file goes straight into domains.
    (input_dir / "domains.csv").write_text("example.com\nfoo.com\nbar.com\n", encoding="utf-8")

    breakdown = web_service._get_breakdown(input_dir / "domains.csv")
    # All three rows are counted, not two.
    assert breakdown == {"websites": 3, "ios": 0, "android": 0, "total": 3}


def test_breakdown_headerless_mixed_list(monkeypatch, tmp_path):
    """A headerless list whose first row is an app ID (not a website) still
    counts every row — detection isn't website-only."""
    input_dir, _ = _client_dirs(monkeypatch, tmp_path)
    (input_dir / "mixed.csv").write_text(
        "284882215\nexample.com\ncom.example.app\n", encoding="utf-8"
    )

    breakdown = web_service._get_breakdown(input_dir / "mixed.csv")
    assert breakdown == {"websites": 1, "ios": 1, "android": 1, "total": 3}


def test_breakdown_cached_by_mtime(monkeypatch, tmp_path):
    """The breakdown sidecar is reused until the file changes."""
    input_dir, _ = _client_dirs(monkeypatch, tmp_path)
    target = input_dir / "list.csv"
    # "Domain" is a recognised header, so it stays a 1-row file.
    target.write_text("Domain\nexample.com\n", encoding="utf-8")

    first = web_service._get_breakdown(target)
    assert first == {"websites": 1, "ios": 0, "android": 0, "total": 1}
    cache_file = input_dir / web_service.BREAKDOWN_CACHE_DIRNAME / "list.csv.json"
    assert cache_file.exists()
    # The cache dir is a directory, so it never shows up as a listed file.
    assert [f["name"] for f in web_service._list_input_files()] == ["list.csv"]
