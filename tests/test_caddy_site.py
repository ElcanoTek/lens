# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""Caddy site placement: bootstrap must write where the Caddyfile imports.

Ported with Explorer's helper. Stock Fedora's Caddyfile already imports
Caddyfile.d/*.caddyfile, so a site written to an unimported conf.d is never
served and never gets a certificate.
"""

import json
import re
import subprocess
from pathlib import Path

import pytest

SCRIPTS = Path(__file__).resolve().parent.parent / "scripts"


def _caddy_helper(script: str, *args: str):
    return subprocess.run(
        [
            "bash",
            "-euo",
            "pipefail",
            "-c",
            f'source "$1"; {script}',
            "bash",
            str(SCRIPTS / "lib/caddy-site.sh"),
            *args,
        ],
        capture_output=True,
        text=True,
        check=False,
    )


def test_caddy_template_contract_matches_installer():
    """Rendered sites must retain their cleanup marker and expected upstream.

    The marker is also a compatibility identifier for files already installed
    by earlier releases; changing both copies would strand those files.
    """
    template = (SCRIPTS.parent / "deploy/lens.caddy").read_text()
    bootstrap = (SCRIPTS / "bootstrap.sh").read_text()
    marker_result = _caddy_helper('printf "%s\\n" "$LENS_CADDY_MARKER"')
    assert marker_result.returncode == 0, marker_result.stderr
    marker = marker_result.stdout.rstrip("\n")
    assert marker == "# Caddy site block for Lens, imported by /etc/caddy/Caddyfile via"
    assert template.splitlines()[0] == marker

    match = re.search(
        r'lens_caddy_adapted_has_site "\$adapted" "\$HOSTNAME_FOR_TLS" "([^"]+)"',
        bootstrap,
    )
    assert match, "bootstrap no longer checks the rendered site's upstream"
    assert re.findall(r"^\s*reverse_proxy\s+(\S+)\s*$", template, re.MULTILINE) == [match.group(1)]


@pytest.mark.parametrize(
    ("import_line", "target", "add_import"),
    [
        ("import Caddyfile.d/*.caddyfile", "Caddyfile.d/lens.caddyfile", "0"),
        ("", "conf.d/lens.caddy", "1"),
        ('import "Caddyfile.d/*.caddyfile"', "Caddyfile.d/lens.caddyfile", "0"),
        ("import ./conf.d/*", "conf.d/lens.caddy", "0"),
        ("import conf.d/*.caddy", "conf.d/lens.caddy", "0"),
    ],
)
def test_caddy_plan_uses_a_loaded_glob(
    tmp_path: Path, import_line: str, target: str, add_import: str
):
    caddyfile = tmp_path / "Caddyfile"
    caddyfile.write_text(import_line + "\n")
    result = _caddy_helper(
        'lens_caddy_plan "$2"; printf "%s\\n%s\\n" "$LENS_CADDY_TARGET" "$LENS_CADDY_ADD_IMPORT"',
        str(caddyfile),
    )
    assert result.returncode == 0, result.stderr
    assert result.stdout.splitlines() == [str(tmp_path / target), add_import]


def test_caddy_plan_resolves_absolute_import(tmp_path: Path):
    imported = tmp_path / "different" / "*.caddyfile"
    caddyfile = tmp_path / "Caddyfile"
    caddyfile.write_text(f'import "{imported}"\n')
    result = _caddy_helper(
        'lens_caddy_plan "$2"; printf "%s\\n%s\\n" "$LENS_CADDY_TARGET" "$LENS_CADDY_ADD_IMPORT"',
        str(caddyfile),
    )
    assert result.returncode == 0, result.stderr
    assert result.stdout.splitlines() == [
        str(tmp_path / "different/lens.caddyfile"),
        "0",
    ]


@pytest.mark.parametrize("orphan_also_imported", [False, True])
def test_caddy_plan_preserves_loaded_manual_copy_and_removes_only_owned_orphan(
    tmp_path: Path, orphan_also_imported: bool
):
    caddyfile = tmp_path / "Caddyfile"
    imports = "import Caddyfile.d/*.caddyfile\n"
    if orphan_also_imported:
        imports += "import conf.d/*.caddy\n"
    caddyfile.write_text(imports)
    loaded = tmp_path / "Caddyfile.d/lens.caddyfile"
    orphan = tmp_path / "conf.d/lens.caddy"
    unrelated = tmp_path / "conf.d/other.caddy"
    for path in (loaded, orphan, unrelated):
        path.parent.mkdir(exist_ok=True)
    marker = "# Caddy site block for Lens, imported by /etc/caddy/Caddyfile via"
    content = f"{marker}\nlens.example {{\n}}\n"
    loaded.write_text(content)
    orphan.write_text(content)
    unrelated.write_text("other.example {\n}\n")
    result = _caddy_helper(
        'lens_caddy_plan "$2"; printf "%s\\n%s\\n" "$LENS_CADDY_TARGET" "$LENS_CADDY_ADD_IMPORT"; lens_caddy_remove_stale "$2" "$LENS_CADDY_TARGET"',
        str(caddyfile),
    )
    assert result.returncode == 0, result.stderr
    assert result.stdout.splitlines() == [str(loaded), "0", str(orphan)]
    assert loaded.read_text() == content
    assert not orphan.exists()
    assert unrelated.exists()


def test_caddy_cleanup_removes_owned_old_hostname_but_not_unmarked_file(
    tmp_path: Path,
):
    caddyfile = tmp_path / "Caddyfile"
    caddyfile.write_text("import Caddyfile.d/*.caddyfile\n")
    directory = tmp_path / "conf.d"
    directory.mkdir()
    unmarked = directory / "lens.caddy"
    old_hostname = directory / "old.caddy"
    unmarked.write_text("lens.example {\n}\n")
    old_hostname.write_text(
        "# Caddy site block for Lens, imported by /etc/caddy/Caddyfile via\nother.example {\n}\n"
    )
    result = _caddy_helper(
        'lens_caddy_plan "$2"; lens_caddy_remove_stale "$2" "$LENS_CADDY_TARGET"',
        str(caddyfile),
    )
    assert result.returncode == 0, result.stderr
    assert unmarked.exists()
    assert not old_hostname.exists()


def test_caddy_adapted_check_requires_host_matcher_and_lens_proxy(tmp_path: Path):
    adapted = tmp_path / "adapted.json"
    route = {
        "match": [{"host": ["other.example"]}],
        "handle": [
            {
                "handler": "subroute",
                "routes": [
                    {
                        "handle": [
                            {
                                "handler": "reverse_proxy",
                                "upstreams": [{"dial": "127.0.0.1:8808"}],
                            }
                        ]
                    }
                ],
            }
        ],
    }
    payload = {
        "email": "lens.example",
        "apps": {"http": {"servers": {"srv0": {"routes": [route]}}}},
    }
    adapted.write_text(json.dumps(payload))
    script = 'lens_caddy_adapted_has_site "$2" lens.example 127.0.0.1:8808'
    assert _caddy_helper(script, str(adapted)).returncode != 0
    route["match"][0]["host"] = ["lens.example"]
    adapted.write_text(json.dumps(payload))
    result = _caddy_helper(script, str(adapted))
    assert result.returncode == 0, result.stderr
    route["handle"][0]["routes"][0]["handle"][0]["upstreams"][0]["dial"] = "127.0.0.1:9999"
    adapted.write_text(json.dumps(payload))
    assert _caddy_helper(script, str(adapted)).returncode != 0


def test_bootstrap_uses_the_tested_caddy_helpers_before_opening_firewall():
    text = (SCRIPTS / "bootstrap.sh").read_text()
    assert 'lens_caddy_plan "$caddyfile"' in text
    assert 'lens_caddy_remove_stale "$caddyfile"' in text
    assert 'lens_caddy_adapted_has_site "$adapted"' in text
    assert text.index("caddy validate --config") < text.index("firewall-cmd --add-service")
