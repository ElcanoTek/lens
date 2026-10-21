// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Command lens is the single Lens binary: the pipeline (`lens run`), the
// dashboard/API/MCP server (`lens serve`) and every operator command.
package main

import (
	"fmt"
	"os"

	"github.com/ElcanoTek/lens/internal/apitoken"
	"github.com/ElcanoTek/lens/internal/auth"
	"github.com/ElcanoTek/lens/internal/config"
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
		config.LoadDotenv()
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
