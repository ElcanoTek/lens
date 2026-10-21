// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Command lens is the single Lens binary: the pipeline (`lens run`), the
// dashboard/API/MCP server (`lens serve`) and every operator command.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ElcanoTek/lens/internal/apitoken"
	"github.com/ElcanoTek/lens/internal/auth"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/dotenv"
	"github.com/ElcanoTek/lens/internal/opscli"
	"github.com/ElcanoTek/lens/internal/pipeline"
	"github.com/ElcanoTek/lens/internal/submit"
	"github.com/ElcanoTek/lens/internal/version"
	"github.com/ElcanoTek/lens/internal/web"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		opscli.Help(os.Stdout)
		return 0
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "--version", "-V", "version":
		// Deploy scripts compare this with the commit they built; it must
		// not depend on .env or config.json.
		fmt.Println(version.String())
		return 0
	case "help", "-h", "--help":
		opscli.Help(os.Stdout)
		return 0

	case "run":
		config.LoadDotenv()
		return pipeline.RunMain(rest)
	case "serve":
		config.LoadDotenv()
		return web.Main(rest)
	case "manage":
		config.LoadDotenv()
		return pipeline.ManageMain(rest)

	case "token", "access":
		// Both databases stay owned by the service user, so these run as it
		// (re-exec via runuser/sudo), from the app directory, where .env
		// supplies LENS_API_TOKEN_DB / LENS_ACCESS_DB / LENS_PUBLIC_URL.
		if done, code := opscli.AsServiceUser(args); done {
			return code
		}
		loadInstanceEnv()
		if cmd == "token" {
			return apitoken.Main(rest)
		}
		return auth.AccessMain(rest)
	case "submit":
		// submit does its own root phase (spool entry) and hand-off.
		return submit.Main(rest)
	case "migrate-access-db":
		return auth.MigrateAccessDBMain(rest)

	case "doctor", "start", "stop", "restart", "status", "logs", "update", "rebuild", "host", "env", "tls":
		return opscli.Main(cmd, rest)
	}
	fmt.Fprintf(os.Stderr, "unknown command: %s (try lens --help)\n", cmd)
	return 2
}

// loadInstanceEnv loads the instance's .env for the service-user commands:
// LENS_HOME's (the re-exec sets it to the app dir), else the app dir's
// (LENS_APP_DIR, default /opt/lens), as the bash CLI read $APP_DIR/.env.
// Only when neither has one does it fall back to the nearest .env above the
// working directory, which for an operator who is already the service user
// (or whose sudo kept a cwd outside the app dir) is the wrong file or none.
func loadInstanceEnv() {
	for _, dir := range []string{os.Getenv("LENS_HOME"), opscli.AppDir()} {
		if dir = strings.TrimSpace(dir); dir == "" {
			continue
		}
		path := filepath.Join(dir, ".env")
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() { //nolint:gosec // G703: the operator-configured Lens home, not request input
			_ = dotenv.Load(path, false)
			return
		}
	}
	config.LoadDotenv()
}
