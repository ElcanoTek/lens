#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Operator CLI for the Lens-local central-auth allowlist."""

import argparse
import sys

from central_auth import CentralAuthStore, normalize_email


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description="Manage Lens central-auth access")
    commands = result.add_subparsers(dest="command", required=True)
    for name in ("grant", "revoke"):
        command = commands.add_parser(name)
        command.add_argument("email")
    commands.add_parser("list")
    return result


def main() -> int:
    args = parser().parse_args()
    store = CentralAuthStore.from_env()
    try:
        if args.command == "grant":
            store.grant_access(args.email)
            print(f"granted {normalize_email(args.email)}")
        elif args.command == "revoke":
            if not store.revoke_access(args.email):
                # Nothing changed, so say so: an operator revoking a typo must
                # not walk away believing the real account is out.
                print(
                    f"error: {normalize_email(args.email)} is not currently allowed",
                    file=sys.stderr,
                )
                return 1
            print(f"revoked {normalize_email(args.email)}")
        else:
            for email in store.list_access():
                print(email)
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
