// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package opscli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ElcanoTek/lens/internal/dotenv"
)

// secretLine is the bash CLI's awk filter: any line mentioning one of these
// words (in the key or the value) is shown as KEY=[REDACTED].
var secretLine = regexp.MustCompile(`TOKEN|KEY|SECRET|PASSWORD|PUBKEY`)

func (c *CLI) env(args []string) int {
	sub := "show"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "show", "":
		if c.Geteuid() != 0 {
			return c.reexecAsRoot("env", []string{"show"})
		}
		return c.envShow()
	case "edit":
		if c.Geteuid() != 0 {
			return c.reexecAsRoot("env", []string{"edit"})
		}
		return c.envEdit()
	case "dump0":
		// Internal (bootstrap.sh): KEY\0VALUE\0 for every bound key, so the
		// installer can reuse an existing .env without sourcing it as a
		// root shell script.
		if len(args) != 1 {
			fmt.Fprintln(c.Stderr, "usage: lens env dump0 FILE")
			return 2
		}
		return c.envDump0(args[0])
	case "set":
		// Internal (bootstrap.sh): merge KEY=$KEY for each named key into
		// FILE. Values come from the environment, never argv, so they do not
		// show up in ps.
		if len(args) < 2 {
			fmt.Fprintln(c.Stderr, "usage: lens env set FILE KEY...")
			return 2
		}
		return c.envSet(args[0], args[1:])
	default:
		fmt.Fprintln(c.Stderr, "usage: lens env [show|edit]")
		return 1
	}
}

func (c *CLI) missingEnv() int {
	fmt.Fprintf(c.Stderr, "%s missing — run bootstrap.sh\n", c.envFile())
	return 1
}

func (c *CLI) envShow() int {
	raw, err := os.ReadFile(c.envFile())
	if err != nil {
		return c.missingEnv()
	}
	if err := RedactEnv(c.Stdout, bytes.NewReader(raw)); err != nil {
		fmt.Fprintf(c.Stderr, "lens env: %v\n", err)
		return 1
	}
	return 0
}

// RedactEnv copies a .env to w the way `lens env show` prints it: comments
// and lines without "=" are dropped, and any line mentioning TOKEN, KEY,
// SECRET, PASSWORD or PUBKEY becomes "KEY=[REDACTED]". The match is on the
// whole line, like the awk filter it replaces, so a key whose value merely
// contains one of the words is hidden too — erring towards hiding.
func RedactEnv(w io.Writer, r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			continue
		}
		key, _, hasEq := strings.Cut(line, "=")
		switch {
		case secretLine.MatchString(line):
			if _, err := fmt.Fprintf(w, "%s=[REDACTED]\n", key); err != nil {
				return err
			}
		case hasEq:
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

func (c *CLI) envEdit() int {
	// bootstrap.sh / update.sh point operators here to paste
	// AUTH_SIGNING_PUBKEY or the OpenRouter key.
	// The editor is a child, not an exec: a rewrite (vim, most GUI editors)
	// replaces the inode and leaves .env owned by root. The dashboard still
	// starts, because systemd reads EnvironmentFile as root, but every job
	// opens the file as the service user and dies on a permission error. So
	// ownership and mode are put back after the editor exits.
	path := c.envFile()
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return c.missingEnv()
	}
	editor := firstNonEmpty(c.Getenv("EDITOR"), "vi")
	// EDITOR may carry flags ("code --wait"), as with the shell's unquoted
	// expansion in the bash CLI.
	argv := append(strings.Fields(editor), path)
	status := c.Run(context.Background(), argv)
	name := c.serviceUser()
	uid, gid, err := c.LookupUser(name)
	if err != nil {
		fmt.Fprintf(c.Stderr, "lens env edit: service account %q missing: %v\n", name, err)
		return 1
	}
	if err := c.Chown(path, uid, gid); err != nil {
		fmt.Fprintf(c.Stderr, "lens env edit: chown %s:%s %s: %v\n", name, name, path, err)
		return 1
	}
	if err := c.Chmod(path, 0o600); err != nil {
		fmt.Fprintf(c.Stderr, "lens env edit: chmod 0600 %s: %v\n", path, err)
		return 1
	}
	return status
}

func (c *CLI) envDump0(path string) int {
	pairs, err := dotenv.ValuesFile(path, false)
	if err != nil {
		fmt.Fprintf(c.Stderr, "lens env dump0: %v\n", err)
		return 1
	}
	bw := bufio.NewWriter(c.Stdout)
	for _, p := range pairs {
		if p.Value == nil {
			continue
		}
		_, _ = bw.WriteString(p.Key + "\x00" + *p.Value + "\x00")
	}
	if err := bw.Flush(); err != nil {
		fmt.Fprintf(c.Stderr, "lens env dump0: %v\n", err)
		return 1
	}
	return 0
}

func (c *CLI) envSet(path string, keys []string) int {
	values := make([][2]string, 0, len(keys))
	for _, k := range keys {
		v, ok := c.LookupEnv(k)
		if !ok {
			fmt.Fprintf(c.Stderr, "lens env set: %s is not set in the environment\n", k)
			return 1
		}
		values = append(values, [2]string{k, v})
	}
	for _, kv := range values {
		if err := SetKey(path, kv[0], kv[1]); err != nil {
			fmt.Fprintf(c.Stderr, "lens env set: %v\n", err)
			return 1
		}
	}
	return 0
}

// SetKey writes key=value into the .env at path the way python-dotenv's
// set_key(quote_mode="always") did for the Python bootstrap: every existing
// binding of key is replaced in place by KEY='value' (backslashes and single
// quotes escaped as \\ and \'), every other line is kept byte for byte, and a
// missing key is appended, after a newline when the file did not end with one. A missing file is
// created. The rewrite goes through a temporary file in the same directory
// and a rename, with mode 0600.
func SetKey(path, key, value string) error {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-chosen .env path
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// The single-quoted dotenv value decodes \\ and \', so escape the
	// backslashes first, then the quotes; otherwise "a\\b" or a trailing "\"
	// (which would escape the closing quote) would not survive a re-read.
	escaped := strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "'", `\'`)
	lineOut := key + "='" + escaped + "'\n"
	var out strings.Builder
	replaced := false
	lines := splitKeepNewline(string(raw))
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		k, rest, ok := bindingKey(line)
		if !ok || k != key {
			out.WriteString(line)
			continue
		}
		// A quoted value may continue over following lines; swallow them so
		// the old value does not survive as stray lines.
		if q := openQuote(rest); q != 0 {
			for !closesQuote(rest, q) && i+1 < len(lines) {
				i++
				rest += lines[i]
			}
		}
		out.WriteString(lineOut)
		replaced = true
	}
	if !replaced {
		if s := out.String(); s != "" && !strings.HasSuffix(s, "\n") {
			out.WriteString("\n")
		}
		out.WriteString(lineOut)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".env.tmp.*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(out.String()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func splitKeepNewline(s string) []string {
	var out []string
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

// bindingKey returns the key a .env line binds ("[export ]KEY = …") and the
// text after "=".
func bindingKey(line string) (key, rest string, ok bool) {
	t := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(t, "export ") {
		t = strings.TrimLeft(t[len("export "):], " \t")
	}
	k, rest, ok := strings.Cut(t, "=")
	if !ok {
		return "", "", false
	}
	k = strings.TrimRight(k, " \t")
	if k == "" || strings.HasPrefix(k, "#") || strings.ContainsAny(k, " \t") {
		return "", "", false
	}
	return k, rest, true
}

func openQuote(rest string) byte {
	v := strings.TrimLeft(rest, " \t")
	if v != "" && (v[0] == '\'' || v[0] == '"') {
		return v[0]
	}
	return 0
}

// closesQuote reports whether the quoted value starting in rest is closed
// (an unescaped matching quote after the opening one).
func closesQuote(rest string, q byte) bool {
	v := strings.TrimLeft(rest, " \t")[1:]
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\\':
			i++
		case q:
			return true
		}
	}
	return false
}
