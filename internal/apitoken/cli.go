// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package apitoken

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

const cliUsage = `usage: lens token [-h] {create,list,revoke} ...

Manage scoped API tokens for Fleet and other callers.

commands:
  create --label NAME [--scope {jobs:read,jobs:run}]... [--json]
                        mint a token (shown once); --scope is repeatable and
                        defaults to both jobs:read and jobs:run
  list                  show tokens (never their secrets)
  revoke ID             revoke a token by id
`

// Main is `lens token create|list|revoke`. It uses LENS_API_TOKEN_DB (or the
// deployed default) and returns the process exit status: 0 on success, 1 on
// an operational error, 2 on a usage error.
func Main(args []string) int {
	return run(context.Background(), args, os.Stdout, os.Stderr, NewStore(DBPath()), os.Getenv("LENS_PUBLIC_URL"))
}

func usageError(stderr io.Writer, msg string) int {
	_, _ = fmt.Fprint(stderr, strings.SplitN(cliUsage, "\n", 2)[0]+"\n")
	_, _ = fmt.Fprintf(stderr, "lens token: error: %s\n", msg)
	return 2
}

type createArgs struct {
	label    string
	hasLabel bool
	scopes   []string
	json     bool
}

func parseCreate(args []string) (createArgs, string, bool) {
	var out createArgs
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "-h", "--help":
			return out, "", true
		case "--json":
			if hasValue {
				return out, "argument --json: ignored explicit argument '" + value + "'", false
			}
			out.json = true
		case "--label", "--scope":
			if !hasValue {
				if len(args) == 0 {
					return out, "argument " + name + ": expected one argument", false
				}
				value, args = args[0], args[1:]
			}
			if name == "--label" {
				out.label, out.hasLabel = value, true
			} else {
				if !slices.Contains(Scopes, value) {
					return out, fmt.Sprintf("argument --scope: invalid choice: '%s' (choose from %s)", value, quotedScopes()), false
				}
				out.scopes = append(out.scopes, value)
			}
		default:
			return out, "unrecognized arguments: " + arg, false
		}
	}
	if !out.hasLabel {
		return out, "the following arguments are required: --label", false
	}
	return out, "", false
}

func quotedScopes() string {
	quoted := make([]string, len(Scopes))
	for i, s := range Scopes {
		quoted[i] = "'" + s + "'"
	}
	return strings.Join(quoted, ", ")
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, store *Store, publicURL string) int {
	defer func() { _ = store.Close() }()
	if len(args) == 0 {
		return usageError(stderr, "the following arguments are required: command")
	}
	command, rest := args[0], args[1:]
	switch command {
	case "-h", "--help":
		_, _ = fmt.Fprint(stdout, cliUsage)
		return 0
	case "create":
		parsed, msg, help := parseCreate(rest)
		if help {
			_, _ = fmt.Fprint(stdout, cliUsage)
			return 0
		}
		if msg != "" {
			return usageError(stderr, msg)
		}
		scopes := parsed.scopes
		if len(scopes) == 0 {
			scopes = Scopes
		}
		token, raw, err := store.Create(ctx, parsed.label, scopes)
		if err != nil {
			return operational(stderr, err)
		}
		if parsed.json {
			printCreatedJSON(stdout, token, raw)
			return 0
		}
		_, _ = fmt.Fprintf(stdout, "Created token %d (%s) with scopes: %s\n", token.ID, token.Label, strings.Join(token.Scopes, " "))
		_, _ = fmt.Fprint(stdout, "Copy it now; Lens stores only its hash and cannot show it again:\n\n")
		_, _ = fmt.Fprintf(stdout, "  %s\n\n", raw)
		base := strings.TrimRight(strings.TrimSpace(publicURL), "/")
		if base == "" {
			base = "https://<this host>"
		}
		_, _ = fmt.Fprintln(stdout, "For Fleet, set in its .env:")
		_, _ = fmt.Fprintf(stdout, "  LENS_MCP_URL=%s/mcp\n", base)
		_, _ = fmt.Fprintln(stdout, "  LENS_API_TOKEN=<the token above>")
		return 0
	case "list":
		if len(rest) > 0 {
			if rest[0] == "-h" || rest[0] == "--help" {
				_, _ = fmt.Fprint(stdout, cliUsage)
				return 0
			}
			return usageError(stderr, "unrecognized arguments: "+strings.Join(rest, " "))
		}
		tokens, err := store.List(ctx)
		if err != nil {
			return operational(stderr, err)
		}
		printTable(stdout, tokens)
		return 0
	case "revoke":
		if len(rest) == 1 && (rest[0] == "-h" || rest[0] == "--help") {
			_, _ = fmt.Fprint(stdout, cliUsage)
			return 0
		}
		if len(rest) == 0 {
			return usageError(stderr, "the following arguments are required: id")
		}
		if len(rest) > 1 {
			return usageError(stderr, "unrecognized arguments: "+strings.Join(rest[1:], " "))
		}
		id, err := strconv.ParseInt(strings.TrimSpace(rest[0]), 10, 64)
		if err != nil {
			return usageError(stderr, fmt.Sprintf("argument id: invalid int value: '%s'", rest[0]))
		}
		revoked, err := store.Revoke(ctx, id)
		if err != nil {
			return operational(stderr, err)
		}
		if !revoked {
			_, _ = fmt.Fprintf(stderr, "No active token with id %d\n", id)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "Revoked token %d. Its download links stopped working too.\n", id)
		return 0
	default:
		return usageError(stderr, fmt.Sprintf("argument command: invalid choice: '%s' (choose from 'create', 'list', 'revoke')", command))
	}
}

func operational(stderr io.Writer, err error) int {
	var verr *ValidationError
	if errors.As(err, &verr) {
		_, _ = fmt.Fprintf(stderr, "lens token: %s\n", verr.msg)
	} else {
		_, _ = fmt.Fprintf(stderr, "lens token: %v\n", err)
	}
	return 1
}

// printCreatedJSON matches Python's json.dumps default formatting
// (", " and ": " separators, keys in this order).
func printCreatedJSON(w io.Writer, token *Token, raw string) {
	quote := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}
	scopes := make([]string, len(token.Scopes))
	for i, s := range token.Scopes {
		scopes[i] = quote(s)
	}
	_, _ = fmt.Fprintf(w, `{"id": %d, "label": %s, "scopes": [%s], "token": %s}`+"\n",
		token.ID, quote(token.Label), strings.Join(scopes, ", "), quote(raw))
}

func printTable(w io.Writer, tokens []*Token) {
	if len(tokens) == 0 {
		_, _ = fmt.Fprintln(w, "No API tokens. Create one with: lens token create --label NAME")
		return
	}
	_, _ = fmt.Fprintf(w, "%4s  %-8s  %-14s  %-20s  %-25s  LABEL\n", "ID", "STATE", "PREFIX", "SCOPES", "LAST USED")
	for _, token := range tokens {
		state := "active"
		if token.RevokedAt != "" {
			state = "revoked"
		}
		lastUsed := token.LastUsedAt
		if lastUsed == "" {
			lastUsed = "never"
		}
		_, _ = fmt.Fprintf(w, "%4d  %-8s  %-14s  %-20s  %-25s  %s\n",
			token.ID, state, token.Prefix+"…", strings.Join(token.Scopes, " "), lastUsed, token.Label)
	}
}
