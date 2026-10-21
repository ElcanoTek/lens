// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package jobs is the dashboard's job queue: the Job model persisted in
// managed-files/outputs/_jobs.json (format shared with the Python release
// and read by operators during updates), the worker that runs queued jobs
// as `lens run` subprocesses (LENS_MAX_PARALLEL_JOBS at a time, default 1),
// cancellation, deletion, and the input-library helpers the dashboard, the
// API and `lens submit` share.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ElcanoTek/lens/internal/logx"
)

var logger = logx.New("lens.web")

// Modes. Users no longer pick one: Lens detects the input type itself.
// Legacy values still load so jobs persisted by older versions (and API
// clients re-queuing them) keep working.
const DefaultMode = "auto"

var allowedModes = map[string]bool{"auto": true, "direct": true, "deep": true, "firecrawl": true, "app": true, "ctv": true}

// ValidMode reports whether mode may be queued.
func ValidMode(mode string) bool { return allowedModes[mode] }

// Job statuses.
const (
	StatusQueued     = "queued"
	StatusRunning    = "running"
	StatusCancelling = "cancelling"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
	StatusCancelled  = "cancelled"
)

// IsTerminal reports whether a status is final.
func IsTerminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCancelled
}

// Job is one queued or finished run. JSON names are the _jobs.json keys.
type Job struct {
	ID               string          `json:"id"`
	InputFile        string          `json:"input_file"`
	Mode             string          `json:"mode"`
	Name             string          `json:"name"`
	LLMModel         string          `json:"llm_model"`
	ResearchFallback bool            `json:"research_fallback"`
	DeepRetry        bool            `json:"deep_retry"`
	ResearchModel    string          `json:"research_model"`
	CustomCategories json.RawMessage `json:"custom_categories"`
	DecisionModel    string          `json:"decision_model"`
	// SubmittedBy is the label of the API token that queued the job; empty
	// for the dashboard and `lens submit`.
	SubmittedBy  string `json:"submitted_by"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at"`
	OutputFile   string `json:"output_file"`
	ProgressFile string `json:"progress_file"`
	LogFile      string `json:"log_file"`
	ReturnCode   *int   `json:"return_code"`
	Error        string `json:"error"`
}

// HasCategories reports whether the job carries custom categories.
func (j *Job) HasCategories() bool {
	s := strings.TrimSpace(string(j.CustomCategories))
	return s != "" && s != "null"
}

// persisted is Job with nullable strings, matching what Python wrote.
type persisted struct {
	ID               string          `json:"id"`
	InputFile        string          `json:"input_file"`
	Mode             string          `json:"mode"`
	Name             *string         `json:"name"`
	LLMModel         *string         `json:"llm_model"`
	ResearchFallback bool            `json:"research_fallback"`
	DeepRetry        bool            `json:"deep_retry"`
	ResearchModel    *string         `json:"research_model"`
	CustomCategories json.RawMessage `json:"custom_categories"`
	DecisionModel    *string         `json:"decision_model"`
	SubmittedBy      *string         `json:"submitted_by"`
	Status           string          `json:"status"`
	CreatedAt        string          `json:"created_at"`
	StartedAt        *string         `json:"started_at"`
	FinishedAt       *string         `json:"finished_at"`
	OutputFile       *string         `json:"output_file"`
	ProgressFile     *string         `json:"progress_file"`
	LogFile          *string         `json:"log_file"`
	ReturnCode       *int            `json:"return_code"`
	Error            *string         `json:"error"`
}

func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func val(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (j *Job) persisted() persisted {
	cats := j.CustomCategories
	if !j.HasCategories() {
		cats = json.RawMessage("null")
	}
	return persisted{
		ID: j.ID, InputFile: j.InputFile, Mode: j.Mode, Name: ptr(j.Name), LLMModel: ptr(j.LLMModel),
		ResearchFallback: j.ResearchFallback, DeepRetry: j.DeepRetry, ResearchModel: ptr(j.ResearchModel),
		CustomCategories: cats, DecisionModel: ptr(j.DecisionModel), SubmittedBy: ptr(j.SubmittedBy),
		Status: j.Status, CreatedAt: j.CreatedAt, StartedAt: ptr(j.StartedAt), FinishedAt: ptr(j.FinishedAt),
		OutputFile: ptr(j.OutputFile), ProgressFile: ptr(j.ProgressFile), LogFile: ptr(j.LogFile),
		ReturnCode: j.ReturnCode, Error: ptr(j.Error),
	}
}

// NewJob carries what a caller supplies to queue a job.
type NewJob struct {
	InputFile        string
	Mode             string
	Name             string
	LLMModel         string
	ResearchFallback bool
	DeepRetry        bool
	ResearchModel    string
	CustomCategories json.RawMessage // already validated/normalised, or nil
	DecisionModel    string
	SubmittedBy      string
}

// Manager owns the job table and runs the queue.
type Manager struct {
	Dirs Dirs
	// Home is the working directory job subprocesses run in.
	Home string
	// Executable and BaseArgs form the job command; the defaults run
	// `<this binary> run`.
	Executable string
	BaseArgs   []string
	// MaxParallel caps concurrently running jobs (1-8).
	MaxParallel int
	// ValidateCategories re-validates persisted definitions on load; a job
	// whose saved definitions no longer validate is marked failed.
	ValidateCategories func(json.RawMessage) (json.RawMessage, error)
	// FormatCategories renders the <output>.categories.json sidecar the job
	// subprocess reads (and users download); nil means indented JSON. The
	// web server sets it to the Python release's json.dumps(indent=2) form.
	FormatCategories func(json.RawMessage) ([]byte, error)
	// Now is the clock (tests).
	Now func() time.Time

	mu      sync.Mutex
	jobs    map[string]*Job
	queue   []string
	running map[string]*exec.Cmd
	current string
	wake    chan struct{}
	wg      sync.WaitGroup
}

// MaxParallelFromEnv reads LENS_MAX_PARALLEL_JOBS, clamped to 1-8.
func MaxParallelFromEnv() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LENS_MAX_PARALLEL_JOBS")))
	if err != nil {
		n = 1
	}
	return max(1, min(n, 8))
}

// NewManager returns a manager for home's managed-files tree.
func NewManager(home string) *Manager {
	exe, err := os.Executable()
	if err != nil {
		exe = "lens"
	}
	return &Manager{
		Dirs: DirsFor(home), Home: home, Executable: exe, BaseArgs: []string{"run"},
		MaxParallel: MaxParallelFromEnv(), Now: time.Now,
		jobs: map[string]*Job{}, running: map[string]*exec.Cmd{}, wake: make(chan struct{}, 1),
	}
}

func (m *Manager) now() string {
	return m.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// save writes _jobs.json; callers hold m.mu. The error is logged here and
// returned so state transitions a caller reports (queued, cancelled,
// deleted, started) can be refused or rolled back when they were not
// persisted: a restart reloads only what is on disk.
func (m *Manager) save() error {
	list := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		list = append(list, j)
	}
	sort.SliceStable(list, func(a, b int) bool { return list[a].CreatedAt < list[b].CreatedAt })
	out := make([]persisted, 0, len(list))
	for _, j := range list {
		out = append(out, j.persisted())
	}
	raw, err := json.MarshalIndent(map[string]any{"saved_at": m.now(), "jobs": out}, "", "  ")
	if err != nil {
		logger.Error("could not encode job state: " + err.Error())
		return err
	}
	if err := os.MkdirAll(m.Dirs.Outputs, 0o755); err != nil { //nolint:gosec // managed-files is operator-readable
		logger.Error("could not create outputs dir: " + err.Error())
		return err
	}
	tmp := m.Dirs.StatePath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil { //nolint:gosec // operators read _jobs.json to drain before updates
		logger.Error("could not save job state: " + err.Error())
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, m.Dirs.StatePath()); err != nil {
		logger.Error("could not save job state: " + err.Error())
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ErrNotSaved wraps a job-state write failure; the requested change was not
// applied.
var ErrNotSaved = errors.New("could not save job state")

func notSaved(err error) error { return fmt.Errorf("%w: %w", ErrNotSaved, err) }

var jobIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Load hydrates the table from _jobs.json. Jobs that were running when the
// server stopped are marked failed ("Interrupted by server restart").
func (m *Manager) Load() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs, m.queue, m.current = map[string]*Job{}, nil, ""

	raw, err := os.ReadFile(m.Dirs.StatePath())
	if err != nil {
		return
	}
	var doc struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return
	}
	interrupted := false
	for _, rawJob := range doc.Jobs {
		var p persisted
		// Defaults for keys absent from older files.
		p.ResearchFallback, p.DeepRetry = true, true
		if json.Unmarshal(rawJob, &p) != nil {
			continue
		}
		id := strings.TrimSpace(p.ID)
		mode := strings.TrimSpace(p.Mode)
		// State on disk is trusted-ish (we wrote it) but is the one input
		// to path construction here: re-validate every filename and the id.
		input := SafeFilenameOrEmpty(p.InputFile)
		if id == "" || !jobIDRe.MatchString(id) || input == "" || !allowedModes[mode] {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(p.Status))
		if status == "" {
			status = StatusQueued
		}
		created := p.CreatedAt
		if created == "" {
			created = m.now()
		}
		job := &Job{
			ID: id, InputFile: input, Mode: mode, Name: strings.TrimSpace(val(p.Name)),
			LLMModel: cleanPersistedModel(val(p.LLMModel)), ResearchFallback: p.ResearchFallback,
			DeepRetry: p.DeepRetry, ResearchModel: cleanPersistedModel(val(p.ResearchModel)),
			DecisionModel: cleanPersistedModel(val(p.DecisionModel)), Status: status, CreatedAt: created,
			StartedAt: val(p.StartedAt), FinishedAt: val(p.FinishedAt),
			OutputFile: SafeFilenameOrEmpty(val(p.OutputFile)), ProgressFile: SafeFilenameOrEmpty(val(p.ProgressFile)),
			LogFile: SafeFilenameOrEmpty(val(p.LogFile)), ReturnCode: p.ReturnCode, Error: val(p.Error),
		}
		if sb := val(p.SubmittedBy); sb != "" {
			job.SubmittedBy = truncateRunes(sb, 80)
		}
		if cats := strings.TrimSpace(string(p.CustomCategories)); cats != "" && cats != "null" {
			if m.ValidateCategories != nil {
				norm, err := m.ValidateCategories(p.CustomCategories)
				if err != nil {
					job.Status = StatusFailed
					job.Error = "Invalid saved custom category definitions"
				} else {
					job.CustomCategories = norm
				}
			} else {
				job.CustomCategories = p.CustomCategories
			}
		}
		if job.Status == StatusRunning || job.Status == StatusCancelling {
			job.Status = StatusFailed
			job.Error = "Interrupted by server restart"
			if job.FinishedAt == "" {
				job.FinishedAt = m.now()
			}
			interrupted = true
		}
		m.jobs[id] = job
	}
	var queued []*Job
	for _, j := range m.jobs {
		if j.Status == StatusQueued {
			queued = append(queued, j)
		}
	}
	sort.SliceStable(queued, func(a, b int) bool { return queued[a].CreatedAt < queued[b].CreatedAt })
	for _, j := range queued {
		m.queue = append(m.queue, j.ID)
	}
	if len(m.queue) > 0 {
		m.signal()
	}
	if interrupted {
		_ = m.save() // best effort; logged
	}
}

func cleanPersistedModel(v string) string {
	s, err := CleanModelID(v)
	if err != nil {
		return ""
	}
	return s
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// Create queues a job and persists the table.
func (m *Manager) Create(n NewJob) (*Job, error) {
	if !allowedModes[n.Mode] {
		return nil, fmt.Errorf("Invalid mode: %s", n.Mode) //nolint:staticcheck // user-facing message
	}
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	job := &Job{
		ID:        m.Now().UTC().Format("20060102150405") + "-" + hex.EncodeToString(suffix),
		InputFile: n.InputFile, Mode: n.Mode, Name: strings.TrimSpace(n.Name),
		LLMModel: strings.TrimSpace(n.LLMModel), ResearchFallback: n.ResearchFallback, DeepRetry: n.DeepRetry,
		ResearchModel: strings.TrimSpace(n.ResearchModel), CustomCategories: n.CustomCategories,
		DecisionModel: strings.TrimSpace(n.DecisionModel), SubmittedBy: n.SubmittedBy,
		Status: StatusQueued, CreatedAt: m.now(),
	}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.queue = append(m.queue, job.ID)
	if err := m.save(); err != nil {
		// Never queue a job the next restart would not know about.
		delete(m.jobs, job.ID)
		m.removeQueued(job.ID)
		m.mu.Unlock()
		return nil, notSaved(err)
	}
	cp := *job
	m.mu.Unlock()
	m.signal()
	return &cp, nil
}

// List returns copies of every job, newest first.
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, *j)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].CreatedAt > out[b].CreatedAt })
	return out
}

// Get returns a copy of one job.
func (m *Manager) Get(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// CurrentID is the most recently started job still running (the
// dashboard's default live view), or "".
func (m *Manager) CurrentID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// QueuePosition is id's 1-based position in the queue, or 0.
func (m *Manager) QueuePosition(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, q := range m.queue {
		if q == id {
			return i + 1
		}
	}
	return 0
}

// QueueLength is the number of queued jobs.
func (m *Manager) QueueLength() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.queue)
}

// Errors returned by Cancel and Delete.
var (
	ErrUnknownJob = errors.New("unknown job")
)

// NotAllowedError says a job is in the wrong state for an operation.
type NotAllowedError struct{ msg string }

func (e *NotAllowedError) Error() string { return e.msg }

// Cancel cancels a queued job, or asks a running one to stop (SIGINT to its
// process group, which the pipeline handles as a graceful shutdown).
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return ErrUnknownJob
	}
	if job.Status == StatusQueued {
		queue := append([]string(nil), m.queue...)
		m.removeQueued(id)
		job.Status = StatusCancelled
		job.FinishedAt = m.now()
		if err := m.save(); err != nil {
			// Unsaved, a restart would run it anyway: refuse instead.
			m.queue, job.Status, job.FinishedAt = queue, StatusQueued, ""
			return notSaved(err)
		}
		return nil
	}
	if cmd, slot := m.running[id]; job.Status == StatusRunning && slot {
		job.Status = StatusCancelling
		if err := m.save(); err != nil {
			job.Status = StatusRunning
			return notSaved(err)
		}
		if cmd != nil && cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
		}
		// Otherwise the process is still being started: runJob sees
		// "cancelling" and either never starts it or signals it right after.
		return nil
	}
	return &NotAllowedError{fmt.Sprintf("Job %s is not cancellable", id)}
}

func (m *Manager) removeQueued(id string) {
	for i, q := range m.queue {
		if q == id {
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
			return
		}
	}
}

// Delete removes a finished job and its artifacts.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return ErrUnknownJob
	}
	if !IsTerminal(job.Status) {
		return &NotAllowedError{fmt.Sprintf("Job %s is not deletable", id)}
	}
	// Forget the job durably first: if that fails, keep both the entry and
	// its files rather than leave a recorded job with no artifacts.
	delete(m.jobs, id)
	if err := m.save(); err != nil {
		m.jobs[id] = job
		return notSaved(err)
	}
	m.removeQueued(id)
	if m.current == id {
		m.current = ""
	}
	for _, name := range []string{job.OutputFile, job.ProgressFile, job.LogFile} {
		if name == "" {
			continue
		}
		path := filepath.Join(m.Dirs.Outputs, name)
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
			_ = os.Remove(path)
		}
	}
	if job.HasCategories() {
		_ = os.Remove(filepath.Join(m.Dirs.Outputs, job.ID+"_output.csv.categories.json"))
	}
	return nil
}

// Run starts queued jobs until ctx is done, then waits for running jobs'
// bookkeeping to finish. Running subprocesses are left to systemd's
// KillMode on shutdown, as with the Python service.
func (m *Manager) Run(ctx context.Context) {
	for {
		m.mu.Lock()
		limit := max(1, min(m.MaxParallel, 8))
		for len(m.queue) > 0 && len(m.running) < limit {
			id := m.queue[0]
			m.queue = m.queue[1:]
			m.running[id] = nil
			m.current = id
			m.wg.Add(1)
			go m.runAndRelease(id)
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			m.wg.Wait()
			return
		case <-m.wake:
		}
	}
}

func (m *Manager) runAndRelease(id string) {
	defer m.wg.Done()
	m.runJob(id)
	m.mu.Lock()
	delete(m.running, id)
	if m.current == id {
		m.current = ""
		for other := range m.running {
			m.current = other
			break
		}
	}
	m.mu.Unlock()
	m.signal()
}

// Command returns the argv a job runs with (exposed for tests).
func (m *Manager) Command(job *Job) []string {
	input := filepath.Join(m.Dirs.Inputs, job.InputFile)
	output := filepath.Join(m.Dirs.Outputs, job.ID+"_output.csv")
	args := append([]string{}, m.BaseArgs...)
	args = append(args,
		"--input-csv", input,
		"--output-csv", output,
		"--progress-file", filepath.Join(m.Dirs.Outputs, job.ID+"_progress.json"),
		"--log-file", filepath.Join(m.Dirs.Outputs, job.ID+".log"),
	)
	if job.Mode == "ctv" {
		args = append(args, "--ctv")
	} else {
		// "app" was the direct scraper; "firecrawl" was removed and runs
		// the default ladder.
		mode := map[string]string{"app": "direct", "firecrawl": "auto"}[job.Mode]
		if mode == "" {
			mode = job.Mode
		}
		args = append(args, "--scrape-mode", mode)
	}
	if job.LLMModel != "" {
		args = append(args, "--llm-model", job.LLMModel)
	}
	if !job.ResearchFallback {
		args = append(args, "--research-fallback", "off")
	}
	if !job.DeepRetry {
		args = append(args, "--deep-retry", "off")
	}
	if job.ResearchModel != "" {
		args = append(args, "--research-model", job.ResearchModel)
	}
	if job.HasCategories() {
		args = append(args, "--custom-categories", output+".categories.json")
		if job.DecisionModel != "" {
			args = append(args, "--decision-model", job.DecisionModel)
		}
	}
	return args
}

func (m *Manager) runJob(id string) {
	m.mu.Lock()
	job := m.jobs[id]
	// Cancelled (or deleted) between leaving the queue and starting here:
	// Run pops the id before this goroutine takes the lock.
	if job == nil || job.Status != StatusQueued {
		m.mu.Unlock()
		return
	}
	job.Status = StatusRunning
	job.StartedAt = m.now()
	if err := m.save(); err != nil {
		// Don't start a run the next restart would see as still queued and
		// start again; fail it in memory so the dashboard says why.
		job.Status, job.StartedAt = StatusFailed, ""
		job.FinishedAt = m.now()
		job.Error = notSaved(err).Error()
		m.mu.Unlock()
		return
	}
	args := m.Command(job)
	snapshot := *job
	m.mu.Unlock()

	fail := func(err error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		job.Status = StatusFailed
		job.FinishedAt = m.now()
		job.Error = err.Error()
		_ = m.save() // best effort; logged
	}

	if snapshot.HasCategories() {
		path := filepath.Join(m.Dirs.Outputs, snapshot.ID+"_output.csv.categories.json")
		pretty := []byte(snapshot.CustomCategories)
		if m.FormatCategories != nil {
			if formatted, err := m.FormatCategories(snapshot.CustomCategories); err == nil {
				pretty = formatted
			}
		} else if indented, err := json.MarshalIndent(snapshot.CustomCategories, "", "  "); err == nil {
			pretty = indented
		}
		if err := os.WriteFile(path, pretty, 0o644); err != nil { //nolint:gosec // downloadable artifact
			fail(err)
			return
		}
	}

	m.mu.Lock()
	if job.Status == StatusCancelling {
		// Cancelled while the categories file was being written.
		job.Status = StatusCancelled
		job.FinishedAt = m.now()
		_ = m.save() // best effort; logged
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	cmd := exec.Command(m.Executable, args...) //nolint:gosec,noctx // argv only (no shell); the job outlives request contexts and is cancelled via its process group
	cmd.Dir = m.Home
	cmd.Stdout, cmd.Stderr = nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fail(err)
		return
	}
	m.mu.Lock()
	m.running[id] = cmd
	if job.Status == StatusCancelling {
		// Cancel arrived while the process was starting.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	}
	job.OutputFile = snapshot.ID + "_output.csv"
	job.ProgressFile = snapshot.ID + "_progress.json"
	job.LogFile = snapshot.ID + ".log"
	_ = m.save() // best effort; logged
	m.mu.Unlock()

	err := cmd.Wait()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			fail(err)
			return
		}
		code = exitErr.ExitCode()
		if code == -1 { // killed by a signal: report it like Python (-N)
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = -int(ws.Signal())
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job.ReturnCode = &code
	switch {
	case job.Status == StatusCancelling:
		job.Status = StatusCancelled
	case code == 0:
		job.Status = StatusCompleted
		job.Error = ""
	default:
		job.Status = StatusFailed
		job.Error = fmt.Sprintf("Analyzer exited with code %d", code)
	}
	job.FinishedAt = m.now()
	_ = m.save() // best effort; logged
}
