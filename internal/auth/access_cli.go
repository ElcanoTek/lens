// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

const accessUsage = "usage: lens access [-h] {grant,revoke,list} ...\n"

// AccessMain is `lens access grant EMAIL | revoke EMAIL | list`: the operator
// CLI for the Lens-local central-auth allowlist in LENS_ACCESS_DB. It returns
// the exit status (0 ok, 1 error, 2 usage).
func AccessMain(args []string) int {
	return accessMain(context.Background(), args, os.Stdout, os.Stderr, os.LookupEnv)
}

func accessMain(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) (string, bool)) int {
	usage := func(msg string) int {
		_, _ = fmt.Fprint(stderr, accessUsage)
		_, _ = fmt.Fprintf(stderr, "lens access: error: %s\n", msg)
		return 2
	}
	if len(args) == 0 {
		return usage("the following arguments are required: command")
	}
	command, rest := args[0], args[1:]
	switch command {
	case "-h", "--help":
		_, _ = fmt.Fprint(stdout, accessUsage+"\nManage Lens central-auth access\n\n"+
			"  grant EMAIL    allow EMAIL to sign in\n  revoke EMAIL   remove EMAIL and end its sessions\n  list           show allowed emails\n")
		return 0
	case "grant", "revoke":
		if len(rest) == 0 {
			return usage("the following arguments are required: email")
		}
		if len(rest) > 1 {
			return usage("unrecognized arguments: " + strings.Join(rest[1:], " "))
		}
	case "list":
		if len(rest) > 0 {
			return usage("unrecognized arguments: " + strings.Join(rest, " "))
		}
	default:
		return usage(fmt.Sprintf("argument command: invalid choice: '%s' (choose from 'grant', 'revoke', 'list')", command))
	}

	store, err := OpenStoreFromEnv(ctx, getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	switch command {
	case "grant":
		if err := store.GrantAccess(ctx, rest[0]); err != nil {
			return accessError(stderr, err)
		}
		normalized, _ := NormalizeEmail(rest[0])
		_, _ = fmt.Fprintf(stdout, "granted %s\n", normalized)
	case "revoke":
		revoked, err := store.RevokeAccess(ctx, rest[0])
		if err != nil {
			return accessError(stderr, err)
		}
		normalized, _ := NormalizeEmail(rest[0])
		if !revoked {
			// Nothing changed, so say so: an operator revoking a typo must
			// not walk away believing the real account is out.
			_, _ = fmt.Fprintf(stderr, "error: %s is not currently allowed\n", normalized)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "revoked %s\n", normalized)
	default:
		emails, err := store.ListAccess(ctx)
		if err != nil {
			return accessError(stderr, err)
		}
		for _, email := range emails {
			_, _ = fmt.Fprintln(stdout, email)
		}
	}
	return 0
}

func accessError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
	return 1
}
