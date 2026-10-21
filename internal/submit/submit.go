// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package submit is `lens submit FILE [options]`: queue a dashboard job from
// a file on this host. The running service picks the request up from the
// spool (managed-files/spool), so the run appears in the Runs table with
// live progress, cost and downloads, exactly like a browser upload.
//
// It runs in two halves, as the Python release's deploy/lens-cli and
// submit_job.py did:
//
//  1. Operator side (root, re-run under sudo when needed): make a fresh spool
//     entry (mktemp -d semantics), copy the input and any categories file
//     into it with mode 0640, chown it to the service user.
//  2. Client side, re-executed as the service user (never as root):
//     validate the categories, write request.json, wait for the service's
//     result.json, print the job id and the dashboard URL
//     (LENS_PUBLIC_URL), clean up.
//
// Exit codes: 0 queued, 1 rejected or invalid input, 2 usage error or the
// service did not answer in time (the request stays spooled and is queued
// when the service picks it up).
package submit

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ElcanoTek/lens/internal/categories"
	"github.com/ElcanoTek/lens/internal/opscli"
)

const usage = "usage: lens submit FILE [--name NAME] [--categories JSON] [--ctv] [--decision-model M]\n" +
	"                        [--llm-model M] [--research-model M] [--no-research-fallback] [--no-deep]\n"

var (
	modelRe = regexp.MustCompile(`^[~A-Za-z0-9/._:-]+$`)
	// Same character allowlist the dashboard applies to uploads.
	unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._ ()-]`)
)

// Runner holds submit's side effects; Main uses the real machine and tests
// replace fields.
type Runner struct {
	Geteuid    func() int
	LookupUser func(name string) (uid, gid int, err error)
	Chown      func(path string, uid, gid int) error
	Executable func() (string, error)
	// Exec replaces the process with argv (sudo re-run); it returns only on
	// failure.
	Exec func(argv, env []string) error
	// AsServiceUser re-executes `lens <args...>` as the service user; see
	// opscli.AsServiceUser.
	AsServiceUser func(args []string) (done bool, code int)
	AppDir        string
	ServiceUser   string
	Sleep         func(time.Duration)
	Stdout        io.Writer
	Stderr        io.Writer
}

// NewRunner returns a Runner wired to the real machine.
func NewRunner() *Runner {
	return &Runner{
		Geteuid: os.Geteuid, LookupUser: lookupUser, Chown: os.Chown, Executable: os.Executable,
		Exec: execReplace, AsServiceUser: opscli.AsServiceUser,
		AppDir: opscli.AppDir(), ServiceUser: opscli.ServiceUser(),
		Sleep: time.Sleep, Stdout: os.Stdout, Stderr: os.Stderr,
	}
}

// Main runs `lens submit` with args (the arguments after "submit") and
// returns the exit status.
func Main(args []string) int { return NewRunner().Main(args) }

// options are the flags both halves share.
type options struct {
	name, categories, decisionModel, llmModel, researchModel string
	noResearchFallback, noDeep, ctv                          bool
	timeout                                                  float64
	// client side only
	entry, input string
}

func modelFlag(fs *flag.FlagSet, target *string, name, help string) {
	fs.Func(name, help, func(v string) error {
		if !modelRe.MatchString(v) {
			return fmt.Errorf("invalid model id: %s", v)
		}
		*target = v
		return nil
	})
}

func parseFlags(args []string, stderr io.Writer, client bool) (*options, []string, error) {
	o := &options{timeout: 30}
	fs := flag.NewFlagSet("lens submit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	fs.StringVar(&o.name, "name", "", "run name shown in the dashboard")
	fs.StringVar(&o.categories, "categories", "", "custom categories JSON file")
	modelFlag(fs, &o.decisionModel, "decision-model", "decision model for custom categories")
	modelFlag(fs, &o.llmModel, "llm-model", "classification model override")
	modelFlag(fs, &o.researchModel, "research-model", "research model override")
	fs.BoolVar(&o.noResearchFallback, "no-research-fallback", false, "turn the research rung off")
	fs.BoolVar(&o.noDeep, "no-deep", false, "skip the headless-Chrome retry; research failures directly")
	fs.BoolVar(&o.ctv, "ctv", false, "force the CTV workflow")
	fs.Float64Var(&o.timeout, "timeout", 30, "seconds to wait for the service")
	if client {
		fs.StringVar(&o.entry, "entry", "", "spool entry (internal)")
		fs.StringVar(&o.input, "input", "", "input file name in the entry (internal)")
	}
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	return o, fs.Args(), nil
}

// forward renders the shared flags for the client re-run.
func (o *options) forward() []string {
	var out []string
	for _, f := range []struct{ flag, value string }{
		{"--name", o.name}, {"--categories", o.categories}, {"--decision-model", o.decisionModel},
		{"--llm-model", o.llmModel}, {"--research-model", o.researchModel},
	} {
		if f.value != "" {
			out = append(out, f.flag, f.value)
		}
	}
	for _, f := range []struct {
		flag string
		on   bool
	}{{"--no-research-fallback", o.noResearchFallback}, {"--no-deep", o.noDeep}, {"--ctv", o.ctv}} {
		if f.on {
			out = append(out, f.flag)
		}
	}
	return append(out, fmt.Sprintf("--timeout=%g", o.timeout))
}

// Main is the method form of the package-level Main.
func (r *Runner) Main(args []string) int {
	if len(args) > 0 && (args[0] == "--entry" || strings.HasPrefix(args[0], "--entry=")) {
		return r.client(args)
	}
	return r.operator(args)
}

func isRegular(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func (r *Runner) operator(args []string) int {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(r.Stderr, usage)
		return 2
	}
	file := args[0]
	if !isRegular(file) {
		fmt.Fprintf(r.Stderr, "lens submit: no such file: %s\n", file)
		return 1
	}
	// Every option is checked before a spool entry exists, so a typo never
	// leaves an orphaned entry behind.
	opts, rest, err := parseFlags(args[1:], r.Stderr, false)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if len(rest) > 0 {
		fmt.Fprintf(r.Stderr, "lens submit: unexpected argument: %s\n%s", rest[0], usage)
		return 2
	}
	if opts.categories != "" && !isRegular(opts.categories) {
		fmt.Fprintf(r.Stderr, "lens submit: no such categories file: %s\n", opts.categories)
		return 1
	}

	uid, gid, err := r.LookupUser(r.ServiceUser)
	if err != nil {
		fmt.Fprintf(r.Stderr, "lens: service account %q missing — run bootstrap.sh\n", r.ServiceUser)
		return 1
	}
	euid := r.Geteuid()
	if euid != 0 && euid != uid {
		return r.sudo(file, opts)
	}

	spool := filepath.Join(r.AppDir, "managed-files", "spool")
	if st, err := os.Stat(spool); err != nil || !st.IsDir() {
		fmt.Fprintf(r.Stderr, "lens submit: %s missing; run lens update so the service creates it\n", spool)
		return 1
	}
	// MkdirTemp creates the entry 0700 with a random suffix, as mktemp -d.
	entry, err := os.MkdirTemp(spool, "submit-")
	if err != nil {
		fmt.Fprintf(r.Stderr, "lens submit: %v\n", err)
		return 1
	}
	fail := func(err error) int {
		_ = os.RemoveAll(entry)
		fmt.Fprintf(r.Stderr, "lens submit: %v\n", err)
		return 1
	}
	if opts.categories != "" {
		dst := filepath.Join(entry, "categories.json")
		if err := install(opts.categories, dst); err != nil {
			return fail(err)
		}
		opts.categories = dst
	}
	name := filepath.Base(file)
	if err := install(file, filepath.Join(entry, name)); err != nil {
		return fail(err)
	}
	if euid == 0 {
		if err := chownTree(r.Chown, entry, uid, gid); err != nil {
			return fail(err)
		}
	}
	if url, ok := envFileValue(filepath.Join(r.AppDir, ".env"), "LENS_PUBLIC_URL"); ok {
		// Forwarded to the service-user re-run (opscli passes it through).
		_ = os.Setenv("LENS_PUBLIC_URL", url)
	}
	clientArgs := append([]string{"--entry", entry, "--input", name}, opts.forward()...)
	if euid == uid {
		return r.client(clientArgs)
	}
	// Validation and the hand-off run as the service user, never as root.
	done, code := r.AsServiceUser(append([]string{"submit"}, clientArgs...))
	if !done {
		return r.client(clientArgs)
	}
	// In production AsServiceUser only returns after a failed exec.
	if code != 0 {
		_ = os.RemoveAll(entry)
	}
	return code
}

// sudo re-runs the whole command as root, as the bash CLI ran its spool
// steps through sudo. Paths are made absolute first.
func (r *Runner) sudo(file string, opts *options) int {
	exe, err := r.Executable()
	if err != nil {
		fmt.Fprintf(r.Stderr, "lens: cannot locate this binary: %v\n", err)
		return 1
	}
	if abs, err := filepath.Abs(file); err == nil {
		file = abs
	}
	if opts.categories != "" {
		if abs, err := filepath.Abs(opts.categories); err == nil {
			opts.categories = abs
		}
	}
	argv := append([]string{"sudo", "--preserve-env=LENS_APP_DIR,LENS_APP_USER,LENS_HOME", exe, "submit", file}, opts.forward()...)
	if err := r.Exec(argv, os.Environ()); err != nil {
		fmt.Fprintf(r.Stderr, "lens: sudo: %v\n", err)
		return 127
	}
	return 0
}

// install copies src to dst with mode 0640 (install -m 0640).
func install(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // operator-chosen input file
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640) //nolint:gosec // inside a fresh spool entry
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, 0o640) //nolint:gosec // install -m 0640: the service user reads it; umask must not narrow it
}

func chownTree(chown func(string, int, int) error, root string, uid, gid int) error {
	return filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return chown(path, uid, gid)
	})
}

// envFileValue reads KEY=value from a .env the way the bash CLI's awk did:
// everything after the first "=", quotes removed.
func envFileValue(path, key string) (string, bool) {
	raw, err := os.ReadFile(path) //nolint:gosec // the instance's .env
	if err != nil {
		return "", false
	}
	value, found := "", false
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "=")
		if ok && k == key {
			value, found = strings.NewReplacer(`"`, "", "'", "").Replace(v), true
		}
	}
	return value, found
}

// request is the spool request.json, in the Python release's key order.
type request struct {
	InputFile        string          `json:"input_file"`
	Mode             string          `json:"mode"`
	Name             *string         `json:"name"`
	LLMModel         *string         `json:"llm_model"`
	ResearchModel    *string         `json:"research_model"`
	DecisionModel    *string         `json:"decision_model"`
	ResearchFallback bool            `json:"research_fallback"`
	DeepRetry        bool            `json:"deep_retry"`
	CustomCategories json.RawMessage `json:"custom_categories,omitempty"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// client is the service-user half (submit_job.py).
func (r *Runner) client(args []string) int {
	opts, rest, err := parseFlags(args, r.Stderr, true)
	if err != nil {
		return 2
	}
	if opts.entry == "" || opts.input == "" || len(rest) > 0 {
		fmt.Fprint(r.Stderr, usage)
		return 2
	}
	entry := opts.entry
	clean := strings.TrimSpace(unsafeChars.ReplaceAllString(filepath.Base(opts.input), "_"))
	if clean != opts.input {
		if err := os.Rename(filepath.Join(entry, opts.input), filepath.Join(entry, clean)); err != nil {
			fmt.Fprintf(r.Stderr, "lens submit: %v\n", err)
			return 1
		}
	}
	mode := "auto"
	if opts.ctv {
		mode = "ctv"
	}
	req := request{
		InputFile: clean, Mode: mode, Name: optional(opts.name), LLMModel: optional(opts.llmModel),
		ResearchModel: optional(opts.researchModel), DecisionModel: optional(opts.decisionModel),
		ResearchFallback: !opts.noResearchFallback, DeepRetry: !opts.noDeep,
	}
	if opts.categories != "" {
		spec, err := categories.Load(opts.categories)
		if err == nil {
			req.CustomCategories, err = json.Marshal(spec)
		}
		if err != nil {
			fmt.Fprintf(r.Stderr, "lens submit: %s: %v\n", opts.categories, err)
			_ = os.RemoveAll(entry)
			return 1
		}
		_ = os.Remove(opts.categories)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(r.Stderr, "lens submit: %v\n", err)
		return 1
	}
	tmp := filepath.Join(entry, "request.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		fmt.Fprintf(r.Stderr, "lens submit: %v\n", err)
		return 1
	}
	if err := os.Rename(tmp, filepath.Join(entry, "request.json")); err != nil {
		fmt.Fprintf(r.Stderr, "lens submit: %v\n", err)
		return 1
	}

	resultPath := filepath.Join(entry, "result.json")
	deadline := time.Now().Add(time.Duration(opts.timeout * float64(time.Second)))
	for !exists(resultPath) {
		if time.Now().After(deadline) {
			fmt.Fprintf(r.Stderr, "lens submit: request is waiting in %s; is lens.service running? "+
				"It will be queued when the service picks it up.\n", entry)
			return 2
		}
		r.Sleep(500 * time.Millisecond)
	}
	var result map[string]any
	data, err := os.ReadFile(resultPath) //nolint:gosec // inside our spool entry
	if err == nil {
		err = json.Unmarshal(data, &result)
	}
	_ = os.RemoveAll(entry)
	if err != nil {
		fmt.Fprintf(r.Stderr, "lens submit: unreadable result: %v\n", err)
		return 1
	}
	if msg, rejected := result["error"]; rejected {
		fmt.Fprintf(r.Stderr, "lens submit: rejected: %v\n", msg)
		return 1
	}
	line := fmt.Sprintf("Queued %v as job %v", result["input_file"], result["job_id"])
	if where := strings.TrimRight(os.Getenv("LENS_PUBLIC_URL"), "/"); where != "" {
		line += " — " + where + "/"
	}
	fmt.Fprintln(r.Stdout, line)
	return 0
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
