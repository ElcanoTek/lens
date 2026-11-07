// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package deploytest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestVersionScript(t *testing.T) {
	requireTools(t, "git", "awk", "sort")
	dir := t.TempDir()
	script := filepath.Join(dir, "scripts/version.sh")
	write(t, script, read(t, "scripts/version.sh"), 0o755)

	gitIn := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	gitIn("init", "-q")
	write(t, filepath.Join(dir, "README.md"), "one\n", 0o644)
	gitIn("add", "-A")
	gitIn("commit", "-q", "-m", "initial")
	sha1 := gitIn("rev-parse", "HEAD")

	// Before any tag exists: next on a fixed date is .1, current is dev, released-at is empty, describe is dev+g<sha>.
	r := runCmd(t, []string{bashPath(t), script, "next", "2026.11.07"}, func(c *exec.Cmd) { c.Dir = dir })
	if r.code != 0 || strings.TrimSpace(r.stdout) != "v2026.11.07.1" {
		t.Fatalf("next = (%d, %q, %q)", r.code, r.stdout, r.stderr)
	}
	r = runCmd(t, []string{bashPath(t), script, "current"}, func(c *exec.Cmd) { c.Dir = dir })
	if r.code != 0 || strings.TrimSpace(r.stdout) != "dev" {
		t.Fatalf("current on untagged commit = (%d, %q)", r.code, r.stdout)
	}
	r = runCmd(t, []string{bashPath(t), script, "released-at", sha1}, func(c *exec.Cmd) { c.Dir = dir })
	if r.code != 0 || strings.TrimSpace(r.stdout) != "" {
		t.Fatalf("released-at on untagged commit = (%d, %q)", r.code, r.stdout)
	}
	r = runCmd(t, []string{bashPath(t), script, "describe"}, func(c *exec.Cmd) { c.Dir = dir })
	if r.code != 0 || !strings.HasPrefix(strings.TrimSpace(r.stdout), "dev+g") {
		t.Fatalf("describe untagged = (%d, %q)", r.code, r.stdout)
	}

	// A malformed tag is ignored by released-at and current.
	gitIn("tag", "v2026.11.07.99oops")
	r = runCmd(t, []string{bashPath(t), script, "released-at", sha1}, func(c *exec.Cmd) { c.Dir = dir })
	if r.code != 0 || strings.TrimSpace(r.stdout) != "" {
		t.Fatalf("released-at with malformed tag = (%d, %q)", r.code, r.stdout)
	}
	gitIn("tag", "-d", "v2026.11.07.99oops")

	// Tag HEAD as v2026.11.07.1.
	gitIn("tag", "-a", "v2026.11.07.1", "-m", "release")
	r = runCmd(t, []string{bashPath(t), script, "current"}, func(c *exec.Cmd) { c.Dir = dir })
	if r.code != 0 || strings.TrimSpace(r.stdout) != "2026.11.07.1" {
		t.Fatalf("current = (%d, %q)", r.code, r.stdout)
	}
	r = runCmd(t, []string{bashPath(t), script, "released-at", sha1}, func(c *exec.Cmd) { c.Dir = dir })
	if r.code != 0 || strings.TrimSpace(r.stdout) != "v2026.11.07.1" {
		t.Fatalf("released-at = (%d, %q)", r.code, r.stdout)
	}
	r = runCmd(t, []string{bashPath(t), script, "describe"}, func(c *exec.Cmd) { c.Dir = dir })
	if r.code != 0 || strings.TrimSpace(r.stdout) != "2026.11.07.1" {
		t.Fatalf("describe = (%d, %q)", r.code, r.stdout)
	}
	// A malformed leading-zero tag is ignored by next (never parsed as octal) and by released-at/current.
	gitIn("tag", "v2026.11.07.08")
	r = runCmd(t, []string{bashPath(t), script, "next", "2026.11.07"}, func(c *exec.Cmd) { c.Dir = dir })
	if r.code != 0 || strings.TrimSpace(r.stdout) != "v2026.11.07.2" {
		t.Fatalf("next with leading-zero tag = (%d, %q, %q)", r.code, r.stdout, r.stderr)
	}
	r = runCmd(t, []string{bashPath(t), script, "released-at", sha1}, func(c *exec.Cmd) { c.Dir = dir })
	if r.code != 0 || strings.TrimSpace(r.stdout) != "v2026.11.07.1" {
		t.Fatalf("released-at with co-located malformed tag = (%d, %q)", r.code, r.stdout)
	}
	gitIn("tag", "-d", "v2026.11.07.08")

	// A nearer malformed tag on a child commit never masks an ancestor valid release tag in describe.
	write(t, filepath.Join(dir, "README.md"), "two\n", 0o644)
	gitIn("add", "-A")
	gitIn("commit", "-q", "-m", "second")
	gitIn("tag", "v2026.11.08.2oops")
	r = runCmd(t, []string{bashPath(t), script, "describe"}, func(c *exec.Cmd) { c.Dir = dir })
	if r.code != 0 || !strings.HasPrefix(strings.TrimSpace(r.stdout), "2026.11.07.1+1.g") {
		t.Fatalf("describe with nearer malformed tag = (%d, %q)", r.code, r.stdout)
	}
	gitIn("tag", "-d", "v2026.11.08.2oops")

	// filter keeps only valid release tags and sorts them chronologically.
	r = runCmd(t, []string{bashPath(t), script, "filter"},
		stdin("v2026.11.07.10\nv2026.11.07.2\nv2026.11.07.0\nbogus\nv2026.11.06.3\n"))
	want := "v2026.11.06.3\nv2026.11.07.2\nv2026.11.07.10"
	if r.code != 0 || strings.TrimSpace(r.stdout) != want {
		t.Fatalf("filter = (%d, %q), want %q", r.code, strings.TrimSpace(r.stdout), want)
	}
}

func TestRetryTransientAndPushRejectionScripts(t *testing.T) {
	retryScript := filepath.Join(repo(t), "scripts/retry-transient.sh")
	rejectScript := filepath.Join(repo(t), "scripts/release-push-rejection.sh")

	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	flaky := filepath.Join(dir, "flaky.sh")
	write(t, flaky, fmt.Sprintf(`#!/bin/sh
n=$(cat %q 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" > %q
if [ "$n" -lt 3 ]; then
  echo "503 Service Unavailable" >&2
  exit 1
fi
`, counter, counter), 0o755)

	stub(t, dir, "sleep", "exit 0")
	r := runCmd(t, []string{bashPath(t), retryScript, flaky},
		extend("PATH="+dir+":"+os.Getenv("PATH"), "RETRY_MAX_ATTEMPTS=4", "RETRY_BASE_SLEEP=0"))
	if r.code != 0 || strings.TrimSpace(readFile(t, counter)) != "3" {
		t.Fatalf("retry-transient = (%d, count=%q, stderr=%s)", r.code, readFile(t, counter), r.stderr)
	}

	cases := []struct {
		log, want string
	}{
		{"! [rejected] v2026.11.07.1 -> v2026.11.07.1 (already exists)\n", "contention"},
		{"! [rejected] v2026.11.07.1 -> v2026.11.07.1 (non-fast-forward)\n", "contention"},
		{"! [remote rejected] (refusing to allow a GitHub App to create or update workflow `.github/workflows/release.yml` without `workflows` permission)\n", "workflow-permission"},
		{"fatal: Authentication failed\n", "other"},
	}
	for _, tc := range cases {
		logFile := filepath.Join(dir, "push.log")
		write(t, logFile, tc.log, 0o600)
		res := runCmd(t, []string{bashPath(t), rejectScript, logFile})
		if res.code != 0 || strings.TrimSpace(res.stdout) != tc.want {
			t.Fatalf("log %q -> (%d, %q), want %q", tc.log, res.code, res.stdout, tc.want)
		}
	}
}

// releaseFixture sets up a local httptest server serving a fake cosign binary
// and signed release artifacts for tag at commit.
type releaseFixture struct {
	mu         sync.Mutex
	root       string
	cache      string
	tag        string
	commit     string
	binaryBody []byte
	server     *httptest.Server
	tamperBin  bool
	tamperJSON bool
	wrongTag   bool
	wrongSHA   bool
	failVerify bool
}

func (f *releaseFixture) update(fn func(*releaseFixture)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func newReleaseFixture(t *testing.T, tag, commit string) *releaseFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "repo")
	cache := filepath.Join(dir, "cosign-cache")
	write(t, filepath.Join(root, "scripts/lib/release.sh"), read(t, "scripts/lib/release.sh"), 0o755)

	// Fake cosign binary whose SHA-256 is pinned in root/scripts/cosign.lock.
	cosignScript := []byte(`#!/bin/sh
if [ "${FAIL_COSIGN_VERIFY:-0}" = "1" ]; then
  exit 1
fi
for arg in "$@"; do
  last="$arg"
done
case "$1" in
  verify-blob)
    grep -q "^valid-bundle-for:$(basename "$last")$" "${last}.sigstore.json"
    ;;
  *)
    exit 1
    ;;
esac
`)
	cosignDigest := sha256Hex(cosignScript)
	lockBody := fmt.Sprintf("version v3.1.4\nlinux/amd64 %s\nlinux/arm64 %s\n", cosignDigest, cosignDigest)
	write(t, filepath.Join(root, "scripts/cosign.lock"), lockBody, 0o644)

	binBody := []byte(fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n  --version) echo \"lens %s (%s)\" ;;\n  migrate-access-db) shift; [ -n \"${MIGRATE_LOG:-}\" ] && echo \"$*\" >> \"$MIGRATE_LOG\" ;;\nesac\n", strings.TrimPrefix(tag, "v"), commit))

	f := &releaseFixture{
		root:       root,
		cache:      cache,
		tag:        tag,
		commit:     commit,
		binaryBody: binBody,
	}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		tagVal := f.tag
		relTag := f.tag
		if f.wrongTag {
			relTag = "v2026.01.01.1"
		}
		relCommit := f.commit
		if f.wrongSHA {
			relCommit = strings.Repeat("f", 40)
		}
		binBytes := append([]byte(nil), f.binaryBody...)
		tamperBin := f.tamperBin
		tamperJSON := f.tamperJSON
		failVerify := f.failVerify
		f.mu.Unlock()

		binDigest := sha256Hex(binBytes)
		relJSON := []byte(fmt.Sprintf(`{"schema":1,"tag":%q,"version":%q,"commit":%q,"built_at":"2026-11-07T00:00:00Z","binaries":{"lens-linux-amd64":%q,"lens-linux-arm64":%q}}`+"\n",
			relTag, strings.TrimPrefix(relTag, "v"), relCommit, binDigest, binDigest))
		jsonDigest := sha256Hex(relJSON)
		sums := []byte(fmt.Sprintf("%s  lens-linux-amd64\n%s  lens-linux-arm64\n%s  release.json\n",
			binDigest, binDigest, jsonDigest))

		switch r.URL.Path {
		case "/latest":
			_, _ = fmt.Fprintf(w, `{"tag_name":%q}`, tagVal)
		case "/cosign/cosign-linux-amd64", "/cosign/cosign-linux-arm64":
			_, _ = w.Write(cosignScript)
		case "/releases/" + tagVal + "/SHA256SUMS":
			_, _ = w.Write(sums)
		case "/releases/" + tagVal + "/SHA256SUMS.sigstore.json":
			if failVerify {
				_, _ = w.Write([]byte("invalid-bundle\n"))
				return
			}
			_, _ = w.Write([]byte("valid-bundle-for:SHA256SUMS\n"))
		case "/releases/" + tagVal + "/release.json":
			if tamperJSON {
				_, _ = w.Write(append(relJSON, ' '))
				return
			}
			_, _ = w.Write(relJSON)
		case "/releases/" + tagVal + "/lens-linux-amd64", "/releases/" + tagVal + "/lens-linux-arm64":
			if tamperBin {
				_, _ = w.Write(append(binBytes, []byte("# tampered\n")...))
				return
			}
			_, _ = w.Write(binBytes)
		case "/releases/" + tagVal + "/lens-linux-amd64.sigstore.json":
			if failVerify {
				_, _ = w.Write([]byte("invalid-bundle\n"))
				return
			}
			_, _ = w.Write([]byte("valid-bundle-for:lens-linux-amd64\n"))
		case "/releases/" + tagVal + "/lens-linux-arm64.sigstore.json":
			if failVerify {
				_, _ = w.Write([]byte("invalid-bundle\n"))
				return
			}
			_, _ = w.Write([]byte("valid-bundle-for:lens-linux-arm64\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *releaseFixture) env() []string {
	return []string{
		"LENS_COSIGN_URL=" + f.server.URL + "/cosign",
		"LENS_RELEASE_BASE_URL=" + f.server.URL + "/releases",
		"LENS_RELEASE_LATEST_URL=" + f.server.URL + "/latest",
		"LENS_COSIGN_CACHE=" + f.cache,
	}
}

func TestReleaseVerifyAndFetchContract(t *testing.T) {
	requireTools(t, "curl", "sha256sum", "awk")
	tag := "v2026.11.07.1"
	commit := strings.Repeat("ab", 20)
	f := newReleaseFixture(t, tag, commit)
	outDir := filepath.Join(filepath.Dir(f.root), "staged")

	call := fmt.Sprintf(`. %q
tag="$(lens_release_latest_tag)"
[ "$tag" = %q ]
lens_release_verify_manifest %q "$tag" %q %q
bin="$(lens_release_fetch %q "$tag" %q %q %q)"
"$bin" --version
`, filepath.Join(f.root, "scripts/lib/release.sh"), tag, f.root, commit, f.cache, f.root, outDir, f.cache, commit)

	r := bash(t, call, nil, extend(f.env()...))
	if r.code != 0 || !strings.Contains(r.stdout, commit) {
		t.Fatalf("release verify+fetch failed (%d):\nstdout=%s\nstderr=%s", r.code, r.stdout, r.stderr)
	}

	// Tampered binary is rejected.
	f.update(func(f *releaseFixture) { f.tamperBin = true })
	r = bash(t, fmt.Sprintf(`. %q && lens_release_fetch %q %q %q %q %q`,
		filepath.Join(f.root, "scripts/lib/release.sh"), f.root, tag, filepath.Join(filepath.Dir(f.root), "bad-bin"), f.cache, commit),
		nil, extend(f.env()...))
	f.update(func(f *releaseFixture) { f.tamperBin = false })
	if r.code == 0 || !strings.Contains(r.stderr, "does not match the signed SHA256SUMS") {
		t.Fatalf("tampered binary was not rejected: (%d, %s)", r.code, r.stderr)
	}

	// Tampered release.json is rejected.
	f.update(func(f *releaseFixture) { f.tamperJSON = true })
	r = bash(t, fmt.Sprintf(`. %q && lens_release_verify_manifest %q %q %q %q`,
		filepath.Join(f.root, "scripts/lib/release.sh"), f.root, tag, commit, f.cache),
		nil, extend(f.env()...))
	f.update(func(f *releaseFixture) { f.tamperJSON = false })
	if r.code == 0 || !strings.Contains(r.stderr, "release.json does not match the signed SHA256SUMS") {
		t.Fatalf("tampered release.json was not rejected: (%d, %s)", r.code, r.stderr)
	}

	// Wrong tag in release.json is rejected.
	f.update(func(f *releaseFixture) { f.wrongTag = true })
	r = bash(t, fmt.Sprintf(`. %q && lens_release_verify_manifest %q %q %q %q`,
		filepath.Join(f.root, "scripts/lib/release.sh"), f.root, tag, commit, f.cache),
		nil, extend(f.env()...))
	f.update(func(f *releaseFixture) { f.wrongTag = false })
	if r.code == 0 || !strings.Contains(r.stderr, "release.json does not name "+tag) {
		t.Fatalf("wrong tag in release.json was not rejected: (%d, %s)", r.code, r.stderr)
	}

	// Wrong commit in release.json is rejected.
	f.update(func(f *releaseFixture) { f.wrongSHA = true })
	r = bash(t, fmt.Sprintf(`. %q && lens_release_verify_manifest %q %q %q %q`,
		filepath.Join(f.root, "scripts/lib/release.sh"), f.root, tag, commit, f.cache),
		nil, extend(f.env()...))
	f.update(func(f *releaseFixture) { f.wrongSHA = false })
	if r.code == 0 || !strings.Contains(r.stderr, "does not name commit "+commit) {
		t.Fatalf("wrong commit in release.json was not rejected: (%d, %s)", r.code, r.stderr)
	}

	// Invalid Sigstore bundle is rejected.
	f.update(func(f *releaseFixture) { f.failVerify = true })
	r = bash(t, fmt.Sprintf(`. %q && lens_release_verify_manifest %q %q %q %q`,
		filepath.Join(f.root, "scripts/lib/release.sh"), f.root, tag, commit, f.cache),
		nil, extend(f.env()...))
	f.update(func(f *releaseFixture) { f.failVerify = false })
	if r.code == 0 || !strings.Contains(r.stderr, "signature verification FAILED") {
		t.Fatalf("invalid signature was not rejected: (%d, %s)", r.code, r.stderr)
	}

	// Tampered cosign binary is rejected when cosign.lock does not match.
	write(t, filepath.Join(f.root, "scripts/cosign.lock"),
		"version v3.1.4\nlinux/amd64 "+strings.Repeat("0", 64)+"\nlinux/arm64 "+strings.Repeat("0", 64)+"\n", 0o644)
	r = bash(t, fmt.Sprintf(`. %q && lens_release_cosign %q %q`,
		filepath.Join(f.root, "scripts/lib/release.sh"), f.root, filepath.Join(filepath.Dir(f.root), "bad-cosign")),
		nil, extend(f.env()...))
	if r.code == 0 || !strings.Contains(r.stderr, "does not match the digest pinned in cosign.lock") {
		t.Fatalf("tampered cosign was not rejected: (%d, %s)", r.code, r.stderr)
	}
}

func TestSignedReleaseUpdateWithoutGoOrMake(t *testing.T) {
	requireTools(t, "git", "rsync", "curl", "sha256sum", "awk")
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("needs curl")
	}
	b := newUpdateBox(t)
	// Route /health probes to STUB_HEALTH while forwarding release downloads to real curl.
	stub(t, b.stubs, "curl", fmt.Sprintf(`for arg in "$@"; do
  if [ "$arg" = "http://127.0.0.1:8808/health" ]; then
    printf '%%s' "${STUB_HEALTH:-200}"
    exit 0
  fi
done
exec %q "$@"
`, realCurl))

	gitIn := func(dir string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// Set up an origin repository with a signed release tag v2026.11.07.1.
	origin := filepath.Join(b.dir, "origin.git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", b.src, origin).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	gitIn(b.src, "remote", "add", "origin", origin)

	// Create an older initial commit so update.sh sees an incoming release tag.
	oldSha := b.sha
	write(t, filepath.Join(b.src, "RELEASE_MARKER"), "v2026.11.07.1\n", 0o644)
	gitIn(b.src, "add", "RELEASE_MARKER")
	gitIn(b.src, "commit", "-q", "-m", "release v2026.11.07.1")
	releaseSha := gitIn(b.src, "rev-parse", "HEAD")
	tag := "v2026.11.07.1"
	gitIn(b.src, "tag", "-a", tag, releaseSha, "-m", "lens 2026.11.07.1")

	f := newReleaseFixture(t, tag, releaseSha)
	// Sync the pinned test cosign.lock into the commit and tag so the checked-out tag has it too.
	write(t, filepath.Join(b.src, "scripts/cosign.lock"), readFile(t, filepath.Join(f.root, "scripts/cosign.lock")), 0o644)
	gitIn(b.src, "commit", "-q", "--amend", "-a", "-m", "release v2026.11.07.1")
	releaseSha = gitIn(b.src, "rev-parse", "HEAD")
	gitIn(b.src, "tag", "-f", "-a", tag, releaseSha, "-m", "lens 2026.11.07.1")
	f.update(func(f *releaseFixture) {
		f.commit = releaseSha
		f.binaryBody = []byte(fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n  --version) echo \"lens %s (%s)\" ;;\n  migrate-access-db) shift; [ -n \"${MIGRATE_LOG:-}\" ] && echo \"$*\" >> \"$MIGRATE_LOG\" ;;\nesac\n", strings.TrimPrefix(tag, "v"), releaseSha))
	})

	gitIn(b.src, "push", "-q", "--force", "origin", "HEAD:refs/heads/main", "refs/tags/"+tag)
	gitIn(b.src, "reset", "-q", "--hard", oldSha)
	gitIn(b.src, "tag", "-d", tag)
	// Also write the test cosign.lock into the pre-update checkout so manifest verification before checkout uses it.
	write(t, filepath.Join(b.src, "scripts/cosign.lock"), readFile(t, filepath.Join(f.root, "scripts/cosign.lock")), 0o644)
	gitIn(b.src, "commit", "-q", "--amend", "-a", "-m", "old release")
	oldSha = gitIn(b.src, "rev-parse", "HEAD")
	write(t, filepath.Join(b.app, ".deployed-revision"), oldSha+"\n", 0o644)

	// Poison go and make so any attempt to invoke them fails the test.
	stub(t, b.stubs, "go", "echo 'go must not be called in signed-release mode' >&2; exit 1")
	stub(t, b.stubs, "make", "echo 'make must not be called in signed-release mode' >&2; exit 1")

	// 1. Test the one-time re-exec from a pre-release update.sh (LENS_UPDATE_NO_PULL=1 + LENS_UPDATE_BASE_SHA set, LENS_FROM_SOURCE unset).
	envVars := append(f.env(),
		"LENS_FROM_SOURCE=0",
		"LENS_UPDATE_NO_PULL=1",
		"LENS_UPDATE_BASE_SHA="+oldSha,
	)
	r := b.run(t, envVars...)
	if r.code != 0 {
		t.Fatalf("signed release update (re-exec path) failed (%d):\nstdout=%s\nstderr=%s", r.code, r.stdout, r.stderr)
	}
	if strings.TrimSpace(readFile(t, filepath.Join(b.app, ".deployed-revision"))) != releaseSha {
		t.Fatalf(".deployed-revision = %q, want %q", readFile(t, filepath.Join(b.app, ".deployed-revision")), releaseSha)
	}
	if !strings.Contains(readFile(t, filepath.Join(b.app, "bin/lens")), releaseSha) {
		t.Fatalf("bin/lens was not updated to the signed release binary")
	}
	if !strings.Contains(readFile(t, b.cli), releaseSha) {
		t.Fatalf("CLI was not updated to the signed release binary")
	}

	// 2. Reset to oldSha and test a normal `lens update --yes` (LENS_UPDATE_NO_PULL=0, LENS_UPDATE_BASE_SHA="").
	gitIn(b.src, "reset", "-q", "--hard", oldSha)
	write(t, filepath.Join(b.app, ".deployed-revision"), oldSha+"\n", 0o644)
	envVars = append(f.env(),
		"LENS_FROM_SOURCE=0",
		"LENS_UPDATE_NO_PULL=0",
		"LENS_UPDATE_BASE_SHA=",
	)
	r = b.run(t, envVars...)
	if r.code != 0 {
		t.Fatalf("signed release update (normal path) failed (%d):\nstdout=%s\nstderr=%s", r.code, r.stdout, r.stderr)
	}
	if strings.TrimSpace(readFile(t, filepath.Join(b.app, ".deployed-revision"))) != releaseSha {
		t.Fatalf(".deployed-revision = %q, want %q", readFile(t, filepath.Join(b.app, ".deployed-revision")), releaseSha)
	}

	// 3. A tampered binary fails BEFORE stopping the live service.
	gitIn(b.src, "reset", "-q", "--hard", oldSha)
	write(t, filepath.Join(b.app, ".deployed-revision"), oldSha+"\n", 0o644)
	_ = os.Remove(b.systemctlLog)
	f.update(func(f *releaseFixture) { f.tamperBin = true })
	r = b.run(t, envVars...)
	f.update(func(f *releaseFixture) { f.tamperBin = false })
	if r.code == 0 {
		t.Fatal("update succeeded with a tampered release binary")
	}
	if exists(b.systemctlLog) && strings.Contains(readFile(t, b.systemctlLog), "stop") {
		t.Fatalf("service was stopped before binary verification failed:\n%s", readFile(t, b.systemctlLog))
	}
	// 4. An automatic update refuses a stale Latest marker that is older than the installed binary
	// (including a post-release source build 2026.11.07.1+2.g... ahead of v2026.11.07.1),
	// while an explicit LENS_UPDATE_TAG / --allow-downgrade permits rolling back.
	write(t, filepath.Join(b.app, "bin/lens"), "#!/bin/sh\necho \"lens 2026.11.07.1+2.gdeadbeef ("+oldSha+")\"\n", 0o755)
	r = b.run(t, envVars...)
	if r.code == 0 || !strings.Contains(r.stderr, "refusing automatic downgrade") {
		t.Fatalf("automatic downgrade from post-release source build was not refused: (%d, %s)", r.code, r.stderr)
	}
	write(t, filepath.Join(b.app, "bin/lens"), "#!/bin/sh\necho \"lens 2026.11.07.9 ("+oldSha+")\"\n", 0o755)
	r = b.run(t, envVars...)
	if r.code == 0 || !strings.Contains(r.stderr, "refusing automatic downgrade") {
		t.Fatalf("automatic downgrade was not refused: (%d, %s)", r.code, r.stderr)
	}
	r = b.run(t, append(envVars, "LENS_UPDATE_TAG="+tag)...)
	if r.code != 0 {
		t.Fatalf("explicit --tag rollback failed (%d):\nstdout=%s\nstderr=%s", r.code, r.stdout, r.stderr)
	}

	// 5. `lens rebuild` (LENS_UPDATE_NO_PULL=1, LENS_UPDATE_BASE_SHA="", LENS_FROM_SOURCE=0) on an untagged
	// commit automatically falls back to building from source instead of failing on a missing release tag.
	gitIn(b.src, "reset", "-q", "--hard", oldSha)
	_ = os.Remove(filepath.Join(b.stubs, "make"))
	stub(t, b.stubs, "go", "exit 0")
	r = b.run(t,
		"LENS_FROM_SOURCE=0",
		"LENS_UPDATE_NO_PULL=1",
		"LENS_UPDATE_BASE_SHA=",
	)
	if r.code != 0 {
		t.Fatalf("lens rebuild on untagged commit failed (%d):\nstdout=%s\nstderr=%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(readFile(t, filepath.Join(b.app, "bin/lens")), "lens test ("+oldSha+")") {
		t.Fatalf("lens rebuild on untagged commit did not fall back to source build: %s", readFile(t, filepath.Join(b.app, "bin/lens")))
	}
}

func TestReleaseWorkflowContract(t *testing.T) {
	wf := read(t, ".github/workflows/release.yml")
	for _, want := range []string{
		"workflow_run:",
		"workflow_dispatch:",
		"scripts/version.sh next",
		"scripts/version.sh released-at",
		"scripts/version.sh filter",
		"scripts/release-push-rejection.sh",
		"scripts/release-newer-published.sh",
		"scripts/retry-transient.sh",
		"scripts/cosign.lock",
		"CGO_ENABLED=0 GOOS=linux GOARCH=\"${ARCH}\"",
		"-X github.com/ElcanoTek/lens/internal/version.Version=${TAG#v}",
		"-X github.com/ElcanoTek/lens/internal/version.Commit=${HEAD_SHA}",
		"https://github.com/ElcanoTek/lens/.github/workflows/release.yml@refs/heads/main",
		"https://token.actions.githubusercontent.com",
		"cosign sign-blob --yes --bundle",
		"cosign verify-blob --bundle",
		"RETAIN=20",
	} {
		if !strings.Contains(wf, want) {
			t.Errorf(".github/workflows/release.yml missing %q", want)
		}
	}
	if strings.Contains(wf, "--cleanup-tag") {
		t.Fatal("release.yml must never delete git tags when pruning old releases")
	}
	// Every uses: line must pin a full 40-char commit SHA.
	usesRe := regexp.MustCompile(`(?m)^\s*-?\s*uses:\s*([^\s#]+)`)
	shaRe := regexp.MustCompile(`^[^@]+@[0-9a-f]{40}$`)
	for _, m := range usesRe.FindAllStringSubmatch(wf, -1) {
		if !shaRe.MatchString(m[1]) {
			t.Errorf("unpinned action in release.yml: %s", m[1])
		}
	}
}
