// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package deploytest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestBootstrapOptionalSecretPrompt(t *testing.T) {
	src := read(t, "scripts/bootstrap.sh")
	fn := between(t, src, "prompt_secret() {", "genbase64()")
	cases := []struct {
		nonInteractive, key, optional, input string
		success                              bool
		want                                 string
	}{
		{"1", "", "optional", "", true, ""},
		{"1", "preset-secret", "optional", "", true, "preset-secret"},
		{"0", "", "optional", "\n", true, ""},
		{"0", "", "optional", "typed-secret\n", true, "typed-secret"},
		{"1", "", "", "", false, ""},
		{"0", "", "", "\n", false, ""},
	}
	for _, tc := range cases {
		r := bash(t, `die() { echo "$*" >&2; exit 1; }; ask() { echo "$*" >&2; }; `+fn+
			"\nNON_INTERACTIVE=\"$1\"; TEST_KEY=\"$2\"; prompt_secret TEST_KEY \"Test key\" \"$3\"",
			[]string{tc.nonInteractive, tc.key, tc.optional}, stdin(tc.input))
		if (r.code == 0) != tc.success {
			t.Fatalf("%+v: code %d stderr %s", tc, r.code, r.stderr)
		}
		if tc.success && r.stdout != tc.want {
			t.Fatalf("%+v: stdout %q", tc, r.stdout)
		}
		if strings.Contains(r.stderr, "preset-secret") || strings.Contains(r.stderr, "typed-secret") {
			t.Fatalf("%+v: secret echoed to stderr", tc)
		}
	}
}

// runuser keeps the cwd; from root's unreadable home, podman fails.
func TestServiceUserCommandsNeverRunFromTheCallersCwd(t *testing.T) {
	for _, script := range []string{"scripts/bootstrap.sh", "scripts/update.sh"} {
		src := read(t, script)
		cd, ru := strings.Index(src, `cd "$SRC_DIR"`), strings.Index(src, "runuser -u")
		if cd < 0 || ru < 0 || cd > ru {
			t.Errorf("%s: cd \"$SRC_DIR\" (%d) must precede the first runuser (%d)", script, cd, ru)
		}
	}
}

// /usr/local/bin/lens links to bin/lens and root runs it for every operator
// command, so bin/ must never belong to the service account (a compromised
// service could otherwise rewrite the binary root runs next).
func TestReleaseBinaryIsNeverOwnedByTheServiceUser(t *testing.T) {
	for _, script := range []string{"scripts/bootstrap.sh", "scripts/update.sh"} {
		src := read(t, script)
		for _, line := range strings.Split(src, "\n") {
			if strings.Contains(line, "chown") && strings.Contains(line, "/bin") && strings.Contains(line, "APP_USER") {
				t.Errorf("%s hands bin/ to the service user: %s", script, strings.TrimSpace(line))
			}
		}
		if !strings.Contains(src, `install -d -o root -g root -m 0755 "$PRIV_DIR/bin"`) && !strings.Contains(src, `chown -R "$BIN_OWNER" "$APP_DIR/bin"`) {
			t.Errorf("%s does not give bin/ to root", script)
		}
	}
	if !strings.Contains(read(t, "scripts/update.sh"), `BIN_OWNER="${LENS_BIN_OWNER:-root:root}"`) {
		t.Error("update.sh's bin/ owner no longer defaults to root:root")
	}
}

// Bootstrap must never compile in, or copy root's binary out of, the
// service-owned $APP_DIR: a bootstrap re-run happens with the service up.
func TestBootstrapBuildsOutsideTheServiceTree(t *testing.T) {
	src := read(t, "scripts/bootstrap.sh")
	for _, want := range []string{
		`lens_build "$PRIV_DIR/tree"`,
		`install -o root -g root -m 0755 "$PRIV_DIR/tree/bin/lens" "$PRIV_DIR/lens"`,
		`mv -T "$PRIV_DIR/bin" "$APP_DIR/bin"`,
		`LENS_BIN="$PRIV_DIR/lens"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("bootstrap.sh lost %q", want)
		}
	}
	if strings.Contains(src, `lens_build "$APP_DIR"`) || strings.Contains(src, `"$APP_DIR/bin/lens" "$PRIV_DIR`) ||
		strings.Contains(src, `"$APP_DIR/deploy/`) {
		t.Error("bootstrap.sh builds in, or copies root's binary from, the service-owned tree")
	}
	if build, chown := strings.Index(src, `lens_build "$PRIV_DIR/tree"`), strings.Index(src, `chown -R "$APP_USER:$APP_USER" "$APP_DIR"`+"\n# bin/"); build < 0 || chown < build {
		t.Error("bootstrap.sh hands $APP_DIR to the service user before building root's binary")
	}
}

// tree populates dir with files (name → content).
func tree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		write(t, filepath.Join(dir, name), body, 0o644)
	}
}

var instanceState = []string{
	".env", ".env.shared", "config.json", "managed-files/results.csv", "data/access.db",
	".local/podman", ".config/containers", "progress.json",
}

// snapshot is update.sh's step 3 before the stop: code, venv when present,
// unit, and the CLI as it is.
const snapshot = `
source "$1"; live="$2"; backup="$3"; unit="$4"; cli="$5"
rsync -a "${LENS_STATE_EXCLUDES[@]}" "$live/" "$backup/"
if [[ -d "$live/.venv" ]]; then cp -a "$live/.venv" "$backup/.venv"; fi
install -m 0644 "$unit" "$backup/lens.service.previous"
if [[ -e "$cli" || -L "$cli" ]]; then cp -a "$cli" "$backup/lens-cli.previous"; fi
`

// swapAndRestore is update.sh's swap followed by a failed health check.
const swapAndRestore = `
source "$1"; live="$2"; stage="$3"; backup="$4"; unit="$5"; cli="$6"
export LENS_BIN_OWNER="$(id -un):$(id -gn)"
rsync -a --delete "${LENS_STATE_EXCLUDES[@]}" "$stage/" "$live/"
echo 'new unit' > "$unit"
lens_install_cli "$live/bin/lens" "$cli" "$LENS_BIN_OWNER"
lens_restore_release "$live" "$backup" "$unit" "$cli"
`

func setupState(t *testing.T, live, stage string) {
	t.Helper()
	for _, name := range instanceState {
		write(t, filepath.Join(live, name), "operator-owned", 0o644)
		write(t, filepath.Join(stage, name), "must not overwrite", 0o644)
	}
}

func checkState(t *testing.T, live, backup string) {
	t.Helper()
	for _, name := range instanceState {
		if got := readFile(t, filepath.Join(live, name)); got != "operator-owned" {
			t.Errorf("%s = %q after restore", name, got)
		}
		if exists(filepath.Join(backup, name)) {
			t.Errorf("snapshot captured instance state %s", name)
		}
	}
}

// A failed FIRST migration must bring the Python release back: its code,
// its venv (which the swap never deleted), its unit and its bash CLI.
func TestFailedFirstGoMigrationRestoresThePythonRelease(t *testing.T) {
	requireTools(t, "rsync")
	dir := t.TempDir()
	live, stage, backup := filepath.Join(dir, "live"), filepath.Join(dir, "stage"), filepath.Join(dir, "backup")
	unit, cli := filepath.Join(dir, "unit"), filepath.Join(dir, "cli")
	setupState(t, live, stage)
	tree(t, live, map[string]string{"web_service.py": "old", ".venv/bin/python": "old", "deploy/lens-cli": "old cli"})
	tree(t, stage, map[string]string{"bin/lens": "new", "deploy/lens.service": "new", "go.mod": "new"})
	write(t, unit, "old unit", 0o644)
	write(t, cli, "old cli", 0o755)
	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	if r := bash(t, snapshot, []string{lib(t), live, backup, unit, cli}); r.code != 0 {
		t.Fatal(r.stderr)
	}
	if r := bash(t, swapAndRestore, []string{lib(t), live, stage, backup, unit, cli}); r.code != 0 {
		t.Fatal(r.stderr)
	}
	if readFile(t, filepath.Join(live, "web_service.py")) != "old" || readFile(t, filepath.Join(live, ".venv/bin/python")) != "old" {
		t.Fatal("Python code or venv not restored")
	}
	if exists(filepath.Join(live, "bin")) || exists(filepath.Join(live, "go.mod")) {
		t.Fatal("Go release files survived the restore")
	}
	if readFile(t, unit) != "old unit" {
		t.Fatal("unit not restored")
	}
	if fi, err := os.Lstat(cli); err != nil || fi.Mode()&os.ModeSymlink != 0 || readFile(t, cli) != "old cli" {
		t.Fatalf("CLI not restored to the bash script: %v %v", fi, err)
	}
	checkState(t, live, backup)
}

func TestFailedGoUpdateRestoresThePreviousGoRelease(t *testing.T) {
	requireTools(t, "rsync")
	dir := t.TempDir()
	live, stage, backup := filepath.Join(dir, "live"), filepath.Join(dir, "stage"), filepath.Join(dir, "backup")
	unit, cli := filepath.Join(dir, "unit"), filepath.Join(dir, "cli")
	setupState(t, live, stage)
	tree(t, live, map[string]string{"bin/lens": "v1", "old-only.txt": "v1"})
	tree(t, stage, map[string]string{"bin/lens": "v2", "new-only.txt": "v2"})
	write(t, unit, "old unit", 0o644)
	if err := os.Symlink(filepath.Join(live, "bin/lens"), cli); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	if r := bash(t, snapshot, []string{lib(t), live, backup, unit, cli}); r.code != 0 {
		t.Fatal(r.stderr)
	}
	if r := bash(t, swapAndRestore, []string{lib(t), live, stage, backup, unit, cli}); r.code != 0 {
		t.Fatal(r.stderr)
	}
	if readFile(t, filepath.Join(live, "bin/lens")) != "v1" || exists(filepath.Join(live, "new-only.txt")) ||
		!exists(filepath.Join(live, "old-only.txt")) || exists(filepath.Join(live, ".venv")) {
		t.Fatal("previous Go release not restored exactly")
	}
	assertCLICopy(t, cli, filepath.Join(live, "bin/lens"))
	if readFile(t, cli) != "v1" {
		t.Fatal("CLI is not the restored binary")
	}
	checkState(t, live, backup)
}

// Snapshots written by the Python-era update.sh have no lens-cli.previous;
// the restored tree's deploy/lens-cli is the CLI then.
func TestRestoreOfALegacySnapshotInstallsItsBashCLI(t *testing.T) {
	requireTools(t, "rsync")
	dir := t.TempDir()
	live, backup := filepath.Join(dir, "live"), filepath.Join(dir, "backup")
	unit, cli := filepath.Join(dir, "unit"), filepath.Join(dir, "cli")
	tree(t, live, map[string]string{"bin/lens": "new", ".venv/python": "stale", ".env": "operator-owned"})
	tree(t, backup, map[string]string{"app.py": "old", ".venv/python": "old", "deploy/lens-cli": "old",
		"lens.service.previous": "old unit"})
	if err := os.Symlink(filepath.Join(live, "bin/lens"), cli); err != nil {
		t.Fatal(err)
	}
	r := bash(t, `source "$1"; export LENS_BIN_OWNER="$(id -un):$(id -gn)"; lens_restore_release "$2" "$3" "$4" "$5"`, []string{lib(t), live, backup, unit, cli})
	if r.code != 0 {
		t.Fatal(r.stderr)
	}
	if readFile(t, cli) != "old" || readFile(t, filepath.Join(live, ".venv/python")) != "old" || readFile(t, unit) != "old unit" {
		t.Fatal("legacy snapshot not restored")
	}
	if fi, _ := os.Lstat(cli); fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm() != 0o755 {
		t.Fatalf("CLI mode = %v", fi.Mode())
	}
	if exists(filepath.Join(live, "bin")) || readFile(t, filepath.Join(live, ".env")) != "operator-owned" {
		t.Fatal("restore touched the wrong files")
	}
}

func TestLensBuildUsesRootOwnedCachesAndStampsTheRevision(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	log := filepath.Join(dir, "make.log")
	stub(t, bin, "make", `{
  printf 'args=%s\n' "$*"
  for v in GOTOOLCHAIN GOENV GOPATH GOMODCACHE GOCACHE HOME; do eval "printf '%s=%s\n' $v \"\$$v\""; done
} > "`+log+`"
[ "${STUB_FAIL:-0}" = 1 ] && exit 2
mkdir -p "$2/bin" && printf '#!/bin/sh\n' > "$2/bin/lens" && chmod 755 "$2/bin/lens"`)
	stage := filepath.Join(dir, "stage")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache")
	script := `source "$1"; lens_build "$2" 0123abc v1.2.3`
	r := bash(t, script, []string{lib(t), stage}, extend("PATH="+bin+":"+os.Getenv("PATH"), "LENS_BUILD_CACHE="+cache, "GOTOOLCHAIN="))
	if r.code != 0 {
		t.Fatalf("lens_build: %s", r.stderr)
	}
	got := readFile(t, log)
	for _, want := range []string{
		"args=-C " + stage + " --no-print-directory build COMMIT=0123abc VERSION=v1.2.3",
		"GOTOOLCHAIN=auto", "GOENV=off", "GOPATH=" + cache + "/gopath", "GOMODCACHE=" + cache + "/gomod",
		"GOCACHE=" + cache + "/gocache", "HOME=" + cache,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("make saw:\n%s\nmissing %q", got, want)
		}
	}
	if fi, err := os.Stat(cache); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir: %v %v", fi, err)
	}
	r = bash(t, script, []string{lib(t), filepath.Join(dir, "stage2")},
		extend("PATH="+bin+":"+os.Getenv("PATH"), "LENS_BUILD_CACHE="+cache, "STUB_FAIL=1"))
	if r.code == 0 {
		t.Fatal("a failed make must fail lens_build")
	}
}

func TestFedoraLatestExcludesBetaRawhideAndFailsClosed(t *testing.T) {
	requireTools(t, "jq")
	feed, _ := json.Marshal([]map[string]string{{"version": "43"}, {"version": "44"}, {"version": "45 Beta"}, {"version": "rawhide"}})
	r := bash(t, `source "$1"; lens_latest_fedora`, []string{lib(t)}, stdin(string(feed)))
	if r.code != 0 || strings.TrimSpace(r.stdout) != "44" {
		t.Fatalf("got %d %q %s", r.code, r.stdout, r.stderr)
	}
	r = bash(t, `source "$1"; lens_latest_fedora`, []string{lib(t)}, stdin(`[{"version":"45 Beta"}]`))
	if r.code == 0 {
		t.Fatal("no stable release must fail")
	}
}

func TestHostDryRunNeverInvokesPackageManager(t *testing.T) {
	bin := t.TempDir()
	stub(t, bin, "dnf", "echo MUTATED >&2\nexit 99")
	for _, cmd := range []string{"update", "tools"} {
		r := runCmd(t, []string{bashPath(t), filepath.Join(repo(t), "scripts/host.sh"), cmd, "--dry-run", "--yes"},
			extend("PATH="+bin+":"+os.Getenv("PATH")))
		if r.code != 0 || strings.Contains(r.stderr, "MUTATED") || !strings.Contains(r.stdout, "dnf --refresh") {
			t.Fatalf("%s: %+v", cmd, r)
		}
	}
}

func TestHostCheckDistinguishesUpdatesFromDNFFailure(t *testing.T) {
	for code, want := range map[int]int{0: 0, 100: 0, 1: 1} {
		bin := t.TempDir()
		stub(t, bin, "dnf", fmt.Sprintf("exit %d", code))
		r := runCmd(t, []string{bashPath(t), filepath.Join(repo(t), "scripts/host.sh"), "check"},
			extend("PATH="+bin+":"+os.Getenv("PATH")))
		if r.code != want {
			t.Fatalf("dnf exit %d: host check exit %d", code, r.code)
		}
	}
}

func TestDeployGitNeedsNoHomeOrGlobalTrustConfig(t *testing.T) {
	requireTools(t, "git")
	src := filepath.Join(t.TempDir(), "source")
	if r := runCmd(t, []string{"git", "init", "-q", src}); r.code != 0 {
		t.Fatal(r.stderr)
	}
	var base []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			base = append(base, kv)
		}
	}
	base = append(base, "GIT_TEST_ASSUME_DIFFERENT_OWNER=1", "GIT_CONFIG_NOSYSTEM=1")
	if r := runCmd(t, []string{"git", "-C", src, "status", "--porcelain"}, env(base...)); r.code == 0 {
		t.Skip("this git does not honour GIT_TEST_ASSUME_DIFFERENT_OWNER")
	}
	if r := bash(t, `source "$1"; lens_git "$2" status --porcelain`, []string{lib(t), src}, env(base...)); r.code != 0 {
		t.Fatal(r.stderr)
	}
	if exists(filepath.Join(src, ".gitconfig")) {
		t.Fatal("lens_git wrote a config file")
	}
}

func TestPruneRollbacksKeepsTheNewest(t *testing.T) {
	for keep, survivors := range map[string]int{"": 2, "1": 1, "3": 3, "junk": 2, "0": 2} {
		dir := t.TempDir()
		app := filepath.Join(dir, "lens")
		if err := os.MkdirAll(app, 0o755); err != nil {
			t.Fatal(err)
		}
		snaps := make([]string, 0, 5)
		for n := range 5 {
			snap := filepath.Join(dir, fmt.Sprintf("lens.rollback.%06d", n))
			if err := os.MkdirAll(filepath.Join(snap, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			ts := time.Unix(int64(1_000_000+n), 0)
			if err := os.Chtimes(snap, ts, ts); err != nil {
				t.Fatal(err)
			}
			snaps = append(snaps, filepath.Base(snap))
		}
		if err := os.MkdirAll(filepath.Join(dir, "lens-other"), 0o755); err != nil {
			t.Fatal(err)
		}
		r := bash(t, `. "$1"; lens_prune_rollbacks "$2" "$3"`, []string{lib(t), app, keep})
		if r.code != 0 {
			t.Fatal(r.stderr)
		}
		left, _ := filepath.Glob(filepath.Join(dir, "lens.rollback.*"))
		names := make([]string, 0, len(left))
		for _, l := range left {
			names = append(names, filepath.Base(l))
		}
		sort.Strings(names)
		if strings.Join(names, ",") != strings.Join(snaps[len(snaps)-survivors:], ",") {
			t.Fatalf("keep=%q: left %v", keep, names)
		}
		if !exists(app) || !exists(filepath.Join(dir, "lens-other")) {
			t.Fatal("pruned something that is not a snapshot")
		}
	}
	r := bash(t, `. "$1"; lens_prune_rollbacks "$2" 2`, []string{lib(t), filepath.Join(t.TempDir(), "lens")})
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("empty prune: %+v", r)
	}
}

// cockpitUninstall runs lens_cockpit_uninstall with stub rpm/dnf.
func cockpitUninstall(t *testing.T, withRPM bool, kv ...string) (out, calls string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	log := filepath.Join(dir, "calls.log")
	if withRPM {
		stub(t, bin, "rpm", `echo "rpm $*" >> "$STUB_LOG"; [ "${STUB_INSTALLED:-0}" = 1 ]`)
	}
	stub(t, bin, "dnf", `echo "dnf $*" >> "$STUB_LOG"; [ "${STUB_FAIL:-0}" != 1 ]`)
	r := runCmd(t, []string{bashPath(t), "-c", `set -euo pipefail; . "$1"; info() { echo "INFO $*"; }; ok() { echo "OK $*"; }; ` +
		`warn() { echo "WARN $*"; }; lens_cockpit_uninstall; echo "RC=$?"`, "test", lib(t)},
		env(append([]string{"PATH=" + bin, "STUB_LOG=" + log}, kv...)...))
	if r.code != 0 || !strings.Contains(r.stdout, "RC=0") {
		t.Fatalf("%+v", r)
	}
	if exists(log) {
		calls = readFile(t, log)
	}
	return r.stdout, calls
}

func TestCockpitUninstall(t *testing.T) {
	out, calls := cockpitUninstall(t, true, "STUB_INSTALLED=1")
	if !strings.Contains(calls, "dnf -y remove cockpit*") || !strings.Contains(out, "OK removed Cockpit") {
		t.Fatalf("out=%q calls=%q", out, calls)
	}
	for _, tc := range []struct {
		rpm bool
		kv  []string
	}{
		{true, nil},
		{false, []string{"STUB_INSTALLED=1"}},
		{true, []string{"STUB_INSTALLED=1", "LENS_KEEP_COCKPIT=1"}},
		{true, []string{"STUB_INSTALLED=1", "DRY_RUN=1"}},
	} {
		out, calls := cockpitUninstall(t, tc.rpm, tc.kv...)
		if strings.Contains(calls, "dnf") {
			t.Fatalf("%+v: dnf ran: %s", tc, out)
		}
		if strings.Contains(strings.Join(tc.kv, " "), "DRY_RUN") && !strings.Contains(out, "[dry-run] dnf -y remove 'cockpit*'") {
			t.Fatalf("dry run output %q", out)
		}
	}
	out, _ = cockpitUninstall(t, true, "STUB_INSTALLED=1", "STUB_FAIL=1")
	if !strings.Contains(out, "WARN could not remove Cockpit") {
		t.Fatalf("out = %q", out)
	}
}

func TestBootstrapOnlyHostMutations(t *testing.T) {
	boot, update := read(t, "scripts/bootstrap.sh"), read(t, "scripts/update.sh")
	for _, fn := range []string{"\nlens_cockpit_uninstall\n", "\nlens_ssh_key_only\n"} {
		if !strings.Contains(boot, fn) {
			t.Errorf("bootstrap no longer calls %q", strings.TrimSpace(fn))
		}
	}
	if !strings.Contains(boot, "lens_set_hostname") {
		t.Error("bootstrap no longer names the box")
	}
	for _, forbidden := range []string{"set_hostname", "cockpit", "ssh_key_only"} {
		if strings.Contains(update, forbidden) {
			t.Errorf("update.sh mentions %q; only bootstrap may", forbidden)
		}
	}
}

const keyOnlyConf = "# Key-only SSH: no passwords, root by key only. Sorts before cloud-init's\n" +
	"# 50-cloud-init.conf so it wins (sshd keeps the first value it reads).\n" +
	"PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin prohibit-password\n"

type sshBox struct {
	root, bin, log, conf string
}

func newSSHBox(t *testing.T, keys int) sshBox {
	t.Helper()
	dir := t.TempDir()
	b := sshBox{root: filepath.Join(dir, "root"), bin: filepath.Join(dir, "bin"), log: filepath.Join(dir, "calls.log")}
	if err := os.MkdirAll(filepath.Join(b.root, "etc/ssh/sshd_config.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(b.root, "home/ops/.ssh/authorized_keys"), strings.Repeat("ssh-ed25519 AAAA ops\n", keys), 0o600)
	stub(t, b.bin, "sshd", `echo "sshd $*" >> "$STUB_LOG"
case "$1" in -t) [ "${STUB_T_FAIL:-0}" != 1 ];; -T) echo "authorizedkeysfile .ssh/authorized_keys"; `+
		`echo "passwordauthentication ${STUB_PA:-no}"; echo "pubkeyauthentication ${STUB_PK:-yes}";; esac`)
	stub(t, b.bin, "systemctl", `echo "systemctl $*" >> "$STUB_LOG"; true`)
	b.conf = filepath.Join(b.root, "etc/ssh/sshd_config.d/01-no-password-login.conf")
	return b
}

func (b sshBox) run(t *testing.T, kv ...string) (string, string) {
	t.Helper()
	r := runCmd(t, []string{bashPath(t), "-c", `set -euo pipefail; . "$1"; info() { echo "INFO $*"; }; ok() { echo "OK $*"; }; ` +
		`warn() { echo "WARN $*"; }; lens_ssh_key_only; echo "RC=$?"`, "test", lib(t)},
		env(append([]string{"PATH=" + b.bin + ":/usr/bin:/bin", "STUB_LOG=" + b.log, "SSH_KEY_ONLY_ROOT=" + b.root}, kv...)...))
	if r.code != 0 || !strings.Contains(r.stdout, "RC=0") {
		t.Fatalf("%+v", r)
	}
	calls := ""
	if exists(b.log) {
		calls = readFile(t, b.log)
	}
	return r.stdout, calls
}

func TestSSHKeyOnlyWritesValidatesAndReloads(t *testing.T) {
	b := newSSHBox(t, 1)
	out, calls := b.run(t)
	if readFile(t, b.conf) != keyOnlyConf {
		t.Fatal("drop-in content")
	}
	if fi, _ := os.Stat(b.conf); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if !strings.Contains(calls, "sshd -t") || !strings.Contains(calls, "systemctl reload sshd") ||
		!strings.Contains(out, "OK SSH is key-only") || strings.Contains(out, "WARN") {
		t.Fatalf("out=%q calls=%q", out, calls)
	}
	// Idempotent: a second run touches nothing.
	write(t, b.log, "", 0o644)
	out, calls = b.run(t)
	if strings.TrimSpace(out) != "RC=0" || calls != "" || readFile(t, b.conf) != keyOnlyConf {
		t.Fatalf("second run: out=%q calls=%q", out, calls)
	}
}

func TestSSHKeyOnlyLeavesSSHDAlone(t *testing.T) {
	for _, tc := range []struct {
		keys   int
		kv     []string
		expect string
	}{
		{0, nil, "WARN no SSH authorized keys found"},
		{1, []string{"LENS_KEEP_SSH_PASSWORDS=1"}, "INFO LENS_KEEP_SSH_PASSWORDS=1"},
		{1, []string{"DRY_RUN=1"}, "INFO [dry-run] would write"},
		{1, []string{"STUB_T_FAIL=1"}, "WARN sshd -t rejected"},
		{1, []string{"STUB_PK=no"}, "WARN sshd does not accept public keys"},
	} {
		b := newSSHBox(t, tc.keys)
		out, calls := b.run(t, tc.kv...)
		if !strings.Contains(out, tc.expect) || exists(b.conf) || strings.Contains(calls, "reload") {
			t.Fatalf("%+v: out=%q calls=%q", tc, out, calls)
		}
	}
}

func TestSSHKeyOnlyWarnsWhenAnEarlierDirectiveWins(t *testing.T) {
	b := newSSHBox(t, 1)
	out, _ := b.run(t, "STUB_PA=yes")
	if !exists(b.conf) || !strings.Contains(out, "WARN sshd still accepts passwords") {
		t.Fatalf("out=%q", out)
	}
}

func TestSSHKeyOnlyPutsBackAHandMadeDropInWhenSSHDTFails(t *testing.T) {
	b := newSSHBox(t, 1)
	write(t, b.conf, "PasswordAuthentication no\n", 0o600)
	out, _ := b.run(t, "STUB_T_FAIL=1")
	if !strings.Contains(out, "WARN sshd -t rejected") || readFile(t, b.conf) != "PasswordAuthentication no\n" {
		t.Fatalf("out=%q conf=%q", out, readFile(t, b.conf))
	}
}

// The update path a deployed Python box takes: its OLD update.sh re-execs the
// new one with these variables, inside the lock it already holds.
func TestUpdateScriptMigrationContract(t *testing.T) {
	src := read(t, "scripts/update.sh")
	for _, want := range []string{
		`LENS_UPDATE_NO_PULL:-0`, `LENS_UPDATE_BASE_SHA:-$before_sha`, `LENS_UPDATE_YES:-0`, `--yes|-y) export LENS_UPDATE_YES=1`,
		`lens_lock "$@"`, `lens_ensure_go`, `mktemp -d "${APP_DIR}.staging.XXXXXX"`, `lens_build "$STAGING"`,
		`"$PRIV_DIR/lens" migrate-access-db`, `install -m 0755 "$STAGING/bin/lens" "$PRIV_DIR/lens"`, `cp -a "$APP_DIR/.venv" "$BACKUP/.venv"`,
		`"$BACKUP/lens-cli.previous"`, `lens_install_cli "$PRIV_DIR/lens" "$CLI_TARGET" "$BIN_OWNER"`,
		`install -m 0644 "$PRIV_DIR/deploy/lens.service" "$UNIT_DIR/"`, `ensure_auth_pubkey`,
		`ensure_rootless_podman`, `remove_firecrawl`, `deploy/motd`, `lens_health`, `lens_prune_rollbacks`,
		`.deployed-revision`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("update.sh lost %q", want)
		}
	}
	// The venv goes only after the Go release answered /health.
	health := strings.Index(src, `lens_health || die`)
	retire := strings.Index(src, `rm -rf "$APP_DIR/.venv" "$APP_DIR/.venv.old"`)
	if health < 0 || retire < health {
		t.Error("the Python venv must be retired after the health check, never before")
	}
	// The build fails before anything live is touched.
	if strings.Index(src, `lens_build "$STAGING"`) > strings.Index(src, `systemctl stop "$SERVICE"`) {
		t.Error("the staging build must precede the service stop")
	}
	if strings.Contains(src, "python") || strings.Contains(src, " uv ") {
		t.Error("update.sh must not need Python or uv")
	}
	excl := read(t, "scripts/lib/deploy.sh")
	for _, want := range []string{"'/.venv'", "'/.env*'", "'/config.json'", "'/managed-files'", "'/data'", "'/.local'", "'/.config'", "'/.cache'"} {
		if !strings.Contains(excl, want) {
			t.Errorf("LENS_STATE_EXCLUDES lost %s", want)
		}
	}
}

func TestUnitRunsTheGoBinary(t *testing.T) {
	unit := read(t, "deploy/lens.service")
	for _, want := range []string{"ExecStart=/opt/lens/bin/lens serve --addr 127.0.0.1:8808", "WorkingDirectory=/opt/lens",
		"EnvironmentFile=/opt/lens/.env", "ProtectSystem=full", "ReadWritePaths=/opt/lens /var/lib/lens",
		"NoNewPrivileges=true        blocks"} {
		if !strings.Contains(unit, want) {
			t.Errorf("lens.service lost %q", want)
		}
	}
	for _, line := range strings.Split(unit, "\n") {
		for _, d := range []string{"NoNewPrivileges=", "ProtectControlGroups=", "ProtectKernelTunables=", "PrivateTmp=", "ProtectHome="} {
			if strings.HasPrefix(line, d) {
				t.Errorf("%s breaks rootless podman and must stay absent", d)
			}
		}
	}
}

// assertCLICopy checks the operator CLI is a regular file (never a symlink
// into the service-owned tree) with the same bytes as the release binary.
func assertCLICopy(t *testing.T, cli, bin string) {
	t.Helper()
	st, err := os.Lstat(cli)
	if err != nil || !st.Mode().IsRegular() {
		t.Fatalf("CLI %s is not a regular file: %v %v", cli, st, err)
	}
	if readFile(t, cli) != readFile(t, bin) {
		t.Fatalf("CLI %s differs from %s", cli, bin)
	}
}

// Root never installs a system file (unit, motd, Caddy site) from the
// service-owned $APP_DIR or the service-owned staging tree.
func TestRootInstallsNoFileFromServiceOwnedTrees(t *testing.T) {
	for _, script := range []string{"scripts/bootstrap.sh", "scripts/update.sh"} {
		for i, line := range strings.Split(read(t, script), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "#") {
				continue
			}
			if strings.Contains(code, `"$APP_DIR/deploy/`) || (strings.Contains(code, `"$STAGING/deploy/`) && !strings.Contains(code, `"$PRIV_DIR/deploy/"`)) {
				t.Errorf("%s:%d installs from a service-owned tree: %s", script, i+1, code)
			}
		}
	}
}
