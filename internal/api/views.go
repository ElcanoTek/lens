// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package api

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ElcanoTek/lens/internal/jobs"
)

// nullable is a job field as JSON: Python's None for an empty value.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// asCount is a progress count: a non-negative integer, 0 when unreadable.
func asCount(v any) int {
	n, ok := jobs.AsInt(v)
	if !ok || n < 0 {
		return 0
	}
	return n
}

func parseISO(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	value = strings.Replace(value, "Z", "+00:00", 1)
	if t, err := time.Parse("2006-01-02T15:04:05.999999999-07:00", value); err == nil {
		return t, true
	}
	// A timestamp without an offset is UTC.
	if t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", value, time.UTC); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// etaSeconds is the same two-rate estimate the dashboard shows: the pace so
// far for what is left, plus the deep-retry cost per queued retry. nil when
// it can't tell yet.
func (a *API) etaSeconds(job *jobs.Job, progress map[string]any) any {
	if job.Status != jobs.StatusRunning {
		return nil
	}
	total := asCount(progress["total_domains"])
	processed := asCount(progress["processed"])
	retrying := asCount(progress["retrying"])
	attempts := processed + retrying
	started, ok := parseISO(job.StartedAt)
	if total <= 0 || attempts <= 0 || !ok {
		return nil
	}
	elapsed := a.now().Sub(started).Seconds()
	if elapsed <= 5 {
		return nil
	}
	remaining := float64(max(0, total-attempts)) * (elapsed / float64(attempts))
	remaining += float64(retrying) * jobs.DeepRetrySecondsPerItem
	return int(math.RoundToEven(remaining))
}

// filePath is where a job's file of kind lives; "" when the job has none.
func (a *API) filePath(job *jobs.Job, kind string) string {
	switch kind {
	case "output":
		if job.OutputFile != "" {
			return filepath.Join(a.dirs.Outputs, job.OutputFile)
		}
	case "log":
		if job.LogFile != "" {
			return filepath.Join(a.dirs.Outputs, job.LogFile)
		}
	case "input":
		return filepath.Join(a.dirs.Inputs, job.InputFile)
	case "categories":
		if job.HasCategories() {
			return filepath.Join(a.dirs.Outputs, job.ID+"_output.csv.categories.json")
		}
	}
	return ""
}

func isFile(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func (a *API) availableFiles(job *jobs.Job) []string {
	kinds := []string{}
	for _, kind := range FileKinds {
		if isFile(a.filePath(job, kind)) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

func (a *API) isCTV(job *jobs.Job) bool {
	if job.Mode == "ctv" {
		return true
	}
	return a.dirs.Breakdown(filepath.Join(a.dirs.Inputs, job.InputFile))["ctv"] != 0
}

func categoryNames(raw json.RawMessage) []string {
	names := []string{}
	var spec struct {
		Categories []struct {
			Name string `json:"name"`
		} `json:"categories"`
	}
	if json.Unmarshal(raw, &spec) == nil {
		for _, c := range spec.Categories {
			names = append(names, c.Name)
		}
	}
	return names
}

// settings is what the job's `lens run` subprocess actually runs with. A
// value the job does not carry is not passed as a flag, so the run falls
// back to this instance's config.json; report that value rather than the
// shipped default, or provenance is wrong on any Lens that overrides it.
func (a *API) settings(job *jobs.Job) Body {
	ctv := a.isCTV(job)
	llm, research := a.cfg.LLMModel, a.cfg.ResearchModel
	content := "websites/apps"
	if ctv {
		llm, research, content = a.cfg.CTVClassificationModel, a.cfg.CTVResearchModel, "ctv"
	}
	if job.LLMModel != "" {
		llm = job.LLMModel
	}
	if job.ResearchModel != "" {
		research = job.ResearchModel
	}
	var decision any
	if job.HasCategories() {
		decision = a.cfg.DecisionModel
		if job.DecisionModel != "" {
			decision = job.DecisionModel
		}
	}
	return Body{
		"mode":              job.Mode,
		"content":           content,
		"llm_model":         llm,
		"research_model":    research,
		"research_fallback": job.ResearchFallback && a.cfg.ResearchFallbackEnabled,
		"deep_retry":        job.DeepRetry && a.cfg.DeepRetryEnabled,
		"custom_categories": categoryNames(job.CustomCategories),
		"decision_model":    decision,
	}
}

func nextStep(job *jobs.Job, files []string) string {
	switch job.Status {
	case jobs.StatusQueued, jobs.StatusRunning, jobs.StatusCancelling:
		return "The job is still in progress. Check again with get_job in a minute or two " +
			"(eta_seconds is an estimate); do not create a duplicate job."
	case jobs.StatusCompleted:
		for _, f := range files {
			if f == "output" {
				return "Done. Use get_job_results for a summary and the first rows, or " +
					"get_download_link with file='output' for the full CSV."
			}
		}
		return "The job completed but its output CSV is missing on the Lens host."
	case jobs.StatusFailed:
		return "The job failed; see error and log_tail. The input can be re-run with create_job."
	}
	return "The job was cancelled."
}

// jobView is a job as the API shows it; detail adds settings, files, queue
// position, log tail and a next_step hint (get_job, create_job, cancel_job).
func (a *API) jobView(job *jobs.Job, r *http.Request, detail bool) Body {
	progress := map[string]any{}
	if job.ProgressFile != "" {
		progress = jobs.ReadProgress(filepath.Join(a.dirs.Outputs, job.ProgressFile))
	}
	total := asCount(progress["total_domains"])
	processed := asCount(progress["processed"])
	percent := 0.0
	if total > 0 {
		percent = math.RoundToEven(float64(processed)/float64(total)*100*10) / 10
	}
	view := Body{
		"id":           job.ID,
		"name":         nullable(job.Name),
		"status":       job.Status,
		"input_file":   job.InputFile,
		"submitted_by": nullable(job.SubmittedBy),
		"created_at":   job.CreatedAt,
		"started_at":   nullable(job.StartedAt),
		"finished_at":  nullable(job.FinishedAt),
		"progress": Body{
			"total":      total,
			"processed":  processed,
			"successful": asCount(progress["successful"]),
			"errors":     asCount(progress["errors"]),
			"retrying":   asCount(progress["retrying"]),
			"percent":    percent,
		},
		"cost_usd":    progress["cost_usd"],
		"eta_seconds": a.etaSeconds(job, progress),
		"error":       nullable(job.Error),
	}
	if base := PublicBaseURL(r); base != "" {
		view["dashboard_url"] = base + "/"
	}
	if detail {
		files := a.availableFiles(job)
		view["settings"] = a.settings(job)
		view["files"] = files
		if job.Status == jobs.StatusQueued {
			if pos := a.manager.QueuePosition(job.ID); pos > 0 {
				view["queue_position"] = pos
			} else {
				view["queue_position"] = nil
			}
		}
		if (job.Status == jobs.StatusFailed || job.Status == jobs.StatusRunning) && job.LogFile != "" {
			view["log_tail"] = jobs.TailLines(filepath.Join(a.dirs.Outputs, job.LogFile), LogTailLines)
		}
		view["next_step"] = nextStep(job, files)
	}
	return view
}
