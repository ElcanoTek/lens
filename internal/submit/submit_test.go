// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package submit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const spec = `{"categories": [{"name": "Sports", "type": "boolean", "question": "Is it about sports?"}]}`

func testRunner(t *testing.T) (*Runner, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errOut bytes.Buffer
	r := &Runner{
		Geteuid:    func() int { return 1000 },
		LookupUser: func(string) (int, int, error) { return 990, 990, nil },
		Chown:      func(string, int, int) error { t.Fatal("unexpected chown"); return nil },
		Executable: func() (string, error) { return "/opt/lens/bin/lens", nil },
		Exec:       func([]string, []string) error { t.Fatal("unexpected exec"); return nil },
		AsServiceUser: func([]string) (bool, int) {
			t.Fatal("unexpected re-run")
			return true, 1
		},
		AppDir: t.TempDir(), ServiceUser: "lens",
		Sleep:  func(time.Duration) { t.Fatal("unexpected wait") },
		Stdout: &out, Stderr: &errOut,
	}
	return r, &out, &errOut
}

func cliEntry(t *testing.T, filename string) string {
	t.Helper()
	entry := filepath.Join(t.TempDir(), "submit-cli")
	if err := os.Mkdir(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, filename), []byte("Domain\nexample.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestClientWritesRequestAndReportsTheJob(t *testing.T) {
	r, out, _ := testRunner(t)
	entry := cliEntry(t, "my sites!.csv")
	cats := filepath.Join(entry, "categories.json")
	if err := os.WriteFile(cats, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LENS_PUBLIC_URL", "https://lens.example.com/")
	r.Sleep = func(time.Duration) {
		raw, err := os.ReadFile(filepath.Join(entry, "request.json"))
		if err != nil {
			t.Fatal(err)
		}
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		var want map[string]any
		_ = json.Unmarshal([]byte(spec), &want)
		if req["input_file"] != "my sites_.csv" || req["mode"] != "ctv" || req["name"] != "CTV run" ||
			!reflect.DeepEqual(req["custom_categories"], want) || req["research_fallback"] != true ||
			req["deep_retry"] != false || req["llm_model"] != nil {
			t.Fatalf("request = %v", req)
		}
		if _, err := os.Stat(filepath.Join(entry, "my sites_.csv")); err != nil {
			t.Fatal("input not renamed")
		}
		if _, err := os.Stat(cats); err == nil {
			t.Fatal("categories file left behind")
		}
		_ = os.WriteFile(filepath.Join(entry, "result.json"), []byte(`{"job_id": "J1", "input_file": "my sites_.csv"}`), 0o600)
	}
	code := r.Main([]string{"--entry", entry, "--input", "my sites!.csv", "--ctv", "--no-deep", "--name", "CTV run", "--categories", cats})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if got := out.String(); got != "Queued my sites_.csv as job J1 — https://lens.example.com/\n" {
		t.Fatalf("out %q", got)
	}
	if _, err := os.Stat(entry); err == nil {
		t.Fatal("entry not cleaned up")
	}
}

func TestClientReportsRejection(t *testing.T) {
	r, out, errOut := testRunner(t)
	entry := cliEntry(t, "sites.csv")
	r.Sleep = func(time.Duration) {
		_ = os.WriteFile(filepath.Join(entry, "result.json"), []byte(`{"error": "bad file"}`), 0o600)
	}
	if code := r.Main([]string{"--entry", entry, "--input", "sites.csv"}); code != 1 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut.String(), "lens submit: rejected: bad file") || out.Len() != 0 {
		t.Fatalf("stderr %q", errOut)
	}
}

func TestClientRejectsInvalidCategoriesBeforeQueueing(t *testing.T) {
	r, _, errOut := testRunner(t)
	entry := cliEntry(t, "sites.csv")
	cats := filepath.Join(entry, "categories.json")
	_ = os.WriteFile(cats, []byte("{not json"), 0o600)
	if code := r.Main([]string{"--entry", entry, "--input", "sites.csv", "--categories", cats}); code != 1 {
		t.Fatalf("code %d", code)
	}
	if _, err := os.Stat(entry); err == nil {
		t.Fatal("entry kept")
	}
	if !strings.Contains(errOut.String(), cats+": Custom categories must be valid JSON.") {
		t.Fatalf("stderr %q", errOut)
	}
}

func TestClientTimesOutWhenTheServiceIsDown(t *testing.T) {
	r, _, errOut := testRunner(t)
	r.Sleep = func(time.Duration) { time.Sleep(20 * time.Millisecond) }
	entry := cliEntry(t, "sites.csv")
	if code := r.Main([]string{"--entry", entry, "--input", "sites.csv", "--timeout", "0.1"}); code != 2 {
		t.Fatalf("code %d", code)
	}
	if _, err := os.Stat(filepath.Join(entry, "request.json")); err != nil {
		t.Fatal("request not left for the service")
	}
	if !strings.Contains(errOut.String(), "is lens.service running?") {
		t.Fatalf("stderr %q", errOut)
	}
}

func TestOperatorArgumentErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sites.csv")
	_ = os.WriteFile(file, []byte("Domain\na.com\n"), 0o600)
	for _, tc := range []struct {
		args []string
		code int
		msg  string
	}{
		{nil, 2, "usage: lens submit FILE"},
		{[]string{"--name", "x"}, 2, "usage: lens submit FILE"},
		{[]string{filepath.Join(dir, "missing.csv")}, 1, "no such file"},
		{[]string{file, "--llm-model", "bad model; rm -rf"}, 2, "invalid model id"},
		{[]string{file, "--bogus"}, 2, "bogus"},
		{[]string{file, "extra"}, 2, "unexpected argument"},
		{[]string{file, "--categories", filepath.Join(dir, "nope.json")}, 1, "no such categories file"},
	} {
		r, _, errOut := testRunner(t)
		if code := r.Main(tc.args); code != tc.code || !strings.Contains(errOut.String(), tc.msg) {
			t.Errorf("%v: code %d, stderr %q", tc.args, code, errOut)
		}
	}
}

func TestOperatorAsRootSpoolsAndHandsOffToTheServiceUser(t *testing.T) {
	r, _, _ := testRunner(t)
	spool := filepath.Join(r.AppDir, "managed-files", "spool")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(r.AppDir, ".env"), []byte("OPENROUTER_API_KEY=x\nLENS_PUBLIC_URL=\"https://lens.example.com\"\n"), 0o600)
	t.Setenv("LENS_PUBLIC_URL", "")
	src := t.TempDir()
	file := filepath.Join(src, "my sites.csv")
	cats := filepath.Join(src, "cats.json")
	_ = os.WriteFile(file, []byte("Domain\na.com\n"), 0o600)
	_ = os.WriteFile(cats, []byte(spec), 0o600)

	r.Geteuid = func() int { return 0 }
	var chowned []string
	r.Chown = func(path string, uid, gid int) error {
		if uid != 990 || gid != 990 {
			t.Fatalf("chown %s to %d:%d", path, uid, gid)
		}
		chowned = append(chowned, filepath.Base(path))
		return nil
	}
	var handed []string
	r.AsServiceUser = func(args []string) (bool, int) {
		handed = args
		return true, 0
	}
	if code := r.Main([]string{file, "--name", "deal sites", "--categories", cats, "--ctv", "--decision-model", "~typesafe/jev-latest"}); code != 0 {
		t.Fatalf("code %d", code)
	}
	entries, _ := os.ReadDir(spool)
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "submit-") {
		t.Fatalf("spool = %v", entries)
	}
	entry := filepath.Join(spool, entries[0].Name())
	if st, _ := os.Stat(entry); st.Mode().Perm() != 0o700 {
		t.Fatalf("entry mode %v", st.Mode())
	}
	for _, name := range []string{"my sites.csv", "categories.json"} {
		st, err := os.Stat(filepath.Join(entry, name))
		if err != nil || st.Mode().Perm() != 0o640 {
			t.Fatalf("%s: %v %v", name, st, err)
		}
	}
	slices.Sort(chowned)
	if !reflect.DeepEqual(chowned, []string{"categories.json", "my sites.csv", entries[0].Name()}) {
		t.Fatalf("chowned %v", chowned)
	}
	want := []string{"submit", "--entry", entry, "--input", "my sites.csv", "--name", "deal sites",
		"--categories", filepath.Join(entry, "categories.json"), "--decision-model", "~typesafe/jev-latest", "--ctv", "--timeout=30"}
	if !reflect.DeepEqual(handed, want) {
		t.Fatalf("handed %v\nwant   %v", handed, want)
	}
	if got := os.Getenv("LENS_PUBLIC_URL"); got != "https://lens.example.com" {
		t.Fatalf("LENS_PUBLIC_URL %q", got)
	}
	// The forwarded flags parse back to the same request on the client side.
	opts, rest, err := parseFlags(handed[1:], &bytes.Buffer{}, true)
	if err != nil || len(rest) != 0 || opts.name != "deal sites" || !opts.ctv || opts.entry != entry || opts.timeout != 30 {
		t.Fatalf("opts %+v %v %v", opts, rest, err)
	}
}

func TestOperatorWithoutSpoolFails(t *testing.T) {
	r, _, errOut := testRunner(t)
	r.Geteuid = func() int { return 0 }
	file := filepath.Join(t.TempDir(), "sites.csv")
	_ = os.WriteFile(file, []byte("Domain\na.com\n"), 0o600)
	if code := r.Main([]string{file}); code != 1 || !strings.Contains(errOut.String(), "run lens update") {
		t.Fatalf("code %d %q", code, errOut)
	}
}

func TestOperatorFailedHandOffRemovesTheEntry(t *testing.T) {
	r, _, _ := testRunner(t)
	spool := filepath.Join(r.AppDir, "managed-files", "spool")
	_ = os.MkdirAll(spool, 0o700)
	r.Geteuid = func() int { return 0 }
	r.Chown = func(string, int, int) error { return nil }
	r.AsServiceUser = func([]string) (bool, int) { return true, 127 }
	file := filepath.Join(t.TempDir(), "sites.csv")
	_ = os.WriteFile(file, []byte("Domain\na.com\n"), 0o600)
	if code := r.Main([]string{file}); code != 127 {
		t.Fatalf("code %d", code)
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Fatalf("spool = %v", entries)
	}
}

func TestOperatorReRunsUnderSudo(t *testing.T) {
	r, _, _ := testRunner(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "sites.csv")
	_ = os.WriteFile(file, []byte("Domain\na.com\n"), 0o600)
	t.Chdir(dir)
	var argv []string
	r.Exec = func(a, _ []string) error {
		argv = a
		return nil
	}
	if code := r.Main([]string{"sites.csv", "--no-research-fallback"}); code != 0 {
		t.Fatalf("code %d", code)
	}
	want := []string{"sudo", "--preserve-env=LENS_APP_DIR,LENS_APP_USER,LENS_HOME", "/opt/lens/bin/lens", "submit", file, "--no-research-fallback", "--timeout=30"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv %v", argv)
	}
}

func TestOperatorAsTheServiceUserRunsInProcess(t *testing.T) {
	r, out, _ := testRunner(t)
	spool := filepath.Join(r.AppDir, "managed-files", "spool")
	_ = os.MkdirAll(spool, 0o700)
	r.Geteuid = func() int { return 990 }
	t.Setenv("LENS_PUBLIC_URL", "")
	r.Sleep = func(time.Duration) {
		entries, _ := os.ReadDir(spool)
		_ = os.WriteFile(filepath.Join(spool, entries[0].Name(), "result.json"), []byte(`{"job_id": "J2", "input_file": "sites.csv"}`), 0o600)
	}
	file := filepath.Join(t.TempDir(), "sites.csv")
	_ = os.WriteFile(file, []byte("Domain\na.com\n"), 0o600)
	if code := r.Main([]string{file}); code != 0 || out.String() != "Queued sites.csv as job J2\n" {
		t.Fatalf("code %d %q", code, out)
	}
}

func TestEnvFileValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	_ = os.WriteFile(path, []byte("# LENS_PUBLIC_URL=no\nLENS_PUBLIC_URL='https://a.example'\r\nOTHER=x\n"), 0o600)
	if v, ok := envFileValue(path, "LENS_PUBLIC_URL"); !ok || v != "https://a.example" {
		t.Fatalf("%q %v", v, ok)
	}
	if _, ok := envFileValue(path, "MISSING"); ok {
		t.Fatal("missing key found")
	}
}
