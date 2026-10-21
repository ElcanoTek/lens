// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ElcanoTek/lens/internal/catalog"
	"github.com/ElcanoTek/lens/internal/jobs"
)

// The index page view model. Every field mirrors what web_service.index()
// handed Jinja, so the rendered page is the same.

type chipView struct {
	Seg   string
	Label string
	Count int
	Pct   string // Python float repr, e.g. "60.0"
}

type fileView struct {
	Name           string
	Size           string
	Uploaded       string
	BreakdownTotal int
	BreakdownLabel string
	BreakdownChips []chipView
	// BreakdownAttr is the whole data-breakdown='…' attribute, byte for byte
	// what Jinja's `| tojson` produced (sorted keys, Python separators,
	// HTML-significant characters \u-escaped). main.js JSON.parses it.
	BreakdownAttr template.HTMLAttr
}

type progressView struct {
	Percentage float64
	Text       string
	ETA        string
	Successful int
	Errors     int
	Retrying   int
	// Segment widths for the stacked status bar: green ok, amber queued for
	// retry, red failed; the empty remainder is unstarted.
	OKPct     string
	RetryPct  string
	FailPct   string
	CostText  string
	CostTitle string
}

type runView struct {
	ID               string
	Title            string
	HasName          bool
	InputFile        string
	Status           string
	IsActive         bool
	IsArchived       bool
	Progress         progressView
	OutputFile       string
	ProgressFile     string
	LogFile          string
	CategoryFile     string
	CreatedAtISO     string
	CreatedAtDisplay string
	SettingsSummary  string
}

type indexPage struct {
	Nonce                             string
	Runs                              []runView
	InputFiles                        []fileView
	Catalog                           catalog.Catalog
	RecommendedModel                  string
	RecommendedClassifyLabel          string
	PreviousRecommendedModels         string
	RecommendedResearchModel          string
	RecommendedResearchLabel          string
	PreviousRecommendedResearchModels string
	RecommendedDecisionModel          string
	RecommendedDecisionLabel          string
	CurrentJobID                      string
	Error                             string
	Message                           string
	FilesError                        string
	FilesMessage                      string
	FilesScope                        string
	CSRFToken                         string
}

const displayLayout = "Jan 02, 2006 15:04 UTC"

// isoLayouts are the shapes datetime.fromisoformat accepted in practice.
var isoLayouts = []string{
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04Z07:00",
	"2006-01-02T15:04",
	"2006-01-02",
}

func parseISO(raw string) (time.Time, bool) {
	raw = strings.Replace(raw, "Z", "+00:00", 1)
	for _, layout := range isoLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

var (
	compactStampRe = regexp.MustCompile(`(\d{14})`)
	jobIDStampRe   = regexp.MustCompile(`^(\d{14})-`)
)

// formatCreatedAt renders a stored created_at for the Runs table. Like the
// Python it prints the stored wall time labelled UTC (Lens only ever stores
// UTC).
func formatCreatedAt(createdAt string) string {
	raw := strings.TrimSpace(createdAt)
	if raw == "" {
		return "Unknown date"
	}
	if t, ok := parseISO(raw); ok {
		return t.Format(displayLayout)
	}
	if m := compactStampRe.FindStringSubmatch(raw); m != nil {
		if t, err := time.Parse("20060102150405", m[1]); err == nil {
			return t.Format(displayLayout)
		}
	}
	return raw
}

// jobIDTime reads the UTC timestamp a job id starts with.
func jobIDTime(jobID string) (time.Time, bool) {
	m := jobIDStampRe.FindStringSubmatch(jobID)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102150405", m[1])
	return t, err == nil
}

func formatJobIDTimestamp(jobID string) string {
	if t, ok := jobIDTime(jobID); ok {
		return t.Format(displayLayout)
	}
	return "Unknown date"
}

// jobIDTimestampISO is datetime.isoformat() of the job-id stamp.
func jobIDTimestampISO(jobID string) string {
	if t, ok := jobIDTime(jobID); ok {
		return t.Format("2006-01-02T15:04:05+00:00")
	}
	return ""
}

var artifactSuffixes = []string{"_output.csv.categories.json", "_output.csv", "_progress.json", ".log"}

func jobIDFromOutputFilename(name string) (string, bool) {
	for _, suffix := range artifactSuffixes {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix), true
		}
	}
	return "", false
}

// countOf is max(0, int(str(progress.get(key) or 0))), 0 when unparsable.
func countOf(progress map[string]any, key string) int {
	n, ok := jobs.AsInt(progress[key])
	if !ok {
		return 0
	}
	return max(0, n)
}

// etaText is the two-rate ETA: items not yet attempted run at the measured
// fast-crawler rate; items queued for the retry pass each cost a roughly
// constant amount (single-session headless Chrome).
func etaText(job *jobs.Job, progress map[string]any, now time.Time) string {
	switch {
	case jobs.IsTerminal(job.Status):
		return "Done"
	case job.Status == jobs.StatusQueued:
		return "Queued"
	}
	total := countOf(progress, "total_domains")
	processed := countOf(progress, "processed")
	retrying := countOf(progress, "retrying")
	// Every item with any status has been through the fast crawler once.
	attempts := processed + retrying
	if total <= 0 || attempts <= 0 || job.StartedAt == "" {
		return "Estimating..."
	}
	started, ok := parseISO(job.StartedAt)
	if !ok {
		return "Estimating..."
	}
	elapsed := now.Sub(started).Seconds()
	if elapsed <= 5 {
		return "Estimating..."
	}
	fastRate := elapsed / float64(attempts)
	remainingFresh := max(0, total-attempts)
	remaining := float64(remainingFresh)*fastRate + float64(retrying)*jobs.DeepRetrySecondsPerItem
	switch {
	case remaining < 60:
		return "< 1m remaining"
	case remaining < 3600:
		return fmt.Sprintf("~%dm remaining", int(math.RoundToEven(remaining/60.0)))
	}
	hours := int(math.Floor(remaining / 3600))
	minutes := int(math.RoundToEven(math.Mod(remaining, 3600) / 60.0))
	return fmt.Sprintf("~%dh %dm remaining", hours, minutes)
}

// strictInt is int(str(v)) for a present value, or 0 for an absent one; ok
// is false when the value does not parse.
func strictInt(progress map[string]any, key string) (int, bool) {
	v, present := progress[key]
	if !present || v == nil {
		return 0, true
	}
	return jobs.AsInt(v)
}

func stringOf(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func progressViewFor(dirs jobs.Dirs, job *jobs.Job, now time.Time) progressView {
	progress := map[string]any{}
	if job.ProgressFile != "" {
		progress = jobs.ReadProgress(filepath.Join(dirs.Outputs, job.ProgressFile))
	}
	total, ok1 := strictInt(progress, "total_domains")
	processed, ok2 := strictInt(progress, "processed")
	successful, ok3 := strictInt(progress, "successful")
	errorsN, ok4 := strictInt(progress, "errors")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		total, processed, successful, errorsN = 0, 0, 0, 0
	}
	pct := 0.0
	if raw, present := progress["completion_percentage"]; present && raw != nil {
		if f, ok := jobs.AsFloat(raw); ok {
			pct = f
		}
	}
	if pct <= 0 && total > 0 {
		pct = float64(processed) / float64(total) * 100.0
	}
	if job.Status == jobs.StatusCompleted {
		pct = 100.0
	}
	pct = math.Max(0, math.Min(100, pct))

	retrying := 0
	if v := progress["retrying"]; pyTruthy(v) {
		if n, ok := jobs.AsInt(v); ok {
			retrying = n
		}
	}

	var text string
	switch {
	case total > 0:
		text = fmt.Sprintf("%d / %d", processed, total)
		if retrying > 0 {
			text += fmt.Sprintf(" · %d to retry", retrying)
		}
	case processed > 0:
		text = fmt.Sprintf("%d processed", processed)
	case job.Status == jobs.StatusQueued:
		text = "Waiting"
	case job.Status == jobs.StatusRunning || job.Status == jobs.StatusCancelling:
		text = "Starting"
	default:
		text = "—"
	}
	seg := func(count int) string {
		if total <= 0 {
			return pyFloat(0)
		}
		return pyFloat(pyRound(float64(count)/float64(total)*100.0, 2))
	}
	return progressView{
		Percentage: pyRound(pct, 1),
		Text:       text,
		ETA:        etaText(job, progress, now),
		Successful: successful,
		Errors:     errorsN,
		Retrying:   retrying,
		OKPct:      seg(successful),
		RetryPct:   seg(retrying),
		FailPct:    seg(errorsN),
		CostText:   stringOf(progress, "cost_text"),
		CostTitle:  stringOf(progress, "cost_title"),
	}
}

// archivedProgress is the progress cell for a run that is only artifacts on
// disk: final counts, no ETA.
func archivedProgress(progress map[string]any) progressView {
	asInt := func(key string) int {
		v := progress[key]
		if !pyTruthy(v) {
			return 0
		}
		n, ok := jobs.AsInt(v)
		if !ok {
			return 0
		}
		return n
	}
	total := asInt("total_domains")
	successful := asInt("successful")
	errorsN := asInt("errors")
	view := progressView{
		Percentage: 0, Text: "—", Successful: successful, Errors: errorsN,
		OKPct: pyFloat(0), RetryPct: pyFloat(0), FailPct: pyFloat(0),
		CostText: stringOf(progress, "cost_text"), CostTitle: stringOf(progress, "cost_title"),
	}
	if total != 0 {
		view.Percentage = 100
		view.Text = fmt.Sprintf("%d ok · %d failed", successful, errorsN)
		view.OKPct = pyFloat(pyRound(float64(successful)/float64(total)*100.0, 2))
		view.FailPct = pyFloat(pyRound(float64(errorsN)/float64(total)*100.0, 2))
	}
	return view
}

// settingsSummary is the Runs table's provenance line: only non-default
// settings. Most jobs run with pure defaults and show nothing; the line
// exists so a run that used a different model or disabled the research
// fallback is distinguishable from its neighbours after the fact.
func settingsSummary(job *jobs.Job) string {
	var parts []string
	if job.SubmittedBy != "" {
		parts = append(parts, "via API: "+job.SubmittedBy)
	}
	if job.HasCategories() {
		var spec struct {
			Categories []struct {
				Name string `json:"name"`
			} `json:"categories"`
		}
		_ = json.Unmarshal(job.CustomCategories, &spec)
		names := make([]string, 0, len(spec.Categories))
		for _, c := range spec.Categories {
			names = append(names, c.Name)
		}
		parts = append(parts, "custom: "+strings.Join(names, ", "))
		if job.DecisionModel != "" {
			parts = append(parts, "decision: "+job.DecisionModel)
		}
	}
	if job.LLMModel != "" {
		parts = append(parts, "model: "+job.LLMModel)
	}
	if !job.DeepRetry {
		parts = append(parts, "headless Chrome retry off")
	}
	if !job.ResearchFallback {
		parts = append(parts, "research fallback off")
	} else if job.ResearchModel != "" {
		parts = append(parts, "research: "+job.ResearchModel)
	}
	return strings.Join(parts, " · ")
}

func fileViews(files []jobs.InputFile) []fileView {
	out := make([]fileView, 0, len(files))
	for _, f := range files {
		chips := make([]chipView, 0, len(f.BreakdownChips))
		types := make([]any, 0, len(f.BreakdownChips))
		for _, c := range f.BreakdownChips {
			chips = append(chips, chipView{Seg: c.Seg, Label: c.Label, Count: c.Count, Pct: pyFloat(c.Pct)})
			types = append(types, map[string]any{"key": c.Key, "seg": c.Seg, "label": c.Label, "count": c.Count, "pct": c.Pct})
		}
		attr := "data-breakdown='" + htmlSafeJSON(map[string]any{"total": f.BreakdownTotal, "types": types}) + "'"
		out = append(out, fileView{
			Name: f.Name, Size: f.Size, Uploaded: f.Uploaded,
			BreakdownTotal: f.BreakdownTotal, BreakdownLabel: f.BreakdownLabel, BreakdownChips: chips,
			BreakdownAttr: template.HTMLAttr(attr), //nolint:gosec // JSON with every quote/markup character \u-escaped
		})
	}
	return out
}

type artifactGroup struct {
	output, progress, log, category string
}

// buildRuns merges live/queued jobs with finished runs found on disk into
// one list, newest first. Artifacts whose job is no longer in state (e.g.
// pre-restart) appear as archived runs so nothing on disk is orphaned from
// the UI.
func buildRuns(dirs jobs.Dirs, list []jobs.Job, now time.Time) []runView {
	known := map[string]bool{}
	for _, j := range list {
		known[j.ID] = true
	}
	groups := map[string]*artifactGroup{}
	for _, name := range dirs.ListOutputFiles() {
		id, ok := jobIDFromOutputFilename(name)
		if !ok {
			continue
		}
		g := groups[id]
		if g == nil {
			g = &artifactGroup{}
			groups[id] = g
		}
		switch {
		case strings.HasSuffix(name, "_output.csv"):
			g.output = name
		case strings.HasSuffix(name, "_output.csv.categories.json"):
			g.category = name
		case strings.HasSuffix(name, "_progress.json"):
			g.progress = name
		case strings.HasSuffix(name, ".log"):
			g.log = name
		}
	}

	runs := make([]runView, 0, len(list)+len(groups))
	for i := range list {
		job := &list[i]
		g := groups[job.ID]
		if g == nil {
			g = &artifactGroup{}
		}
		runs = append(runs, runView{
			ID: job.ID, Title: firstNonEmpty(job.Name, job.ID), HasName: job.Name != "",
			InputFile: job.InputFile, Status: job.Status,
			IsActive: job.Status == jobs.StatusQueued || job.Status == jobs.StatusRunning || job.Status == jobs.StatusCancelling,
			Progress: progressViewFor(dirs, job, now),
			OutputFile: firstNonEmpty(job.OutputFile, g.output), ProgressFile: firstNonEmpty(job.ProgressFile, g.progress),
			LogFile: firstNonEmpty(job.LogFile, g.log), CategoryFile: g.category,
			CreatedAtISO: job.CreatedAt, CreatedAtDisplay: formatCreatedAt(job.CreatedAt),
			SettingsSummary: settingsSummary(job),
		})
	}

	ids := make([]string, 0, len(groups))
	for id := range groups {
		if !known[id] {
			ids = append(ids, id)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	for _, id := range ids {
		g := groups[id]
		progress := map[string]any{}
		if g.progress != "" {
			progress = jobs.ReadProgress(filepath.Join(dirs.Outputs, g.progress))
		}
		runs = append(runs, runView{
			ID: id, Title: id, Status: "archived", IsArchived: true,
			Progress:   archivedProgress(progress),
			OutputFile: g.output, ProgressFile: g.progress, LogFile: g.log, CategoryFile: g.category,
			CreatedAtISO: jobIDTimestampISO(id), CreatedAtDisplay: formatJobIDTimestamp(id),
		})
	}
	// Stable, so equal stamps keep the order above (Python's sort is too).
	sort.SliceStable(runs, func(a, b int) bool { return runs[a].CreatedAtISO > runs[b].CreatedAtISO })
	return runs
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !s.auth.Authenticated(r) {
		s.auth.Unauthenticated(w, r)
		return
	}
	now := s.now()
	cat := s.catalog.Get(r.Context())
	label := func(kind, fallback string) string {
		if l := cat.Label(kind, catalog.Recommended(kind)); l != "" {
			return l
		}
		return fallback
	}
	lastQuery := func(key string) string { return queryLast(r, key) }
	page := indexPage{
		Nonce:                             Nonce(r),
		Runs:                              buildRuns(s.dirs, s.manager.List(), now),
		InputFiles:                        fileViews(s.dirs.ListInputFiles(now)),
		Catalog:                           cat,
		RecommendedModel:                  catalog.RecommendedModel,
		RecommendedClassifyLabel:          label("classify", "OpenAI: GPT Luna Latest"),
		PreviousRecommendedModels:         strings.Join(catalog.PreviousRecommendedModels, ","),
		RecommendedResearchModel:          catalog.RecommendedResearchModel,
		RecommendedResearchLabel:          label("research", "Perplexity Sonar"),
		PreviousRecommendedResearchModels: strings.Join(catalog.PreviousRecommendedResearchModels, ","),
		RecommendedDecisionModel:          catalog.RecommendedDecisionModel,
		RecommendedDecisionLabel:          label("decision", "TypeSafe: Jev Latest"),
		CurrentJobID:                      s.manager.CurrentID(),
		Error:                             lastQuery("error"),
		Message:                           lastQuery("message"),
		FilesError:                        lastQuery("files_error"),
		FilesMessage:                      lastQuery("files_message"),
		FilesScope:                        lastQuery("files_scope"),
		CSRFToken:                         s.auth.CSRFToken(r),
	}
	s.render(w, "index.html", page)
}
