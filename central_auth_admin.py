#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Operator CLI for the Lens-local central-auth allowlist."""

import argparse

from central_auth import CentralAuthStore


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
    if args.command == "grant":
        store.grant_access(args.email)
        print(f"granted {args.email.strip().casefold()}")
    elif args.command == "revoke":
        store.revoke_access(args.email)
        print(f"revoked {args.email.strip().casefold()}")
    else:
        for email in store.list_access():
            print(email)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
