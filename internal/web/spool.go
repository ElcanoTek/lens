// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ElcanoTek/lens/internal/categories"
	"github.com/ElcanoTek/lens/internal/categories/pyjson"
	"github.com/ElcanoTek/lens/internal/jobs"
)

// `lens submit` hands jobs to the running service through
// managed-files/spool/: one subdirectory per request holding the input file
// and request.json. Only root and the service user can write there, so no
// web credential is involved. The service answers with result.json
// ({"job_id", "input_file"} or {"error"}), written atomically.
const (
	// SpoolPollInterval is how often the spool is scanned.
	SpoolPollInterval = 2 * time.Second
	// SpoolResultTTL is how long an answered entry the CLI never collected
	// is kept before it is swept.
	SpoolResultTTL = time.Hour
)

func writeSpoolResult(entry string, result pyjson.Object) error {
	tmp := filepath.Join(entry, "result.json.tmp")
	if err := os.WriteFile(tmp, []byte(pyjson.Dumps(result)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(entry, "result.json"))
}

// spoolModelID is _clean_model_id(request.get(key)).
func spoolModelID(request map[string]any, key string) (string, error) {
	v := request[key]
	if !pyTruthy(v) {
		return "", nil
	}
	return jobs.CleanModelID(pyStr(v))
}

func spoolBool(request map[string]any, key string, def bool) bool {
	v, ok := request[key]
	if !ok {
		return def
	}
	return pyTruthy(v)
}

// ingestSpoolEntry turns one request into a normal dashboard job. Every
// failure is answered in result.json; nothing is queued then.
func (s *Server) ingestSpoolEntry(entry string) {
	job, stored, err := s.spoolJob(entry)
	if err != nil {
		if werr := writeSpoolResult(entry, pyjson.Object{{Key: "error", Value: err.Error()}}); werr != nil {
			logger.Warn(fmt.Sprintf("Could not answer lens submit request %s: %v", filepath.Base(entry), werr))
		}
		logger.Warn(fmt.Sprintf("Rejected lens submit request %s: %v", filepath.Base(entry), err))
		return
	}
	if err := writeSpoolResult(entry, pyjson.Object{{Key: "job_id", Value: job.ID}, {Key: "input_file", Value: stored}}); err != nil {
		logger.Warn(fmt.Sprintf("Could not answer lens submit request %s: %v", filepath.Base(entry), err))
	}
	logger.Info(fmt.Sprintf("Queued job %s from lens submit (%s)", job.ID, stored))
}

func (s *Server) spoolJob(entry string) (*jobs.Job, string, error) {
	raw, err := os.ReadFile(filepath.Join(entry, "request.json")) //nolint:gosec // inside the 0700 spool
	if err != nil {
		return nil, "", err
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, "", err
	}
	request, ok := decoded.(map[string]any)
	if !ok {
		return nil, "", errors.New("request.json must be an object")
	}
	name := ""
	if v := request["input_file"]; pyTruthy(v) {
		name = pyStr(v)
	}
	filename, err := jobs.ValidateInputFilename(name)
	if err != nil {
		return nil, "", err
	}
	source := filepath.Join(entry, filename)
	if !isRegular(source) {
		return nil, "", fmt.Errorf("%s is missing from the request", filename)
	}
	payload, err := readCapped(source, s.maxUpload)
	if err != nil {
		return nil, "", err
	}
	if len(payload) == 0 {
		return nil, "", errors.New("Input file is empty") //nolint:staticcheck // user-facing message, kept from the Python release
	}
	if len(payload) > s.maxUpload {
		return nil, "", fmt.Errorf("File exceeds %d MB limit", s.maxUpload/(1024*1024)) //nolint:staticcheck // user-facing message, kept from the Python release
	}
	if err := jobs.VerifyInputPayload(filename, payload); err != nil {
		return nil, "", err
	}
	mode := jobs.DefaultMode
	if v := request["mode"]; pyTruthy(v) {
		mode = pyStr(v)
	}
	if !jobs.ValidMode(mode) {
		return nil, "", fmt.Errorf("Invalid mode: %s", mode) //nolint:staticcheck // user-facing message, kept from the Python release
	}
	var cats json.RawMessage
	if v, ok := request["custom_categories"]; ok && v != nil {
		spec, err := categories.Validate(v)
		if err != nil {
			return nil, "", err
		}
		if cats, err = marshalSpec(spec); err != nil {
			return nil, "", err
		}
	}
	jobName := ""
	if v := request["name"]; pyTruthy(v) {
		jobName = strings.TrimSpace(pyStr(v))
	}
	llm, err := spoolModelID(request, "llm_model")
	if err != nil {
		return nil, "", err
	}
	research, err := spoolModelID(request, "research_model")
	if err != nil {
		return nil, "", err
	}
	decision, err := spoolModelID(request, "decision_model")
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(s.dirs.Inputs, 0o755); err != nil { //nolint:gosec // operator-readable library
		return nil, "", err
	}
	stored, err := jobs.StoreUniqueInput(s.dirs.Inputs, filename, payload)
	if err != nil {
		return nil, "", err
	}
	s.dirs.Breakdown(filepath.Join(s.dirs.Inputs, stored))
	job, err := s.manager.Create(jobs.NewJob{
		InputFile: stored, Mode: mode, Name: jobName, LLMModel: llm, ResearchModel: research,
		DecisionModel: decision, ResearchFallback: spoolBool(request, "research_fallback", true),
		DeepRetry: spoolBool(request, "deep_retry", false), CustomCategories: cats,
	})
	if err != nil {
		return nil, "", err
	}
	_ = os.Remove(source)
	return job, stored, nil
}

func readCapped(path string, limit int) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // inside the spool entry
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, int64(limit)+1))
}

// ProcessSpoolOnce ingests every complete request and sweeps answered
// entries the CLI never collected. Symlinked entries are ignored.
func (s *Server) ProcessSpoolOnce() {
	entries, err := os.ReadDir(s.dirs.Spool)
	if err != nil {
		return
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Name() < entries[b].Name() })
	now := s.now()
	for _, e := range entries {
		// ReadDir does not follow symlinks: a symlink is never IsDir.
		if !e.IsDir() {
			continue
		}
		entry := filepath.Join(s.dirs.Spool, e.Name())
		if st, err := os.Stat(filepath.Join(entry, "result.json")); err == nil {
			// The CLI removes entries it has read; sweep the ones it did not.
			if now.Sub(st.ModTime()) > SpoolResultTTL {
				_ = os.RemoveAll(entry)
			}
			continue
		}
		if isRegular(filepath.Join(entry, "request.json")) {
			s.ingestSpoolEntry(entry)
		}
	}
}

// runSpool polls the spool until ctx is done. The watcher never dies: a
// panic in one pass is logged and the next pass runs on schedule.
func (s *Server) runSpool(ctx context.Context) {
	ticker := time.NewTicker(s.spoolInterval)
	defer ticker.Stop()
	for {
		func() {
			defer func() {
				if p := recover(); p != nil {
					logger.Error(fmt.Sprintf("lens submit spool watcher failed: %v", p))
				}
			}()
			s.ProcessSpoolOnce()
		}()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
