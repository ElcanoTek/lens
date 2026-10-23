// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package deploytest

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLens stands in for bin/lens: it reports the revision the Makefile
// stamped and records migrate-access-db calls.
const fakeLens = `#!/bin/sh
case "$1" in
  --version) echo "lens test (@COMMIT@)" ;;
  migrate-access-db) shift; echo "$*" >> "$MIGRATE_LOG" ;;
esac
`

// fakeMakefile builds fakeLens the way the real Makefile builds bin/lens:
// from the COMMIT update.sh passes, failing on demand.
const fakeMakefile = "build:\n" +
	"\ttest -z \"$(FAIL_BUILD)\"\n" +
	"\tmkdir -p bin\n" +
	"\tsed 's/@COMMIT@/$(COMMIT)/' fake-lens.sh > bin/lens\n" +
	"\tchmod 755 bin/lens\n"

type updateBox struct {
	dir, src, app, unit, cli, stubs, sha string
	systemctlLog, migrateLog             string
}

// newUpdateBox lays out a box mid-migration: /opt/lens holds a Python
// release (code, .venv, the copied bash CLI, its unit) and /opt/lens-src is
// already fast-forwarded to a Go release — the state the OLD update.sh leaves
// when it re-execs the new one.
func newUpdateBox(t *testing.T) updateBox {
	t.Helper()
	requireTools(t, "git", "rsync", "make", "sed")
	dir := t.TempDir()
	b := updateBox{dir: dir, src: filepath.Join(dir, "lens-src"), app: filepath.Join(dir, "lens"),
		unit: filepath.Join(dir, "units"), cli: filepath.Join(dir, "cli"), stubs: filepath.Join(dir, "stubs"),
		systemctlLog: filepath.Join(dir, "systemctl.log"), migrateLog: filepath.Join(dir, "migrate.log")}

	// Source checkout: the real deploy scripts + unit, a fake build.
	for _, rel := range []string{"scripts/update.sh", "scripts/lib/deploy.sh", "deploy/lens.service", "deploy/motd"} {
		write(t, filepath.Join(b.src, rel), read(t, rel), 0o755)
	}
	write(t, filepath.Join(b.src, "Makefile"), fakeMakefile, 0o644)
	write(t, filepath.Join(b.src, "fake-lens.sh"), fakeLens, 0o755)
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", b.src, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "go release")
	b.sha = git("rev-parse", "HEAD")

	// The live Python release and its instance state.
	tree(t, b.app, map[string]string{
		"web_service.py": "python", ".venv/bin/python": "python", "deploy/lens-cli": "bash cli",
		".env": "AUTH_SIGNING_PUBKEY=abc\n", "managed-files/outputs/_jobs.json": `{"jobs":[]}`,
		"data/access.db": "legacy", ".deployed-revision": "oldsha\n",
	})
	write(t, filepath.Join(b.unit, "lens.service"), "python unit", 0o644)
	write(t, b.cli, "bash cli", 0o755)

	stub(t, b.stubs, "systemctl", `echo "$*" >> "`+b.systemctlLog+`"`)
	stub(t, b.stubs, "curl", `printf '%s' "${STUB_HEALTH:-200}"`)
	for _, name := range []string{"dnf", "restorecon", "go", "usermod", "loginctl"} {
		stub(t, b.stubs, name, "exit 0")
	}
	stub(t, b.stubs, "podman", "exit 1")
	stub(t, b.stubs, "runuser", `shift 3; exec "$@"`)
	return b
}

func (b updateBox) run(t *testing.T, kv ...string) result {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return runCmd(t, []string{bashPath(t), filepath.Join(b.src, "scripts/update.sh")}, extend(append([]string{
		"PATH=" + b.stubs + ":" + os.Getenv("PATH"),
		// What the Python-era update.sh passes when it re-execs this one,
		// inside the deploy lock it holds.
		"LENS_UPDATE_NO_PULL=1", "LENS_UPDATE_YES=1", "LENS_UPDATE_BASE_SHA=0123456789ab", "LENS_DEPLOY_LOCKED=1",
		"SRC_DIR=" + b.src, "APP_DIR=" + b.app, "APP_USER=" + u.Username, "LENS_BIN_OWNER=" + u.Username,
		"LENS_UNIT_DIR=" + b.unit, "LENS_CLI_TARGET=" + b.cli, "LENS_STATE_DIR=" + filepath.Join(b.dir, "state"),
		"LENS_MOTD_FILE=" + filepath.Join(b.dir, "motd"), "LENS_BUILD_CACHE=" + filepath.Join(b.dir, "cache"),
		"LENS_UPDATE_SKIP_DEEP=1", "LENS_UPDATE_TEST_NONROOT=1", "LENS_HEALTH_ATTEMPTS=1",
		"MIGRATE_LOG=" + b.migrateLog, "TERM=dumb",
	}, kv...)...))
}

func (b updateBox) assertPythonRelease(t *testing.T) {
	t.Helper()
	if readFile(t, filepath.Join(b.app, "web_service.py")) != "python" || readFile(t, filepath.Join(b.app, ".venv/bin/python")) != "python" {
		t.Fatal("Python release not in place")
	}
	if exists(filepath.Join(b.app, "bin")) || exists(filepath.Join(b.app, "Makefile")) {
		t.Fatal("Go release files in the live tree")
	}
	if readFile(t, filepath.Join(b.unit, "lens.service")) != "python unit" {
		t.Fatal("unit is not the Python one")
	}
	if fi, err := os.Lstat(b.cli); err != nil || fi.Mode()&os.ModeSymlink != 0 || readFile(t, b.cli) != "bash cli" {
		t.Fatal("CLI is not the bash script")
	}
	if strings.TrimSpace(readFile(t, filepath.Join(b.app, ".deployed-revision"))) != "oldsha" {
		t.Fatal(".deployed-revision moved")
	}
	b.assertState(t)
}

func (b updateBox) assertState(t *testing.T) {
	t.Helper()
	if readFile(t, filepath.Join(b.app, "managed-files/outputs/_jobs.json")) != `{"jobs":[]}` ||
		readFile(t, filepath.Join(b.app, "data/access.db")) != "legacy" ||
		!strings.Contains(readFile(t, filepath.Join(b.app, ".env")), "AUTH_SIGNING_PUBKEY=abc") {
		t.Fatal("instance state was touched")
	}
}

func TestUpdateMigratesAPythonReleaseToGo(t *testing.T) {
	b := newUpdateBox(t)
	r := b.run(t)
	if r.code != 0 {
		t.Fatalf("update failed (%d):\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "Updated 0123456789ab → "+b.sha[:12]) {
		t.Fatalf("summary lost the base sha:\n%s", r.stdout)
	}
	if got := readFile(t, filepath.Join(b.app, "bin/lens")); !strings.Contains(got, b.sha) {
		t.Fatal("bin/lens not stamped with the deployed revision")
	}
	if exists(filepath.Join(b.app, ".venv")) || exists(filepath.Join(b.app, "web_service.py")) {
		t.Fatal("Python release not retired after a healthy start")
	}
	if strings.TrimSpace(readFile(t, filepath.Join(b.app, ".deployed-revision"))) != b.sha {
		t.Fatal(".deployed-revision not written")
	}
	assertCLICopy(t, b.cli, filepath.Join(b.app, "bin/lens"))
	if readFile(t, filepath.Join(b.unit, "lens.service")) != read(t, "deploy/lens.service") {
		t.Fatal("unit not installed")
	}
	migrate := readFile(t, b.migrateLog)
	if !strings.Contains(migrate, "--legacy "+b.app+"/data/access.db --target "+filepath.Join(b.dir, "state/access.db")) {
		t.Fatalf("migrate-access-db args: %s", migrate)
	}
	calls := readFile(t, b.systemctlLog)
	if strings.Index(calls, "stop lens.service") > strings.Index(calls, "start lens.service") {
		t.Fatalf("systemctl order: %s", calls)
	}
	// The snapshot keeps the Python release (venv included) for a manual
	// rollback until it is pruned.
	snaps, _ := filepath.Glob(b.app + ".rollback.*")
	if len(snaps) != 1 || readFile(t, filepath.Join(snaps[0], ".venv/bin/python")) != "python" ||
		readFile(t, filepath.Join(snaps[0], "lens-cli.previous")) != "bash cli" {
		t.Fatalf("snapshot %v incomplete", snaps)
	}
	if left, _ := filepath.Glob(b.app + ".staging.*"); len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}
	b.assertState(t)
}

func TestUpdateFailedHealthCheckRestoresThePythonRelease(t *testing.T) {
	b := newUpdateBox(t)
	r := b.run(t, "STUB_HEALTH=503")
	if r.code == 0 {
		t.Fatal("an unhealthy release must fail the update")
	}
	if !strings.Contains(r.stderr, "restoring") {
		t.Fatalf("no rollback message:\n%s", r.stderr)
	}
	b.assertPythonRelease(t)
}

func TestUpdateBuildFailureLeavesTheLiveInstallUntouched(t *testing.T) {
	b := newUpdateBox(t)
	r := b.run(t, "FAIL_BUILD=1")
	if r.code == 0 || !strings.Contains(r.stderr, "live install untouched") {
		t.Fatalf("code=%d stderr=%s", r.code, r.stderr)
	}
	if exists(b.systemctlLog) && strings.Contains(readFile(t, b.systemctlLog), "stop") {
		t.Fatal("a failed build stopped the service")
	}
	if snaps, _ := filepath.Glob(b.app + ".rollback.*"); len(snaps) != 0 {
		t.Fatal("a failed build took a snapshot")
	}
	b.assertPythonRelease(t)
}

// A second update, Go to Go, whose health check fails, comes back on the
// previous Go release with the CLI still a symlink.
func TestUpdateFailedGoToGoRestoresThePreviousGoRelease(t *testing.T) {
	b := newUpdateBox(t)
	if r := b.run(t); r.code != 0 {
		t.Fatalf("first update: %s", r.stderr)
	}
	first := readFile(t, filepath.Join(b.app, "bin/lens"))
	write(t, filepath.Join(b.src, "fake-lens.sh"), fakeLens+"# v2\n", 0o755)
	cmd := exec.Command("git", "-C", b.src, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qam", "v2")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if r := b.run(t, "STUB_HEALTH=503"); r.code == 0 {
		t.Fatal("unhealthy second update succeeded")
	}
	if readFile(t, filepath.Join(b.app, "bin/lens")) != first {
		t.Fatal("previous Go binary not restored")
	}
	assertCLICopy(t, b.cli, filepath.Join(b.app, "bin/lens"))
	if exists(filepath.Join(b.app, ".venv")) {
		t.Fatal("a venv reappeared")
	}
	b.assertState(t)
}

// The real first migration: a box on the Python release runs ITS update.sh
// (testdata/python-release is that release's update.sh + lib/deploy.sh,
// verbatim), which fetches the Go release, notices update.sh changed and
// re-execs the new script with LENS_UPDATE_NO_PULL=1 LENS_UPDATE_YES=1
// LENS_UPDATE_BASE_SHA=<old HEAD>. The old script refuses to run as anyone
// but root and has no override, so this needs root (CI runs it under sudo).
func TestPythonEraUpdateReexecsIntoTheGoUpdate(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the Python-era update.sh only runs as root")
	}
	b := newUpdateBox(t)
	gitIn := func(dir string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// Rebuild the checkout's history: the Python release, then the Go one.
	work := filepath.Join(b.dir, "work")
	if err := os.Rename(b.src, work); err != nil {
		t.Fatal(err)
	}
	goRev := gitIn(work, "rev-parse", "HEAD")
	gitIn(work, "checkout", "-q", "--orphan", "python")
	gitIn(work, "rm", "-rq", "--cached", ".")
	pyTree := filepath.Join(b.dir, "pytree")
	write(t, filepath.Join(pyTree, "scripts/update.sh"), readFile(t, filepath.Join(repo(t), "internal/deploytest/testdata/python-release/update.sh")), 0o755)
	write(t, filepath.Join(pyTree, "scripts/lib/deploy.sh"), readFile(t, filepath.Join(repo(t), "internal/deploytest/testdata/python-release/deploy.sh")), 0o644)
	write(t, filepath.Join(pyTree, "web_service.py"), "python", 0o644)
	cmd := exec.Command("git", "--git-dir="+filepath.Join(work, ".git"), "--work-tree="+pyTree,
		"-c", "user.name=t", "-c", "user.email=t@example.com", "add", "-A")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	gitIn(work, "commit", "-q", "-m", "python release")
	pyRev := gitIn(work, "rev-parse", "HEAD")
	// main = python release + the Go release's tree on top.
	gitIn(work, "checkout", "-q", "-B", "main")
	gitIn(work, "read-tree", "-u", "--reset", goRev)
	gitIn(work, "commit", "-q", "-m", "go release")
	goHead := gitIn(work, "rev-parse", "HEAD")

	origin := filepath.Join(b.dir, "origin.git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", work, origin).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := exec.Command("git", "clone", "-q", "--branch", "main", origin, b.src).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	gitIn(b.src, "reset", "-q", "--hard", pyRev) // the box is on the Python release
	write(t, filepath.Join(b.app, ".deployed-revision"), pyRev+"\n", 0o644)

	r := b.run(t, "LENS_UPDATE_NO_PULL=0", "LENS_UPDATE_BASE_SHA=")
	if r.code != 0 {
		t.Fatalf("migration failed (%d):\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, "update.sh changed in this update — re-executing the new version") {
		t.Fatalf("old script did not re-exec:\n%s", r.stderr)
	}
	if !strings.Contains(r.stdout, "Updated "+pyRev[:12]+" → "+goHead[:12]) {
		t.Fatalf("summary:\n%s", r.stdout)
	}
	if strings.TrimSpace(readFile(t, filepath.Join(b.app, ".deployed-revision"))) != goHead ||
		!strings.Contains(readFile(t, filepath.Join(b.app, "bin/lens")), goHead) || exists(filepath.Join(b.app, ".venv")) {
		t.Fatal("Go release not deployed")
	}
	b.assertState(t)
}

// firecrawlPodman fakes the lens user's podman storage on a box from before
// Firecrawl's removal, next to things that are not Firecrawl's: a labelled
// container with a foreign name, a lookalike volume, an unrelated named and
// anonymous volume, the Chrome image and podman's own network. FAIL_RMI makes
// one image removal fail, as for an image another container still uses.
const firecrawlPodman = `echo "$*" >> "$PODMAN_LOG"
case "$*" in
  "ps -a --filter label=io.podman.compose.project=elcano-firecrawl --format {{.ID}} {{.Names}}")
    printf 'c0ffee elcano-firecrawl_api_1\nbad1 imposter\n' ;;
  "ps -a --filter label=com.docker.compose.project=elcano-firecrawl --format {{.ID}} {{.Names}}")
    echo 'c0ffee elcano-firecrawl_api_1' ;;
  "images --format {{.Repository}}:{{.Tag}}")
    printf 'ghcr.io/firecrawl/firecrawl:latest\ndocker.io/library/redis:alpine\nselenium/standalone-chrome:latest\n' ;;
  "volume ls -q") printf 'elcano-firecrawl_nuq-postgres-data\nelcano-firecrawl_extra\nlens-jobs-archive\n3f9a1b2c4d5e\n' ;;
  "network ls --format {{.Name}}") printf 'elcano-firecrawl_backend\npodman\n' ;;
  "rmi docker.io/library/redis:alpine") [ -z "$FAIL_RMI" ] || exit 1 ;;
esac
exit 0`

// Only what the retired compose project made is removed, and nothing is
// forced past another container's use of it.
func TestUpdateRemovesOnlyFirecrawlsOwnContainersAndVolumes(t *testing.T) {
	b := newUpdateBox(t)
	podmanLog := filepath.Join(b.dir, "podman.log")
	stub(t, b.stubs, "podman", firecrawlPodman)
	r := b.run(t, "PODMAN_LOG="+podmanLog)
	if r.code != 0 {
		t.Fatalf("update failed (%d):\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	calls := readFile(t, podmanLog)
	for _, want := range []string{"rm -f -v c0ffee", "volume rm elcano-firecrawl_nuq-postgres-data",
		"network rm elcano-firecrawl_backend", "rmi ghcr.io/firecrawl/firecrawl:latest", "rmi docker.io/library/redis:alpine",
		"image prune -f"} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing podman %q in:\n%s", want, calls)
		}
	}
	if strings.Count(calls, "rm -f -v c0ffee") != 1 {
		t.Errorf("container removed more than once:\n%s", calls)
	}
	// Only arguments are logged, so any of these here means it was acted on.
	for _, never := range []string{"bad1", "volume prune", "elcano-firecrawl_extra", "lens-jobs-archive", "3f9a1b2c4d5e",
		"selenium", "rmi -f", "network rm podman"} {
		if strings.Contains(calls, never) {
			t.Errorf("podman touched %q:\n%s", never, calls)
		}
	}
	if !strings.Contains(r.stdout, "removed the retired Firecrawl stack") {
		t.Fatalf("no removal reported:\n%s", r.stdout)
	}
}

func TestUpdateReportsAFirecrawlCleanupThatCouldNotFinish(t *testing.T) {
	b := newUpdateBox(t)
	stub(t, b.stubs, "podman", firecrawlPodman)
	r := b.run(t, "PODMAN_LOG="+filepath.Join(b.dir, "podman.log"), "FAIL_RMI=1")
	if r.code != 0 {
		t.Fatalf("update failed (%d):\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	out := r.stdout + r.stderr
	if !strings.Contains(out, "could not remove every Firecrawl container, volume, network or image") ||
		strings.Contains(out, "removed the retired Firecrawl stack") {
		t.Fatalf("a partial cleanup was reported as complete:\n%s", out)
	}
}

func TestUpdateScriptNeverPrunesVolumes(t *testing.T) {
	if strings.Contains(read(t, "scripts/update.sh"), "volume prune") {
		t.Fatal("update.sh prunes every unused volume; remove only Firecrawl's own")
	}
}
