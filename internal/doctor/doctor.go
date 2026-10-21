// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package doctor implements `lens doctor`: read-only, bounded deployment
// diagnostics for a Lens box.
//
// Ground rules, carried over from scripts/doctor.py:
//
//   - Read-only. Doctor never installs, restarts, pulls or fetches anything;
//     every remedy is printed for the operator to run.
//   - Bounded. Every external command runs under a timeout, and a timeout
//     counts as a failure rather than hanging a monitoring job.
//   - No pipeline imports. A broken release must still be diagnosable, so
//     doctor depends only on the dotenv parser, config defaults and the
//     embedded asset set.
//   - No secret values. .env is parsed (never sourced as shell), and only key
//     names and booleans leave this package.
//
// Every host dependency (command runner, PATH lookup, euid, account lookup,
// stat, filesystem root for fixed host paths) is injectable through Host so
// the checks are testable without root, systemd or podman.
package doctor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	lens "github.com/ElcanoTek/lens"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/dotenv"
)

// Check statuses as they appear in the text and JSON reports.
const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusFail = "fail"
)

// defaultCommandTimeout bounds every external command unless a check needs
// longer (podman info starts the service user's pause process).
const defaultCommandTimeout = 15 * time.Second

// Check is one diagnostic result. Detail is the success detail for an ok
// check and the remedy otherwise, exactly like the Python report.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Options says which install to diagnose.
type Options struct {
	App       string // the Lens home, /opt/lens
	Src       string // the git checkout updates build from, /opt/lens-src
	User      string // the service account, lens
	PublicURL string // optional https:// origin to probe /health on
}

// Diagnose runs every check and returns them in report order.
func Diagnose(ctx context.Context, opts Options, h Host) []Check {
	h = h.withDefaults()
	d := &diagnosis{ctx: ctx, opts: opts, h: h}
	d.tools()
	d.binary()
	d.assets()
	d.pythonLeftovers()
	env := d.configuration()
	d.authDatabases(env)
	d.service()
	d.release()
	d.disk()
	d.osSupport()
	d.podman()
	d.firecrawl()
	d.sshd()
	d.cockpit()
	d.caddy()
	d.publicTLS()
	d.managedFiles()
	d.jobs()
	d.optionalTools()
	return d.checks
}

type diagnosis struct {
	ctx    context.Context
	opts   Options
	h      Host
	checks []Check
}

// add records a check: detail when good, remedy otherwise; a bad check is a
// failure unless warning is set.
func (d *diagnosis) add(name string, good bool, detail, remedy string, warning bool) {
	c := Check{Name: name, Status: StatusOK, Detail: detail}
	if !good {
		c.Detail = remedy
		c.Status = StatusFail
		if warning {
			c.Status = StatusWarn
		}
	}
	d.checks = append(d.checks, c)
}

func (d *diagnosis) run(name string, args ...string) (int, string) {
	return d.h.Run(d.ctx, RunOptions{Timeout: defaultCommandTimeout}, name, args...)
}

func (d *diagnosis) app(parts ...string) string {
	return filepath.Join(append([]string{d.opts.App}, parts...)...)
}

func (d *diagnosis) tools() {
	// make is what update.sh builds with; go is checked separately because
	// update.sh installs it on demand.
	for _, name := range []string{"git", "curl", "rsync", "make", "dnf", "systemctl"} {
		d.add(name, d.h.LookPath(name), "installed", "Run bootstrap to install "+name, false)
	}
	if !d.h.LookPath("go") {
		d.add("go-toolchain", false, "",
			"No Go toolchain; lens update installs golang via dnf (or: lens host tools)", true)
		return
	}
	// Run outside any module so GOTOOLCHAIN cannot trigger a download.
	code, out := d.h.Run(d.ctx, RunOptions{Timeout: defaultCommandTimeout, Dir: "/"}, "go", "version")
	d.add("go-toolchain", code == 0, out,
		"Go toolchain is broken; lens update needs it (lens host tools reinstalls golang)", true)
}

func (d *diagnosis) deployedRevision() string {
	raw, err := os.ReadFile(d.app(".deployed-revision"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// binary replaces the Python release's interpreter + `uv pip check` checks:
// the release is the binary, so it must exist, run, and identify itself as
// the revision update.sh stamped into .deployed-revision.
func (d *diagnosis) binary() {
	bin := d.app("bin", "lens")
	fi, err := d.h.Stat(bin)
	if err != nil || fi.IsDir || fi.Mode&0o111 == 0 {
		d.add("binary", false, "", "Run lens rebuild; "+bin+" is missing or not executable", false)
		return
	}
	code, out := d.run(bin, "--version")
	deployed := d.deployedRevision()
	good := code == 0 && deployed != "" && strings.Contains(out, deployed)
	remedy := "Run lens rebuild; " + bin + " does not run"
	if code == 0 {
		remedy = fmt.Sprintf("Run lens rebuild; bin/lens reports %q but .deployed-revision is %q",
			firstLine(out), short(deployed))
	}
	d.add("binary", good, firstLine(out), remedy, false)
}

// assets proves this binary carries the dashboard it serves. A build from a
// tree with a missing static/ or templates/ compiles fine and only fails when
// a page renders, so look before an operator does.
func (d *diagnosis) assets() {
	var missing []string
	for _, p := range []string{"templates/index.html", "templates/signed_out.html"} {
		if _, err := fs.Stat(lens.Templates, p); err != nil {
			missing = append(missing, p)
		}
	}
	for _, p := range []string{"static/main.js", "static/styles.css"} {
		if _, err := fs.Stat(lens.Static, p); err != nil {
			missing = append(missing, p)
		}
	}
	if len(lens.ContentTaxonomy) == 0 {
		missing = append(missing, "content_taxonomy.tsv")
	}
	if len(lens.IANATLDs) == 0 {
		missing = append(missing, "iana_tlds.txt")
	}
	d.add("assets", len(missing) == 0, "embedded dashboard assets present",
		"This lens binary lacks embedded assets ("+strings.Join(missing, ", ")+"); run lens rebuild", false)
}

// pythonLeftovers flags the venv a Python release left behind. update.sh
// retires it after the first healthy Go start, so one that survives means the
// migration never completed (or a bootstrap re-run predates the Go release).
func (d *diagnosis) pythonLeftovers() {
	var found []string
	for _, name := range []string{".venv", ".venv.old"} {
		if fi, err := d.h.Stat(d.app(name)); err == nil && fi.IsDir {
			found = append(found, d.app(name))
		}
	}
	d.add("python-venv", len(found) == 0, "no retired Python virtualenv",
		"Retired Python virtualenv still present ("+strings.Join(found, ", ")+
			"); the next successful lens update or lens rebuild removes it", true)
}

// envReport is what doctor learns from .env: key names and booleans only.
type envReport struct {
	Missing   []string
	KeyOK     bool
	ModeOK    bool
	SessionOK bool
	Central   bool
	AccessDB  string
	TokenDB   string
}

// inspectEnv parses path without executing it and reports only key names and
// booleans; values never leave this function except the two database paths,
// which are locations, not secrets.
func inspectEnv(path string) (*envReport, error) {
	pairs, err := dotenv.ReadFile(path)
	if err != nil {
		return nil, err
	}
	v := map[string]string{}
	for _, p := range pairs {
		if p.Value != nil {
			v[p.Key] = *p.Value
		}
	}
	mode, set := v["LENS_AUTH_MODE"]
	if !set {
		mode = "elcano"
	}
	// The service normalises the mode the same way (auth_provider.py), so a
	// "Central" that works must not be reported as invalid.
	mode = strings.ToLower(strings.TrimSpace(mode))
	required := []string{"OPENROUTER_API_KEY", "AUTH_SIGNING_PUBKEY"}
	if mode == "central" {
		required = append(required, "AUTH_ISSUER_URL", "LENS_PUBLIC_URL", "AUTH_CLIENT_ID",
			"AUTH_CLIENT_SECRET", "LENS_SESSION_SECRET")
	}
	r := &envReport{Missing: []string{}}
	for _, k := range required {
		if v[k] == "" {
			r.Missing = append(r.Missing, k)
		}
	}
	key, err := base64.StdEncoding.Strict().DecodeString(v["AUTH_SIGNING_PUBKEY"])
	r.KeyOK = err == nil && len(key) == 32
	r.ModeOK = mode == "elcano" || mode == "central"
	r.Central = mode == "central"
	r.SessionOK = mode != "central" || len(v["LENS_SESSION_SECRET"]) >= 32
	r.AccessDB = v["LENS_ACCESS_DB"]
	r.TokenDB = v["LENS_API_TOKEN_DB"]
	return r, nil
}

// ServiceUserCanRead reports whether acct can open a file with fi's owner,
// group and mode, using the owner → group → other precedence the kernel
// applies.
//
// Doctor runs as root, so a mode of 0600 is not enough. systemd reads
// EnvironmentFile as root and the dashboard stays up, while each job
// subprocess runs as the service user and opens .env itself.
func ServiceUserCanRead(fi FileInfo, acct Account) bool {
	if fi.UID == acct.UID {
		return fi.Mode&0o400 != 0
	}
	if fi.GID == acct.GID || containsInt(acct.Groups, fi.GID) {
		return fi.Mode&0o040 != 0
	}
	return fi.Mode&0o004 != 0
}

func (d *diagnosis) configuration() *envReport {
	envFile := d.app(".env")
	fi, err := d.h.Stat(envFile)
	if err != nil {
		d.add("configuration", false, "", "Cannot read configuration; run doctor with sudo or restore .env", false)
		return nil
	}
	perm := fi.Mode.Perm()
	d.add("env-permissions", perm == 0o600 || perm == 0o640, "private", "chmod 600 "+envFile, false)
	// Mode 0600 owned by root still looks "private" and still crashes every
	// job: the web process gets the variables from EnvironmentFile, and the
	// analyzer opens the file as the service user.
	acct, aerr := d.h.LookupUser(d.opts.User)
	d.add("env-readable", aerr == nil && ServiceUserCanRead(fi, acct),
		d.opts.User+" can read .env",
		fmt.Sprintf("chown %[1]s:%[1]s %[2]s && chmod 600 %[2]s. "+
			"Jobs run as the service user and load .env themselves. "+
			"A root-owned 0600 file passes the mode check and crashes every run.", d.opts.User, envFile), false)
	env, err := inspectEnv(envFile)
	if err != nil {
		d.add("configuration", false, "", "Cannot read configuration; run doctor with sudo or restore .env", false)
		return nil
	}
	good := len(env.Missing) == 0 && env.KeyOK && env.ModeOK && env.SessionOK
	remedy := "lens env edit: check auth mode, signing key, session secret and required credentials"
	if len(env.Missing) > 0 {
		remedy += "; missing: " + strings.Join(env.Missing, ", ")
	}
	d.add("configuration", good, "auth and API credentials configured", remedy, false)
	return env
}

// authDatabases checks the SQLite files under /var/lib/lens that outlive
// every release: central auth's allowlist + sessions, and the API tokens.
// Both hold credentials, so they must belong to the service user and stay
// private to it.
func (d *diagnosis) authDatabases(env *envReport) {
	acct, err := d.h.LookupUser(d.opts.User)
	if err != nil {
		d.add("service-account", false, "", "Lens service account "+d.opts.User+" missing; rerun bootstrap", false)
		return
	}
	accessDB, tokenDB := d.h.path("/var/lib/lens/access.db"), d.h.path("/var/lib/lens/api_tokens.db")
	central := false
	if env != nil {
		central = env.Central
		if env.AccessDB != "" {
			accessDB = env.AccessDB
		}
		if env.TokenDB != "" {
			tokenDB = env.TokenDB
		}
	}
	fi, err := d.h.Stat(accessDB)
	switch {
	case err != nil && central:
		d.add("access-db", false, "",
			"Central-auth access database missing at "+accessDB+"; lens update (migrate-access-db) creates it", false)
	case err != nil:
		d.add("access-db", true, "not used (elcano mode)", "", false)
	default:
		d.dbPerms("access-db", accessDB, fi, acct)
	}
	fi, err = d.h.Stat(tokenDB)
	if err != nil {
		d.add("token-db", true, "no API tokens minted yet", "", false)
		return
	}
	d.dbPerms("token-db", tokenDB, fi, acct)
}

func (d *diagnosis) dbPerms(name, path string, fi FileInfo, acct Account) {
	if fi.UID != acct.UID || fi.Mode&0o600 != 0o600 {
		d.add(name, false, "", fmt.Sprintf("chown %[1]s:%[1]s %[2]s && chmod 600 %[2]s; the service must read and write it", d.opts.User, path), false)
		return
	}
	d.add(name, fi.Mode&0o077 == 0, "owned by "+d.opts.User+", private",
		"chmod 600 "+path+"; it holds credentials and is readable by other accounts", true)
}

func (d *diagnosis) service() {
	code, _ := d.run("systemctl", "is-active", "--quiet", "lens.service")
	d.add("service", code == 0, "lens.service active", "Inspect lens logs; then lens restart", false)
	code, status := d.run("curl", "-sS", "--connect-timeout", "2", "--max-time", "5",
		"-o", "/dev/null", "-w", "%{http_code}", "http://127.0.0.1:8808/health")
	// Only 200: a 302 to a login page is a live proxy, not a ready Lens.
	d.add("readiness", code == 0 && status == "200", "/health returns 200", "Readiness failed; inspect lens logs", false)
	code, pid := d.run("systemctl", "show", "--property=MainPID", "--value", "lens.service")
	if n, err := strconv.Atoi(pid); code == 0 && err == nil && n > 0 {
		same := d.h.SameFile(d.h.path("/proc/"+pid+"/exe"), d.app("bin", "lens"))
		d.add("running-binary", same, "running process is the installed bin/lens",
			"Run lens restart; the running process is not the installed bin/lens (stale after an update, or still the Python release)", false)
	}
}

func (d *diagnosis) release() {
	src := d.opts.Src
	git := []string{"-c", "safe.directory=" + src, "-C", src}
	code, head := d.run("git", append(git, "rev-parse", "HEAD")...)
	deployed := d.deployedRevision()
	d.add("release", code == 0 && head == deployed, "deployed "+short(head),
		"Run lens rebuild; source and deployed revision differ or stamp is missing", false)
	code, dirty := d.run("git", append(git, "status", "--porcelain")...)
	d.add("checkout", code == 0 && dirty == "", "clean", "Resolve local source changes before lens update", false)
}

func (d *diagnosis) disk() {
	free, err := d.h.DiskFree(d.opts.App)
	if err != nil {
		d.add("disk", false, "", "Application directory is missing or inaccessible", false)
		return
	}
	d.add("disk", free >= 2<<30, fmt.Sprintf("%.1f GiB available", float64(free)/(1<<30)),
		"Less than 2 GiB free; inspect run outputs and Podman storage", false)
}

func (d *diagnosis) osSupport() {
	release, err := readOSRelease(d.h.path("/etc/os-release"), d.h.path("/usr/lib/os-release"))
	if err != nil {
		d.add("os-support", false, "", "Cannot read OS lifecycle metadata", true)
		return
	}
	end := release["SUPPORT_END"]
	if end == "" {
		d.add("os-support", false, "", "OS does not publish SUPPORT_END; verify vendor lifecycle", true)
		return
	}
	date, err := time.Parse("2006-01-02", end)
	if err != nil {
		d.add("os-support", false, "", "Cannot read OS lifecycle metadata", true)
		return
	}
	now := d.h.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	days := int(date.Sub(today).Hours() / 24)
	d.add("os-support", days >= 30, "support ends "+end, "OS support ends "+end+"; plan lens host upgrade", true)
}

// podman probes rootless Podman exactly as a deep scrape uses it: as the
// service user, with its HOME and runtime dir, from a directory it can read
// (runuser keeps the caller's cwd, and root's home is unreadable to lens).
func (d *diagnosis) podman() {
	if !d.h.LookPath("podman") {
		d.add("podman", false, "", "Optional container scraping unavailable; install Podman with bootstrap", true)
		return
	}
	acct, err := d.h.LookupUser(d.opts.User)
	if err != nil {
		d.add("podman", false, "", "Lens service account missing; rerun bootstrap", false)
		return
	}
	podman := func(timeout time.Duration, args ...string) int {
		var argv []string
		if d.h.Geteuid() == 0 {
			argv = append(argv, "runuser", "-u", d.opts.User, "--")
		}
		argv = append(argv, "env", "HOME="+d.opts.App, "XDG_RUNTIME_DIR=/run/user/"+strconv.Itoa(acct.UID), "podman")
		argv = append(argv, args...)
		code, _ := d.h.Run(d.ctx, RunOptions{Timeout: timeout, Dir: d.opts.App}, argv[0], argv[1:]...)
		return code
	}
	works := podman(20*time.Second, "info") == 0
	d.add("podman", works, "service-user rootless Podman works",
		"Check subuid/subgid, lingering and lens user's Podman; rerun bootstrap", true)

	// The prerequisites update.sh heals: subordinate id ranges (image unpack
	// fails with "insufficient UIDs or GIDs" without them) and lingering
	// (keeps /run/user/<uid>, podman's runtime root, alive for the service).
	var missing []string
	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		if !hasUserEntry(d.h.path(f), d.opts.User) {
			missing = append(missing, f)
		}
	}
	if _, err := d.h.Stat(d.h.path("/var/lib/systemd/linger/" + d.opts.User)); err != nil {
		missing = append(missing, "lingering")
	}
	d.add("podman-rootless", len(missing) == 0, "subuid/subgid ranges and lingering configured",
		"Rootless Podman prerequisites missing ("+strings.Join(missing, ", ")+"); lens update adds them", true)
	if !works {
		return
	}
	image := strings.TrimSpace(os.Getenv("LENS_DEEP_SCRAPE_IMAGE"))
	if image == "" {
		image = config.Load(d.app("config.json")).DeepScrapeImage
	}
	d.add("chrome-image", podman(defaultCommandTimeout, "image", "exists", image) == 0,
		image+" present",
		image+" is not pre-pulled; the first deep scrape downloads it (~1 GB). lens update pre-pulls it", true)
}

func (d *diagnosis) firecrawl() {
	code, load := d.run("systemctl", "show", "--property=LoadState", "--value", "firecrawl.service")
	if code == 0 && load == "loaded" {
		d.add("firecrawl", false, "", "Firecrawl was removed from Lens; run `lens update` to retire the old stack", true)
	}
}

func (d *diagnosis) sshd() {
	if !d.h.LookPath("sshd") || d.h.Geteuid() != 0 {
		return
	}
	code, effective := d.run("sshd", "-T")
	if code != 0 {
		return
	}
	accepts := false
	for _, line := range strings.Split(effective, "\n") {
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "passwordauthentication yes", "permitrootlogin yes":
			accepts = true
		}
	}
	d.add("sshd-passwords", !accepts, "key-only",
		"sshd accepts passwords — see production-support README, Access: SSH keys", true)
}

// cockpit flags the root-capable web console the Vultr image ships on :9090;
// bootstrap removes it, so one that is back was reinstalled by hand.
func (d *diagnosis) cockpit() {
	if !d.h.LookPath("rpm") {
		return
	}
	code, _ := d.run("rpm", "-q", "cockpit-ws")
	d.add("cockpit", code != 0, "not installed",
		"Cockpit's web console (:9090) is installed; remove it: dnf -y remove 'cockpit*'", true)
}

func (d *diagnosis) caddy() {
	if !d.h.LookPath("caddy") {
		return
	}
	code, _ := d.run("systemctl", "is-active", "--quiet", "caddy.service")
	d.add("caddy", code == 0, "caddy.service active", "Caddy is installed but not running; lens tls restart, then journalctl -u caddy", true)
}

func (d *diagnosis) publicTLS() {
	if d.opts.PublicURL == "" {
		return
	}
	code, status := d.run("curl", "-fsS", "--connect-timeout", "3", "--max-time", "10",
		"-o", "/dev/null", "-w", "%{http_code}", strings.TrimRight(d.opts.PublicURL, "/")+"/health")
	d.add("public-tls", code == 0 && status == "200", "HTTPS readiness and certificate valid", "Check DNS, Caddy and public TLS", false)
}

// managedFiles checks the upload/output/spool tree the service writes. A
// root-owned directory (an operator's manual copy) makes every upload or job
// fail while the dashboard itself looks healthy.
func (d *diagnosis) managedFiles() {
	acct, err := d.h.LookupUser(d.opts.User)
	if err != nil {
		return
	}
	var missing, foreign []string
	for _, sub := range []string{"", "inputs", "outputs", "spool"} {
		p := d.app("managed-files", sub)
		fi, err := d.h.Stat(p)
		switch {
		case err != nil:
			missing = append(missing, p)
		case !fi.IsDir || fi.UID != acct.UID:
			foreign = append(foreign, p)
		}
	}
	if len(foreign) > 0 {
		d.add("managed-files", false, "",
			fmt.Sprintf("chown -R %[1]s:%[1]s %[2]s; not owned by %[1]s: %[3]s", d.opts.User, d.app("managed-files"), strings.Join(foreign, ", ")), false)
		return
	}
	d.add("managed-files", len(missing) == 0, "inputs, outputs and spool owned by "+d.opts.User,
		"Missing "+strings.Join(missing, ", ")+"; the service creates them on start (lens restart)", true)
}

// jobs reports the dashboard queue, the idle check before an update or
// restart (production-support: no running/queued jobs in _jobs.json). Active
// jobs are not a fault, so this stays ok and says what is in flight.
func (d *diagnosis) jobs() {
	raw, err := os.ReadFile(d.app("managed-files", "outputs", "_jobs.json"))
	if errors.Is(err, fs.ErrNotExist) {
		d.add("jobs", true, "idle (no job state yet)", "", false)
		return
	}
	var state struct {
		Jobs []struct {
			Status string `json:"status"`
		} `json:"jobs"`
	}
	if err != nil || json.Unmarshal(raw, &state) != nil {
		d.add("jobs", false, "", "managed-files/outputs/_jobs.json is unreadable or corrupt; the dashboard will start with an empty queue", true)
		return
	}
	counts := map[string]int{}
	for _, j := range state.Jobs {
		s := strings.ToLower(strings.TrimSpace(j.Status))
		if s == "" {
			s = "queued"
		}
		counts[s]++
	}
	active := counts["running"] + counts["cancelling"] + counts["queued"]
	detail := "idle (no running or queued jobs)"
	if active > 0 {
		detail = fmt.Sprintf("busy: %d running, %d queued — let them finish before lens update or restart",
			counts["running"]+counts["cancelling"], counts["queued"])
	}
	d.add("jobs", true, detail, "", false)
}

func (d *diagnosis) optionalTools() {
	for _, name := range []string{"node", "npm"} {
		if d.h.LookPath(name) {
			code, out := d.run(name, "--version")
			d.add(name, code == 0, out, "Optional "+name+" is broken; lens host tools", true)
		}
	}
}

// Main is `lens doctor`. It returns 1 when any check fails (or warns, with
// --strict), 2 on a usage error.
func Main(args []string) int {
	return run(context.Background(), args, os.Stdout, os.Stderr, os.Getenv, DefaultHost(), Diagnose)
}

// run is Main with every dependency injected; diagnose is Diagnose outside
// tests.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string, h Host,
	diagnose func(context.Context, Options, Host) []Check) int {
	fset := flag.NewFlagSet("lens doctor", flag.ContinueOnError)
	fset.SetOutput(stderr)
	asJSON := fset.Bool("json", false, "machine-readable checks")
	fset.Bool("check", false, "read-only (the default)")
	strict := fset.Bool("strict", false, "warnings also exit 1")
	publicURL := fset.String("public-url", "", "also check HTTPS /health")
	fset.Usage = func() {
		fmt.Fprintln(stderr, "usage: lens doctor [--json] [--check] [--strict] [--public-url https://HOST]")
		fmt.Fprintln(stderr, "Read-only, bounded deployment diagnostics; never prints secret values.")
		fset.PrintDefaults()
	}
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fset.NArg() > 0 {
		fmt.Fprintf(stderr, "lens doctor: unexpected argument %q\n", fset.Arg(0))
		return 2
	}
	if *publicURL != "" && !strings.HasPrefix(*publicURL, "https://") {
		fmt.Fprintln(stderr, "lens doctor: --public-url must use https://")
		return 2
	}
	opts := Options{
		App:       firstNonEmpty(getenv("LENS_APP_DIR"), "/opt/lens"),
		Src:       firstNonEmpty(getenv("LENS_SRC_DIR"), "/opt/lens-src"),
		User:      firstNonEmpty(getenv("APP_USER"), getenv("LENS_APP_USER"), "lens"),
		PublicURL: *publicURL,
	}
	checks := diagnose(ctx, opts, h)
	failed := false
	for _, c := range checks {
		if c.Status == StatusFail || (*strict && c.Status == StatusWarn) {
			failed = true
		}
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			OK     bool    `json:"ok"`
			Checks []Check `json:"checks"`
		}{!failed, checks})
	} else {
		for _, c := range checks {
			fmt.Fprintf(stdout, "%-4s %s: %s\n", strings.ToUpper(c.Status), c.Name, c.Detail)
		}
		fmt.Fprintln(stdout, "\nHost packages: lens host check | lens host update. Doctor changes nothing.")
	}
	if failed {
		return 1
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func short(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// hasUserEntry reports whether an /etc/subuid-style file has a line for user.
func hasUserEntry(path, user string) bool {
	raw, err := os.ReadFile(path) //nolint:gosec // fixed host path under the injectable root
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, user+":") {
			return true
		}
	}
	return false
}

// readOSRelease parses the first readable os-release file (freedesktop
// format: KEY=value, optionally quoted, # comments).
func readOSRelease(paths ...string) (map[string]string, error) {
	var lastErr error
	for _, p := range paths {
		raw, err := os.ReadFile(p) //nolint:gosec // fixed host path under the injectable root
		if err != nil {
			lastErr = err
			continue
		}
		out := map[string]string{}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			if uq, err := strconv.Unquote(v); err == nil {
				v = uq
			} else {
				v = strings.Trim(v, `"'`)
			}
			out[k] = v
		}
		return out, nil
	}
	return nil, lastErr
}
