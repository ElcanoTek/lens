// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/progress"
)

// manageCommands are `lens manage`'s subcommands, in manage.py's order.
var manageCommands = []string{"setup", "progress", "reset", "reset-failed", "validate", "sample", "stats", "help"}

// ManageMain is `lens manage COMMAND` (manage.py). It returns the exit
// code.
func ManageMain(args []string) int {
	return manage(args, os.Stdin, os.Stdout, os.Stderr)
}

func manage(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	usage := "usage: lens manage {" + strings.Join(manageCommands, ",") + "}"
	if len(args) != 1 {
		fmt.Fprintln(stderr, usage)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "lens manage: error: the following arguments are required: command")
		} else {
			fmt.Fprintln(stderr, "lens manage: error: unrecognized arguments: "+strings.Join(args[1:], " "))
		}
		return 2
	}
	command := args[0]
	if command == "-h" || command == "--help" {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	known := false
	for _, c := range manageCommands {
		known = known || c == command
	}
	if !known {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintf(stderr, "lens manage: error: argument command: invalid choice: '%s'\n", command)
		return 2
	}

	config.LoadDotenv()
	cfg := config.Load("config.json")
	in := bufio.NewReader(stdin)
	switch command {
	case "setup":
		return setupEnvironment(stdout)
	case "progress":
		showProgress(cfg, stdout)
	case "reset":
		resetProgress(cfg, in, stdout)
	case "reset-failed":
		resetFailed(cfg, in, stdout)
	case "validate":
		validateConfig(cfg, stdout)
	case "sample":
		return createSampleInput(stdout)
	case "stats":
		showStats(cfg, stdout)
	case "help":
		fmt.Fprint(stdout, `Available commands:
  setup       - Setup environment and config files
  progress    - Show current progress
  reset       - Reset all progress
  reset-failed- Reset failed domains for retry
  validate    - Validate configuration
  sample      - Create sample input file
  stats       - Show detailed statistics
`)
	}
	return 0
}

// defaultEnvFile is what setup writes when .env.example is missing.
const defaultEnvFile = "# Add your API keys here\nOPENROUTER_API_KEY=\n"

func copyIfMissing(dst, src string, perm os.FileMode, fallback func() ([]byte, error), stdout io.Writer, fromMsg, fallbackMsg string) error {
	if fileExists(dst) {
		return nil
	}
	data, err := os.ReadFile(src) //nolint:gosec // fixed example file in the Lens home
	switch {
	case err == nil:
		fmt.Fprintln(stdout, fromMsg)
	case errors.Is(err, fs.ErrNotExist):
		if data, err = fallback(); err != nil {
			return err
		}
		fmt.Fprintln(stdout, fallbackMsg)
	default:
		return err
	}
	return os.WriteFile(dst, data, perm) //nolint:gosec // dst is one of two fixed names in the Lens home
}

// setupEnvironment scaffolds .env and config.json from their examples (or
// built-in defaults) without touching existing files.
func setupEnvironment(stdout io.Writer) int {
	fmt.Fprintln(stdout, "🔧 Setting up environment...")
	if err := copyIfMissing(".env", ".env.example", 0o600, // .env holds secrets
		func() ([]byte, error) { return []byte(defaultEnvFile), nil },
		stdout, "Creating .env from .env.example", "Creating basic .env file"); err != nil {
		fmt.Fprintf(stdout, "❌ Could not create .env: %v\n", err)
		return 1
	}
	if err := copyIfMissing("config.json", "config.json.example", 0o644,
		func() ([]byte, error) {
			var b bytes.Buffer
			err := config.Default().WriteJSON(&b)
			return b.Bytes(), err
		},
		stdout, "Creating config.json from config.json.example", "Creating config.json with the built-in defaults"); err != nil {
		fmt.Fprintf(stdout, "❌ Could not create config.json: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "✅ Environment setup complete!")
	fmt.Fprintln(stdout, "📝 Please edit .env file with your API keys before running the script.")
	return 0
}

// progressEntry is the part of a progress entry manage prints.
type progressEntry struct {
	Status       string `json:"status"`
	Timestamp    string `json:"timestamp"`
	ErrorMessage string `json:"error_message"`
}

// orderedEntries reads processed_domains in file order (the order items
// were first recorded), which the Python dict preserved and Tracker does
// not expose.
func orderedEntries(path string) ([]string, map[string]progressEntry) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-chosen progress path
	if err != nil {
		return nil, nil
	}
	var doc struct {
		Processed json.RawMessage `json:"processed_domains"`
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.Processed) == 0 {
		return nil, nil
	}
	entries := map[string]progressEntry{}
	if json.Unmarshal(doc.Processed, &entries) != nil {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(doc.Processed))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil
	}
	var order []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		key, _ := tok.(string)
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			break
		}
		order = append(order, key)
	}
	return order, entries
}

func showProgress(cfg *config.Config, stdout io.Writer) {
	tracker := progress.Open(cfg.ProgressFilePath)
	tracker.Summary().Print(stdout)
	order, entries := orderedEntries(cfg.ProgressFilePath)
	if len(order) == 0 {
		return
	}
	fmt.Fprintln(stdout, "\n📋 Recent activity (last 10 domains):")
	if len(order) > 10 {
		order = order[len(order)-10:]
	}
	for _, id := range order {
		e := entries[id]
		fmt.Fprintf(stdout, "   %s - %s (%s)\n", id, e.Status, e.Timestamp)
	}
}

func prompt(in *bufio.Reader, stdout io.Writer, question string) string {
	fmt.Fprint(stdout, question)
	line, _ := in.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

func resetProgress(cfg *config.Config, in *bufio.Reader, stdout io.Writer) {
	if !fileExists(cfg.ProgressFilePath) {
		fmt.Fprintln(stdout, "ℹ️  No progress file found")
		return
	}
	answer := prompt(in, stdout, fmt.Sprintf("Are you sure you want to reset progress? This will delete %s (y/N): ", cfg.ProgressFilePath))
	if strings.ToLower(answer) != "y" {
		fmt.Fprintln(stdout, "❌ Progress reset cancelled")
		return
	}
	if err := os.Remove(cfg.ProgressFilePath); err != nil {
		fmt.Fprintf(stdout, "❌ Could not delete %s: %v\n", cfg.ProgressFilePath, err)
		return
	}
	fmt.Fprintln(stdout, "✅ Progress reset successfully")
}

// failedInOrder lists failed ids in file order.
func failedInOrder(cfg *config.Config) ([]string, map[string]progressEntry) {
	order, entries := orderedEntries(cfg.ProgressFilePath)
	var failed []string
	for _, id := range order {
		if entries[id].Status == progress.StatusError {
			failed = append(failed, id)
		}
	}
	return failed, entries
}

func resetFailed(cfg *config.Config, in *bufio.Reader, stdout io.Writer) {
	tracker := progress.Open(cfg.ProgressFilePath)
	failed, _ := failedInOrder(cfg)
	if len(failed) == 0 {
		fmt.Fprintln(stdout, "ℹ️  No failed domains found")
		return
	}
	fmt.Fprintf(stdout, "Found %d failed domains:\n", len(failed))
	for _, id := range failed[:min(10, len(failed))] {
		fmt.Fprintf(stdout, "   %s\n", id)
	}
	if len(failed) > 10 {
		fmt.Fprintf(stdout, "   ... and %d more\n", len(failed)-10)
	}
	answer := prompt(in, stdout, fmt.Sprintf("Reset %d failed domains for retry? (y/N): ", len(failed)))
	if strings.ToLower(answer) != "y" {
		fmt.Fprintln(stdout, "❌ Reset cancelled")
		return
	}
	if _, err := tracker.ResetFailed(); err != nil {
		fmt.Fprintf(stdout, "❌ Could not reset failed domains: %v\n", err)
		return
	}
	fmt.Fprintln(stdout, "✅ Failed domains reset successfully")
}

func validateConfig(cfg *config.Config, stdout io.Writer) bool {
	fmt.Fprintln(stdout, "🔍 Validating configuration...")
	if cfg.OpenRouterAPIKey == "" {
		fmt.Fprintln(stdout, "❌ Missing required environment variables:")
		fmt.Fprintln(stdout, "   OPENROUTER_API_KEY")
		fmt.Fprintln(stdout, "Please update your .env file with the required credentials")
		return false
	}
	if !fileExists(cfg.InputCSVPath) {
		fmt.Fprintf(stdout, "❌ Input file not found: %s\n", cfg.InputCSVPath)
		return false
	}
	fmt.Fprintln(stdout, "✅ Configuration validation passed")
	return true
}

const sampleInput = "Domain\nexample.com\ngoogle.com\ncnn.com\namazon.com\ngithub.com"

func createSampleInput(stdout io.Writer) int {
	if err := os.WriteFile("sample_input.csv", []byte(sampleInput), 0o644); err != nil { //nolint:gosec // a sample input for the operator
		fmt.Fprintf(stdout, "❌ Could not create sample_input.csv: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "✅ Created sample_input.csv")
	fmt.Fprintln(stdout, "You can use this as a template for your input file")
	return 0
}

func showStats(cfg *config.Config, stdout io.Writer) {
	tracker := progress.Open(cfg.ProgressFilePath)
	s := tracker.Summary()
	fmt.Fprintln(stdout, "📊 Detailed Statistics:")
	fmt.Fprintf(stdout, "   Total domains: %d\n", s.Total)
	fmt.Fprintf(stdout, "   Processed: %d\n", s.Processed)
	fmt.Fprintf(stdout, "   Successful: %d\n", s.Successful)
	fmt.Fprintf(stdout, "   Errors: %d\n", s.Errors)
	fmt.Fprintf(stdout, "   Remaining: %d\n", s.Remaining)
	fmt.Fprintf(stdout, "   Completion: %.1f%%\n", s.CompletionPercent)
	if s.HasElapsed {
		fmt.Fprintf(stdout, "   Elapsed time: %s\n", s.Elapsed.Round(time.Second))
	}
	if s.HasEstimate {
		fmt.Fprintf(stdout, "   Estimated remaining: %s\n", s.EstimatedRemaining)
	}
	failed, entries := failedInOrder(cfg)
	if len(failed) == 0 {
		return
	}
	fmt.Fprintf(stdout, "\n❌ Failed domains (%d):\n", len(failed))
	for _, id := range failed[:min(5, len(failed))] {
		fmt.Fprintf(stdout, "   %s: %s\n", id, orDefault(entries[id].ErrorMessage, "Unknown error"))
	}
	if len(failed) > 5 {
		fmt.Fprintf(stdout, "   ... and %d more\n", len(failed)-5)
	}
}
