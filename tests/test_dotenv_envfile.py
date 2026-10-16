# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""The vendored dotenv parser on the shapes Lens's `.env` actually takes.

The same file is systemd's EnvironmentFile for lens.service and is read by
config.py, scripts/doctor.py and scripts/bootstrap.sh through _vendor.dotenv.
Bootstrap writes it with set_key() (single-quoted values); operators edit it by
hand with `lens env edit`.
"""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from _vendor.dotenv import dotenv_values, set_key


@pytest.mark.parametrize(
    "value",
    [
        "sk-or-v1-0123456789abcdef",
        "MCowBQYDK2VwAyEAq8+/mXk3c2VjcmV0LWtleS1ub3QtcmVhbA==",  # base64 Ed25519 key
        "https://lens.example.com",
        "$(touch /tmp/pwned)",
        "${HOME}",  # stays literal: callers read with interpolate=False
        "with spaces # and a hash",
        "it's quoted",
        "",
    ],
)
def test_bootstrap_written_values_round_trip(tmp_path, value):
    env = tmp_path / ".env"
    env.write_text("# operator comment\nOTHER=kept\n")
    set_key(env, "OPENROUTER_API_KEY", value)
    set_key(env, "LENS_AUTH_MODE", "central")
    values = dotenv_values(env, interpolate=False)
    assert values == {"OTHER": "kept", "OPENROUTER_API_KEY": value, "LENS_AUTH_MODE": "central"}
    assert env.read_text().startswith("# operator comment\n")


def test_hand_edited_environment_file_shapes(tmp_path):
    env = tmp_path / ".env"
    env.write_text(
        "# Lens settings\n"
        "AUTH_SIGNING_PUBKEY=abc+/=\n"
        'LENS_PUBLIC_URL="https://lens.example.com"  # trailing comment\n'
        "export LENS_ACCESS_DB=/var/lib/lens/access.db\n"
        "TYPESAFE_API_KEY=\n"
        "  INDENTED = value with spaces  \n"
        "BARE_FLAG\n"
        "\r\n"
        'MULTI="line one\\nline two"\n'
    )
    assert dotenv_values(env, interpolate=False) == {
        "AUTH_SIGNING_PUBKEY": "abc+/=",
        "LENS_PUBLIC_URL": "https://lens.example.com",
        "LENS_ACCESS_DB": "/var/lib/lens/access.db",
        "TYPESAFE_API_KEY": "",
        "INDENTED": "value with spaces",
        "BARE_FLAG": None,
        "MULTI": "line one\nline two",
    }
