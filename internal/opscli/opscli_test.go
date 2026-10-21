// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package opscli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ElcanoTek/lens/internal/dotenv"
)

type execCall struct {
	argv []string
	dir  string
}

type harness struct {
	*CLI
	execs  []execCall
	runs   [][]string
	chowns []string
	chmods []string
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func newHarness(t *testing.T, euid int, env map[string]string) *harness {
	t.Helper()
	h := &harness{}
	h.CLI = &CLI{
		Getenv:    func(k string) string { return env[k] },
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		Geteuid:   func() int { return euid },
		LookupUser: func(name string) (int, int, error) {
			if name != "lens" {
				return 0, 0, errors.New("unknown user")
			}
			return 987, 986, nil
		},
		Executable: func() (string, error) { return "/opt/lens/bin/lens", nil },
		Exec: func(argv, _ []string, dir string) error {
			h.execs = append(h.execs, execCall{argv: argv, dir: dir})
			return nil
		},
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		Chown: func(path string, uid, gid int) error {
			h.chowns = append(h.chowns, strings.Join([]string{path, strconv.Itoa(uid), strconv.Itoa(gid)}, " "))
			return nil
		},
		Chmod: func(path string, mode os.FileMode) error {
			h.chmods = append(h.chmods, path+" "+mode.String())
			return nil
		},
		Doctor: func(args []string) int { h.runs = append(h.runs, append([]string{"doctor"}, args...)); return 0 },
		Stdin:  strings.NewReader(""),
	}
	h.Run = func(_ context.Context, argv []string) int {
		h.runs = append(h.runs, argv)
		return 0
	}
	h.Stdout, h.Stderr = &h.stdout, &h.stderr
	return h
}

func (h *harness) lastExec(t *testing.T) execCall {
	t.Helper()
	if len(h.execs) == 0 {
		t.Fatalf("nothing exec'd; stderr=%s", h.stderr.String())
	}
	return h.execs[len(h.execs)-1]
}

const sudoPrefix = "sudo --preserve-env=LENS_SRC_DIR,LENS_APP_DIR,LENS_UPDATE_YES,LENS_UPDATE_BRANCH,LENS_UPDATE_SKIP_DEEP,LENS_KEEP_ROLLBACKS,EDITOR,LENS_APP_USER,LENS_HOME"

func TestServiceCommandsUseSudoOnlyWhenNotRoot(t *testing.T) {
	src := t.TempDir()
	if err := os.Mkdir(filepath.Join(src, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		cmd  string
		args []string
		want string
	}{
		{"start", nil, "systemctl start lens.service"},
		{"stop", nil, "systemctl stop lens.service"},
		{"restart", nil, "systemctl restart lens.service"},
		{"status", nil, "systemctl status lens.service"},
		{"logs", nil, "journalctl -fu lens.service"},
		{"logs", []string{"--since", "-10min"}, "journalctl -u lens.service --since -10min"},
		{"update", []string{"--yes"}, "bash " + src + "/scripts/update.sh --yes"},
		{"rebuild", nil, "env LENS_UPDATE_NO_PULL=1 LENS_UPDATE_YES=1 bash " + src + "/scripts/update.sh"},
		{"host", []string{"update", "--yes"}, "bash " + src + "/scripts/host.sh update --yes"},
		{"doctor", []string{"--json"}, "/opt/lens/bin/lens doctor --json"},
	}
	for _, tc := range cases {
		for _, euid := range []int{0, 1000} {
			h := newHarness(t, euid, map[string]string{"LENS_SRC_DIR": src})
			code := h.Main(tc.cmd, tc.args)
			if code != 0 {
				t.Fatalf("%s: exit %d, stderr=%s", tc.cmd, code, h.stderr.String())
			}
			if euid == 0 && tc.cmd == "doctor" {
				// Root runs doctor in-process.
				if len(h.execs) != 0 || strings.Join(h.runs[0], " ") != "doctor --json" {
					t.Fatalf("doctor as root: execs=%v runs=%v", h.execs, h.runs)
				}
				continue
			}
			want := tc.want
			if euid != 0 {
				want = sudoPrefix + " " + want
			}
			if got := strings.Join(h.lastExec(t).argv, " "); got != want {
				t.Errorf("%s euid=%d:\n got %s\nwant %s", tc.cmd, euid, got, want)
			}
		}
	}
}

func TestUpdateNeedsACheckout(t *testing.T) {
	for _, cmd := range []string{"update", "rebuild"} {
		h := newHarness(t, 0, map[string]string{"LENS_SRC_DIR": t.TempDir()})
		if code := h.Main(cmd, nil); code != 1 || len(h.execs) != 0 {
			t.Fatalf("%s: code=%d execs=%v", cmd, code, h.execs)
		}
		if !strings.Contains(h.stderr.String(), "no source checkout at") {
			t.Fatalf("stderr = %q", h.stderr.String())
		}
	}
}

func TestHelpAndUnknown(t *testing.T) {
	h := newHarness(t, 1000, nil)
	if code := h.Main("help", nil); code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"lens update [--yes]", "lens doctor", "lens access grant", "lens submit FILE",
		"lens token create --label NAME", "lens tls status|reload|restart", "lens env edit", "lens host check"} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Errorf("help lacks %q", want)
		}
	}
	if code := h.Main("bogus", nil); code != 2 || !strings.Contains(h.stderr.String(), "unknown command: bogus") {
		t.Fatalf("code=%d stderr=%q", code, h.stderr.String())
	}
}

func TestEnvShowRedactsSecrets(t *testing.T) {
	in := strings.Join([]string{
		"# OPENROUTER_API_KEY=commented",
		"  # indented comment",
		"OPENROUTER_API_KEY=sk-live",
		"AUTH_SIGNING_PUBKEY='abc='",
		"LENS_SESSION_SECRET=s3cret",
		"LENS_AUTH_MODE=central",
		"LENS_PUBLIC_URL=https://lens.example.com",
		"EMPTY=",
		"",
		"no equals sign",
		"ADMIN_PASSWORD=hunter2",
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := RedactEnv(&out, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	want := "OPENROUTER_API_KEY=[REDACTED]\nAUTH_SIGNING_PUBKEY=[REDACTED]\nLENS_SESSION_SECRET=[REDACTED]\n" +
		"LENS_AUTH_MODE=central\nLENS_PUBLIC_URL=https://lens.example.com\nEMPTY=\nADMIN_PASSWORD=[REDACTED]\n"
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
	for _, secret := range []string{"sk-live", "abc=", "s3cret", "hunter2", "commented"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("leaked %q", secret)
		}
	}
}

func TestEnvShowAsRootAndViaSudo(t *testing.T) {
	app := t.TempDir()
	if err := os.WriteFile(filepath.Join(app, ".env"), []byte("OPENROUTER_API_KEY=x\nLENS_AUTH_MODE=elcano\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, 0, map[string]string{"LENS_APP_DIR": app})
	if code := h.Main("env", nil); code != 0 {
		t.Fatal(h.stderr.String())
	}
	if h.stdout.String() != "OPENROUTER_API_KEY=[REDACTED]\nLENS_AUTH_MODE=elcano\n" {
		t.Fatalf("stdout = %q", h.stdout.String())
	}
	h = newHarness(t, 1000, map[string]string{"LENS_APP_DIR": app})
	h.Main("env", []string{"show"})
	if got := strings.Join(h.lastExec(t).argv, " "); got != sudoPrefix+" /opt/lens/bin/lens env show" {
		t.Fatalf("exec = %s", got)
	}
	h = newHarness(t, 0, map[string]string{"LENS_APP_DIR": t.TempDir()})
	if code := h.Main("env", nil); code != 1 || !strings.Contains(h.stderr.String(), "missing — run bootstrap.sh") {
		t.Fatalf("code=%d stderr=%q", code, h.stderr.String())
	}
}

// The editor may replace the inode (vim does), leaving a root-owned .env that
// every job then fails to open; edit must hand it back to the service user.
func TestEnvEditReturnsOwnershipToTheServiceUser(t *testing.T) {
	app := t.TempDir()
	env := filepath.Join(app, ".env")
	if err := os.WriteFile(env, []byte("OPENROUTER_API_KEY=present\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	editor := filepath.Join(t.TempDir(), "ed")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nprintf 'OPENROUTER_API_KEY=edited\\n' > \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, 0, map[string]string{"LENS_APP_DIR": app, "EDITOR": editor})
	h.Run = New().runChild // the real child runner, so the stub editor actually edits
	if code := h.Main("env", []string{"edit"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if len(h.chowns) != 1 || h.chowns[0] != env+" 987 986" {
		t.Fatalf("chowns = %v", h.chowns)
	}
	if len(h.chmods) != 1 || h.chmods[0] != env+" -rw-------" {
		t.Fatalf("chmods = %v", h.chmods)
	}
	raw, _ := os.ReadFile(env)
	if string(raw) != "OPENROUTER_API_KEY=edited\n" {
		t.Fatalf(".env = %q", raw)
	}

	// Not root: the whole edit re-runs under sudo, with EDITOR preserved.
	h = newHarness(t, 1000, map[string]string{"LENS_APP_DIR": app, "EDITOR": editor})
	h.Main("env", []string{"edit"})
	if got := strings.Join(h.lastExec(t).argv, " "); got != sudoPrefix+" /opt/lens/bin/lens env edit" {
		t.Fatalf("exec = %s", got)
	}
}

func TestAsServiceUser(t *testing.T) {
	env := map[string]string{"LENS_APP_DIR": "/srv/lens", "LENS_API_TOKEN_DB": "/var/lib/lens/t.db"}
	// Already the service user: carry on in-process.
	h := newHarness(t, 987, env)
	if done, _ := h.AsServiceUser([]string{"token", "list"}); done || len(h.execs) != 0 {
		t.Fatalf("done=%v execs=%v", done, h.execs)
	}
	// Root: runuser, from the Lens home.
	h = newHarness(t, 0, env)
	done, code := h.AsServiceUser([]string{"token", "create", "--label", "fleet-acme"})
	if !done || code != 0 {
		t.Fatalf("done=%v code=%d", done, code)
	}
	call := h.lastExec(t)
	want := "runuser -u lens -- env LENS_HOME=/srv/lens LENS_APP_DIR=/srv/lens LENS_API_TOKEN_DB=/var/lib/lens/t.db /opt/lens/bin/lens token create --label fleet-acme"
	if got := strings.Join(call.argv, " "); got != want || call.dir != "/srv/lens" {
		t.Fatalf("exec = %s (dir %s)", got, call.dir)
	}
	// Another account: sudo -u.
	h = newHarness(t, 1000, env)
	h.AsServiceUser([]string{"submit", "/tmp/x.csv"})
	if got := strings.Join(h.lastExec(t).argv[:3], " "); got != "sudo -u lens" {
		t.Fatalf("exec = %v", h.lastExec(t).argv)
	}
	// Missing account.
	h = newHarness(t, 0, map[string]string{"LENS_APP_USER": "nobody-here"})
	if done, code := h.AsServiceUser([]string{"token", "list"}); !done || code != 1 {
		t.Fatalf("done=%v code=%d", done, code)
	}
}

func TestEnvSetMatchesPythonDotenvSetKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	orig := "# keep me\nTYPESAFE_API_KEY='existing-secret'\nOTHER_SETTING='preserved'\nexport AUTH_CLIENT_ID=old\nMULTI=\"line one\nline two\"\nTAIL=no-newline"
	if err := os.WriteFile(path, []byte(orig), 0o640); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"AUTH_CLIENT_ID": "lens", "MULTI": "one", "NEW_KEY": "it's"}
	h := newHarness(t, 0, env)
	if code := h.Main("env", []string{"set", path, "AUTH_CLIENT_ID", "MULTI", "NEW_KEY"}); code != 0 {
		t.Fatal(h.stderr.String())
	}
	raw, _ := os.ReadFile(path)
	want := "# keep me\nTYPESAFE_API_KEY='existing-secret'\nOTHER_SETTING='preserved'\nAUTH_CLIENT_ID='lens'\nMULTI='one'\nTAIL=no-newline\nNEW_KEY='it\\'s'\n"
	if string(raw) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", raw, want)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	if code := h.Main("env", []string{"set", path, "UNSET_KEY"}); code != 1 {
		t.Fatal("an unset variable must fail, not write an empty value")
	}
}

// The bootstrap round trip: reuse an existing TYPESAFE_API_KEY (dump0 →
// exported), write the merged keys back (set), never lose other settings.
func TestBootstrapEnvRoundTripPreservesExistingKey(t *testing.T) {
	for _, existing := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), ".env")
		if existing {
			if err := os.WriteFile(path, []byte("TYPESAFE_API_KEY='existing-secret'\nOTHER_SETTING='preserved'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		env := map[string]string{"TYPESAFE_API_KEY": "provided-secret", "OPENROUTER_API_KEY": "test"}
		if existing {
			h := newHarness(t, 0, nil)
			if code := h.Main("env", []string{"dump0", path}); code != 0 {
				t.Fatal(h.stderr.String())
			}
			fields := strings.Split(strings.TrimSuffix(h.stdout.String(), "\x00"), "\x00")
			for i := 0; i+1 < len(fields); i += 2 {
				if strings.HasPrefix(fields[i], "TYPESAFE_") {
					env[fields[i]] = fields[i+1]
				}
			}
		}
		h := newHarness(t, 0, env)
		if code := h.Main("env", []string{"set", path, "OPENROUTER_API_KEY", "TYPESAFE_API_KEY"}); code != 0 {
			t.Fatal(h.stderr.String())
		}
		pairs, err := dotenv.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, p := range pairs {
			if p.Value != nil {
				got[p.Key] = *p.Value
			}
		}
		want := "provided-secret"
		if existing {
			want = "existing-secret"
			if got["OTHER_SETTING"] != "preserved" {
				t.Fatalf("lost OTHER_SETTING: %v", got)
			}
		}
		if got["TYPESAFE_API_KEY"] != want {
			t.Fatalf("existing=%v TYPESAFE_API_KEY=%q", existing, got["TYPESAFE_API_KEY"])
		}
		if strings.Contains(h.stdout.String()+h.stderr.String(), "secret") {
			t.Fatal("set printed a value")
		}
	}
}

func TestTLSStatusFindsEitherSiteLocation(t *testing.T) {
	root := t.TempDir()
	site := filepath.Join(root, "etc/caddy/Caddyfile.d/lens.caddyfile")
	if err := os.MkdirAll(filepath.Dir(site), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Caddy site block for Lens, imported by /etc/caddy/Caddyfile via\n\n" +
		"127.0.0.1 {\n\treverse_proxy 127.0.0.1:8808\n}\n"
	if err := os.WriteFile(site, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := siteHost(site); got != "127.0.0.1" {
		t.Fatalf("siteHost = %q", got)
	}
	h := newHarness(t, 1000, nil)
	h.Root = root
	if code := h.Main("tls", []string{"status"}); code != 0 {
		t.Fatal(h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "Host: 127.0.0.1") || !strings.Contains(out, "could not reach 127.0.0.1:443") {
		t.Fatalf("stdout = %q", out)
	}
	if strings.Join(h.runs[len(h.runs)-1], " ") != "systemctl is-active caddy.service" {
		t.Fatalf("runs = %v", h.runs)
	}

	h = newHarness(t, 1000, nil)
	h.Root = t.TempDir()
	h.Main("tls", nil)
	if !strings.Contains(h.stdout.String(), "No Caddy site block at") {
		t.Fatalf("stdout = %q", h.stdout.String())
	}

	h = newHarness(t, 1000, nil)
	h.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if code := h.Main("tls", []string{"status"}); code != 1 || !strings.Contains(h.stderr.String(), "Caddy not installed") {
		t.Fatalf("code=%d stderr=%q", code, h.stderr.String())
	}

	h = newHarness(t, 1000, nil)
	h.Main("tls", []string{"reload"})
	if got := strings.Join(h.lastExec(t).argv, " "); got != sudoPrefix+" caddy reload --config /etc/caddy/Caddyfile" {
		t.Fatalf("exec = %s", got)
	}
}
