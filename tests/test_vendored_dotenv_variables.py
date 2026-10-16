# SPDX-License-Identifier: BSD-3-Clause
# Ported from python-dotenv 1.2.3, upstream path tests/test_variables.py, to test the copy
# vendored in _vendor/dotenv/. Modified: imports point at the vendored package.
# Copyright and licence: see _vendor/dotenv/LICENSE.

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import pytest

from _vendor.dotenv.variables import Literal, Variable, parse_variables


@pytest.mark.parametrize(
    "value,expected",
    [
        ("", []),
        ("a", [Literal(value="a")]),
        ("${a}", [Variable(name="a", default=None)]),
        ("${a:-b}", [Variable(name="a", default="b")]),
        (
            "${a}${b}",
            [
                Variable(name="a", default=None),
                Variable(name="b", default=None),
            ],
        ),
        (
            "a${b}c${d}e",
            [
                Literal(value="a"),
                Variable(name="b", default=None),
                Literal(value="c"),
                Variable(name="d", default=None),
                Literal(value="e"),
            ],
        ),
    ],
)
def test_parse_variables(value, expected):
    result = parse_variables(value)

    assert list(result) == expected
