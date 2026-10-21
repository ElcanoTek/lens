// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package doctor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost is a machine where nothing exists unless a test says so.
type fakeHost struct {
	mu    sync.Mutex
	calls [][]string
	dirs  []string
	run   func(opts RunOptions, argv []string) (int, string)
	look  map[string]bool
	euid  int
	acct  *Account
}

func (f *fakeHost) host(root string) Host {
	return Host{
		Run: func(_ context.Context, opts RunOptions, name string, args ...string) (int, string) {
			argv := append([]string{name}, args...)
			f.mu.Lock()
			f.calls = append(f.calls, argv)
			f.dirs = append(f.dirs, opts.Dir)
			f.mu.Unlock()
			if f.run != nil {
				return f.run(opts, argv)
			}
			return 127, ""
		},
		LookPath: func(name string) bool { return f.look[name] },
		Geteuid:  func() int { return f.euid },
		LookupUser: func(string) (Account, error) {
			if f.acct == nil {
				return Account{}, errors.New("no such user")
			}
			return *f.acct, nil
		},
		DiskFree: func(string) (uint64, error) { return 100 << 30, nil },
		Now:      func() time.Time { return time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC) },
		Root:     root,
	}
}

func byName(checks []Check) map[string]Check {
	out := map[string]Check{}
	for _, c := range checks {
		out[c.Name] = c
	}
	return out
}

func selfAccount() *Account {
	return &Account{UID: os.Getuid(), GID: os.Getgid()}
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestInspectEnvNeverExecutesOrDisclosesValues(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "executed")
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 32))
	env := filepath.Join(dir, ".env")
	writeFile(t, env, "OPENROUTER_API_KEY='$(touch "+marker+")'\nAUTH_SIGNING_PUBKEY="+key+"\nLENS_AUTH_MODE=central\n", 0o600)

	r, err := inspectEnv(env)
	if err != nil {
		t.Fatal(err)
	}
	if !r.KeyOK || r.SessionOK || !r.Central {
		t.Fatalf("report = %+v", r)
	}
	if !contains(r.Missing, "AUTH_CLIENT_SECRET") {
		t.Fatalf("missing = %v", r.Missing)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal(".env was executed")
	}

	// The whole JSON report must carry neither the key nor the payload.
	f := &fakeHost{acct: selfAccount()}
	var out bytes.Buffer
	code := run(context.Background(), []string{"--json"}, &out, &bytes.Buffer{},
		mapEnv(map[string]string{"LENS_APP_DIR": dir, "LENS_SRC_DIR": dir}), f.host(t.TempDir()), Diagnose)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (broken install)", code)
	}
	if strings.Contains(out.String(), key) || strings.Contains(out.String(), "touch") {
		t.Fatalf("report leaks a value:\n%s", out.String())
	}
}

func TestInspectEnvModes(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32))
	cases := []struct {
		name, body          string
		modeOK, session, ok bool
	}{
		{"elcano default", "OPENROUTER_API_KEY=a\nAUTH_SIGNING_PUBKEY=" + key + "\n", true, true, true},
		{"central normalised", "LENS_AUTH_MODE= Central\nOPENROUTER_API_KEY=a\nAUTH_SIGNING_PUBKEY=" + key +
			"\nAUTH_ISSUER_URL=https://a\nLENS_PUBLIC_URL=https://b\nAUTH_CLIENT_ID=lens\nAUTH_CLIENT_SECRET=s\nLENS_SESSION_SECRET=" +
			strings.Repeat("s", 32) + "\n", true, true, true},
		{"bogus mode", "LENS_AUTH_MODE=magic\nOPENROUTER_API_KEY=a\nAUTH_SIGNING_PUBKEY=" + key + "\n", false, true, false},
		{"short key", "OPENROUTER_API_KEY=a\nAUTH_SIGNING_PUBKEY=" + base64.StdEncoding.EncodeToString([]byte("short")) + "\n", true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := filepath.Join(t.TempDir(), ".env")
			writeFile(t, env, tc.body, 0o600)
			r, err := inspectEnv(env)
			if err != nil {
				t.Fatal(err)
			}
			good := len(r.Missing) == 0 && r.KeyOK && r.ModeOK && r.SessionOK
			if r.ModeOK != tc.modeOK || r.SessionOK != tc.session || good != tc.ok {
				t.Fatalf("report = %+v", r)
			}
		})
	}
}

func TestBrokenInstallReportsWithoutCrashingOrRepairs(t *testing.T) {
	f := &fakeHost{}
	dir := t.TempDir()
	checks := byName(Diagnose(context.Background(),
		Options{App: filepath.Join(dir, "missing"), Src: filepath.Join(dir, "src"), User: "lens"}, f.host(dir)))
	for _, name := range []string{"readiness", "configuration", "binary", "release"} {
		if checks[name].Status != StatusFail {
			t.Errorf("%s = %+v, want fail", name, checks[name])
		}
	}
	for _, argv := range f.calls {
		for _, tok := range argv {
			switch tok {
			case "install", "upgrade", "restart", "start", "pull", "fetch":
				t.Fatalf("doctor ran a mutating command: %v", argv)
			}
		}
	}
}

func TestExecRunnerTimeoutIsAFailure(t *testing.T) {
	code, out := ExecRunner(context.Background(), RunOptions{Timeout: 10 * time.Millisecond}, "sleep", "5")
	if code != 127 || out != "" {
		t.Fatalf("got (%d, %q), want (127, \"\")", code, out)
	}
	code, out = ExecRunner(context.Background(), RunOptions{}, "sh", "-c", "echo hi; exit 3")
	if code != 3 || out != "hi" {
		t.Fatalf("got (%d, %q), want (3, \"hi\")", code, out)
	}
	if code, _ := ExecRunner(context.Background(), RunOptions{}, "/nonexistent/binary"); code != 127 {
		t.Fatalf("missing binary code = %d", code)
	}
}

func TestPodmanProbeUsesServiceHomeNotRootCwd(t *testing.T) {
	app := t.TempDir()
	f := &fakeHost{euid: 0, acct: selfAccount(), look: map[string]bool{"podman": true}}
	f.run = func(opts RunOptions, argv []string) (int, string) {
		if contains(argv, "podman") && opts.Dir == app {
			return 0, ""
		}
		if contains(argv, "podman") {
			return 125, ""
		}
		return 127, ""
	}
	checks := byName(Diagnose(context.Background(), Options{App: app, Src: app, User: "lens"}, f.host(t.TempDir())))
	if checks["podman"].Status != StatusOK {
		t.Fatalf("podman = %+v", checks["podman"])
	}
	var probe []string
	for _, argv := range f.calls {
		if contains(argv, "podman") {
			probe = argv
			break
		}
	}
	if strings.Join(probe[:4], " ") != "runuser -u lens --" {
		t.Fatalf("probe = %v", probe)
	}
	// The image probe runs only once podman works, and the same way.
	if checks["chrome-image"].Status != StatusOK || !strings.Contains(checks["chrome-image"].Detail, "standalone-chrome") {
		t.Fatalf("chrome-image = %+v", checks["chrome-image"])
	}
}

func TestPodmanRootlessPrerequisites(t *testing.T) {
	root := t.TempDir()
	app := t.TempDir()
	f := &fakeHost{euid: 1000, acct: selfAccount(), look: map[string]bool{"podman": true}}
	checks := byName(Diagnose(context.Background(), Options{App: app, Src: app, User: "lens"}, f.host(root)))
	if c := checks["podman-rootless"]; c.Status != StatusWarn || !strings.Contains(c.Detail, "/etc/subuid") {
		t.Fatalf("podman-rootless = %+v", c)
	}
	if _, ok := checks["chrome-image"]; ok {
		t.Fatal("chrome-image probed although podman does not work")
	}
	writeFile(t, filepath.Join(root, "etc/subuid"), "lens:200000:65536\n", 0o644)
	writeFile(t, filepath.Join(root, "etc/subgid"), "lens:200000:65536\n", 0o644)
	writeFile(t, filepath.Join(root, "var/lib/systemd/linger/lens"), "", 0o644)
	checks = byName(Diagnose(context.Background(), Options{App: app, Src: app, User: "lens"}, f.host(root)))
	if c := checks["podman-rootless"]; c.Status != StatusOK {
		t.Fatalf("podman-rootless = %+v", c)
	}
}

func TestJSONExitContract(t *testing.T) {
	diag := func(context.Context, Options, Host) []Check {
		return []Check{{Name: "podman", Status: StatusWarn, Detail: "optional runtime missing"}}
	}
	for _, tc := range []struct {
		strict bool
		want   int
	}{{false, 0}, {true, 1}} {
		args := []string{"--json"}
		if tc.strict {
			args = append(args, "--strict")
		}
		var out bytes.Buffer
		if got := run(context.Background(), args, &out, &bytes.Buffer{}, mapEnv(nil), Host{}, diag); got != tc.want {
			t.Fatalf("strict=%v exit = %d, want %d", tc.strict, got, tc.want)
		}
		var report struct {
			OK     bool    `json:"ok"`
			Checks []Check `json:"checks"`
		}
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.OK != !tc.strict || report.Checks[0].Status != StatusWarn {
			t.Fatalf("report = %+v", report)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	diag := func(context.Context, Options, Host) []Check { return nil }
	for _, args := range [][]string{{"--public-url", "http://lens.example.com"}, {"--bogus"}, {"extra"}} {
		if got := run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}, mapEnv(nil), Host{}, diag); got != 2 {
			t.Fatalf("%v exit = %d, want 2", args, got)
		}
	}
	var out bytes.Buffer
	if got := run(context.Background(), []string{"--check"}, &out, &bytes.Buffer{}, mapEnv(nil), Host{}, diag); got != 0 {
		t.Fatalf("--check exit = %d", got)
	}
	if !strings.Contains(out.String(), "Doctor changes nothing.") {
		t.Fatalf("text report = %q", out.String())
	}
}

func TestLoginRedirectIsNotReadiness(t *testing.T) {
	f := &fakeHost{}
	f.run = func(_ RunOptions, argv []string) (int, string) {
		if argv[0] == "curl" {
			return 0, "302"
		}
		return 127, ""
	}
	dir := t.TempDir()
	checks := byName(Diagnose(context.Background(),
		Options{App: dir, Src: dir, User: "lens", PublicURL: "https://lens.example.com"}, f.host(dir)))
	for _, name := range []string{"readiness", "public-tls"} {
		if checks[name].Status != StatusFail {
			t.Errorf("%s = %+v, want fail", name, checks[name])
		}
	}
}

func TestServiceUserCanRead(t *testing.T) {
	lensAcct := Account{UID: 987, GID: 987}
	cases := []struct {
		name string
		fi   FileInfo
		acct Account
		want bool
	}{
		{"root-owned private", FileInfo{Mode: 0o600, UID: 0, GID: 0}, lensAcct, false},
		{"owned private", FileInfo{Mode: 0o600, UID: 987, GID: 987}, lensAcct, true},
		{"root owner, lens group 0640", FileInfo{Mode: 0o640, UID: 0, GID: 987}, lensAcct, true},
		{"owner without read bit", FileInfo{Mode: 0o044, UID: 987, GID: 987}, lensAcct, false},
		{"supplementary group", FileInfo{Mode: 0o640, UID: 0, GID: 50}, Account{UID: 987, GID: 987, Groups: []int{50}}, true},
		{"world readable", FileInfo{Mode: 0o604, UID: 0, GID: 0}, lensAcct, true},
	}
	for _, tc := range cases {
		if got := ServiceUserCanRead(tc.fi, tc.acct); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFlagsEnvTheServiceUserCannotRead(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	writeFile(t, env, "OPENROUTER_API_KEY=x\n", 0o600)
	f := &fakeHost{acct: &Account{UID: os.Getuid() + 1, GID: os.Getgid() + 1}}
	checks := byName(Diagnose(context.Background(), Options{App: dir, Src: dir, User: "lens"}, f.host(dir)))
	if c := checks["env-readable"]; c.Status != StatusFail || !strings.Contains(c.Detail, "chown lens:lens "+env) {
		t.Fatalf("env-readable = %+v", c)
	}
	if checks["env-permissions"].Status != StatusOK {
		t.Fatalf("env-permissions = %+v", checks["env-permissions"])
	}
}

func TestSSHDPasswordWarning(t *testing.T) {
	cases := []struct {
		effective, status string
	}{
		{"passwordauthentication no\npermitrootlogin prohibit-password", StatusOK},
		{"passwordauthentication yes\npermitrootlogin prohibit-password", StatusWarn},
		{"passwordauthentication no\npermitrootlogin yes", StatusWarn},
	}
	for _, tc := range cases {
		f := &fakeHost{euid: 0, look: map[string]bool{"sshd": true}}
		f.run = func(_ RunOptions, argv []string) (int, string) {
			if argv[0] == "sshd" {
				return 0, tc.effective
			}
			return 127, ""
		}
		dir := t.TempDir()
		c := byName(Diagnose(context.Background(), Options{App: dir, Src: dir, User: "lens"}, f.host(dir)))["sshd-passwords"]
		if c.Status != tc.status {
			t.Fatalf("%q: %+v", tc.effective, c)
		}
		if tc.status == StatusWarn && !strings.Contains(c.Detail, "production-support README, Access: SSH keys") {
			t.Fatalf("remedy = %q", c.Detail)
		}
	}
}

func TestSSHDCheckSkippedWhenNotRoot(t *testing.T) {
	f := &fakeHost{euid: 1000, look: map[string]bool{"sshd": true}}
	f.run = func(RunOptions, []string) (int, string) { return 0, "passwordauthentication yes" }
	dir := t.TempDir()
	if _, ok := byName(Diagnose(context.Background(), Options{App: dir, Src: dir, User: "lens"}, f.host(dir)))["sshd-passwords"]; ok {
		t.Fatal("sshd-passwords reported without root")
	}
}

// goRelease lays out a deployed Go release under app.
func goRelease(t *testing.T, app, rev string) {
	t.Helper()
	writeFile(t, filepath.Join(app, "bin/lens"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(app, ".deployed-revision"), rev+"\n", 0o644)
}

func TestBinaryMatchesDeployedRevision(t *testing.T) {
	rev := strings.Repeat("ab", 20)
	for _, tc := range []struct {
		name, version, status string
	}{
		{"match", "lens v1 (" + rev + ")", StatusOK},
		{"stale", "lens v0 (" + strings.Repeat("cd", 20) + ")", StatusFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := t.TempDir()
			goRelease(t, app, rev)
			f := &fakeHost{}
			f.run = func(_ RunOptions, argv []string) (int, string) {
				if argv[0] == filepath.Join(app, "bin/lens") && argv[1] == "--version" {
					return 0, tc.version
				}
				if argv[0] == "git" && contains(argv, "rev-parse") {
					return 0, rev
				}
				return 127, ""
			}
			checks := byName(Diagnose(context.Background(), Options{App: app, Src: app, User: "lens"}, f.host(t.TempDir())))
			if checks["binary"].Status != tc.status {
				t.Fatalf("binary = %+v", checks["binary"])
			}
			if checks["release"].Status != StatusOK {
				t.Fatalf("release = %+v", checks["release"])
			}
		})
	}
}

func TestRunningBinaryAndAssets(t *testing.T) {
	app := t.TempDir()
	root := t.TempDir()
	goRelease(t, app, "rev")
	if err := os.MkdirAll(filepath.Join(root, "proc/42"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(app, "bin/lens"), filepath.Join(root, "proc/42/exe")); err != nil {
		t.Fatal(err)
	}
	f := &fakeHost{}
	f.run = func(_ RunOptions, argv []string) (int, string) {
		if argv[0] == "systemctl" && contains(argv, "--property=MainPID") {
			return 0, "42"
		}
		return 127, ""
	}
	checks := byName(Diagnose(context.Background(), Options{App: app, Src: app, User: "lens"}, f.host(root)))
	if checks["running-binary"].Status != StatusOK {
		t.Fatalf("running-binary = %+v", checks["running-binary"])
	}
	if checks["assets"].Status != StatusOK {
		t.Fatalf("assets = %+v", checks["assets"])
	}
}

func TestPythonVenvLeftoverIsAdvisory(t *testing.T) {
	app := t.TempDir()
	f := &fakeHost{}
	if c := byName(Diagnose(context.Background(), Options{App: app, Src: app, User: "lens"}, f.host(app)))["python-venv"]; c.Status != StatusOK {
		t.Fatalf("python-venv = %+v", c)
	}
	if err := os.MkdirAll(filepath.Join(app, ".venv/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := byName(Diagnose(context.Background(), Options{App: app, Src: app, User: "lens"}, f.host(app)))["python-venv"]
	if c.Status != StatusWarn || !strings.Contains(c.Detail, ".venv") {
		t.Fatalf("python-venv = %+v", c)
	}
}

func TestAuthDatabases(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32))
	app := t.TempDir()
	db := filepath.Join(t.TempDir(), "access.db")
	writeFile(t, filepath.Join(app, ".env"), "LENS_AUTH_MODE=central\nOPENROUTER_API_KEY=a\nAUTH_SIGNING_PUBKEY="+key+
		"\nLENS_ACCESS_DB="+db+"\n", 0o600)
	f := &fakeHost{acct: selfAccount()}
	diag := func() map[string]Check {
		return byName(Diagnose(context.Background(), Options{App: app, Src: app, User: "lens"}, f.host(t.TempDir())))
	}
	c := diag()
	if c["access-db"].Status != StatusFail || !strings.Contains(c["access-db"].Detail, db) {
		t.Fatalf("access-db = %+v", c["access-db"])
	}
	if c["token-db"].Status != StatusOK {
		t.Fatalf("token-db = %+v", c["token-db"])
	}
	writeFile(t, db, "", 0o644)
	if c := diag()["access-db"]; c.Status != StatusWarn {
		t.Fatalf("world-readable access-db = %+v", c)
	}
	writeFile(t, db, "", 0o600)
	if c := diag()["access-db"]; c.Status != StatusOK {
		t.Fatalf("private access-db = %+v", c)
	}
	f.acct = &Account{UID: os.Getuid() + 1}
	if c := diag()["access-db"]; c.Status != StatusFail || !strings.Contains(c.Detail, "chown lens:lens "+db) {
		t.Fatalf("foreign access-db = %+v", c)
	}
}

func TestManagedFilesAndJobs(t *testing.T) {
	app := t.TempDir()
	f := &fakeHost{acct: selfAccount()}
	diag := func() map[string]Check {
		return byName(Diagnose(context.Background(), Options{App: app, Src: app, User: "lens"}, f.host(t.TempDir())))
	}
	c := diag()
	if c["managed-files"].Status != StatusWarn || c["jobs"].Status != StatusOK {
		t.Fatalf("empty box: managed-files=%+v jobs=%+v", c["managed-files"], c["jobs"])
	}
	for _, sub := range []string{"inputs", "outputs", "spool"} {
		if err := os.MkdirAll(filepath.Join(app, "managed-files", sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(app, "managed-files/outputs/_jobs.json"),
		`{"saved_at":"x","jobs":[{"status":"running"},{"status":"queued"},{"status":"queued"},{"status":"completed"}]}`, 0o644)
	c = diag()
	if c["managed-files"].Status != StatusOK {
		t.Fatalf("managed-files = %+v", c["managed-files"])
	}
	if j := c["jobs"]; j.Status != StatusOK || !strings.Contains(j.Detail, "1 running, 2 queued") {
		t.Fatalf("jobs = %+v", j)
	}
	writeFile(t, filepath.Join(app, "managed-files/outputs/_jobs.json"), `{not json`, 0o644)
	if j := diag()["jobs"]; j.Status != StatusWarn {
		t.Fatalf("corrupt jobs = %+v", j)
	}
	f.acct = &Account{UID: os.Getuid() + 1}
	if m := diag()["managed-files"]; m.Status != StatusFail || !strings.Contains(m.Detail, "chown -R lens:lens") {
		t.Fatalf("foreign managed-files = %+v", m)
	}
}

func TestOSSupportAndFirecrawlAndCockpit(t *testing.T) {
	root := t.TempDir()
	app := t.TempDir()
	writeFile(t, filepath.Join(root, "etc/os-release"), "NAME=\"Fedora Linux\"\nSUPPORT_END=2026-11-01\n", 0o644)
	f := &fakeHost{look: map[string]bool{"rpm": true}}
	f.run = func(_ RunOptions, argv []string) (int, string) {
		switch {
		case argv[0] == "systemctl" && contains(argv, "firecrawl.service"):
			return 0, "loaded"
		case argv[0] == "rpm":
			return 0, "cockpit-ws-300"
		}
		return 127, ""
	}
	c := byName(Diagnose(context.Background(), Options{App: app, Src: app, User: "lens"}, f.host(root)))
	if c["os-support"].Status != StatusWarn || !strings.Contains(c["os-support"].Detail, "2026-11-01") {
		t.Fatalf("os-support = %+v", c["os-support"])
	}
	if c["firecrawl"].Status != StatusWarn {
		t.Fatalf("firecrawl = %+v", c["firecrawl"])
	}
	if c["cockpit"].Status != StatusWarn {
		t.Fatalf("cockpit = %+v", c["cockpit"])
	}
	writeFile(t, filepath.Join(root, "etc/os-release"), "SUPPORT_END=2027-06-01\n", 0o644)
	if c := byName(Diagnose(context.Background(), Options{App: app, Src: app, User: "lens"}, f.host(root)))["os-support"]; c.Status != StatusOK {
		t.Fatalf("os-support = %+v", c)
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func mapEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}
