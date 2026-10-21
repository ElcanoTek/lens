// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package opscli is the operator half of the lens binary: the commands the
// Python release shipped as the bash script deploy/lens-cli
// (/usr/local/bin/lens), now subcommands of the one Go binary that
// /usr/local/bin/lens links to.
//
//	lens start|stop|restart|status|logs   systemd / journald for lens.service
//	lens update|rebuild|host               exec scripts/{update,host}.sh from the source checkout
//	lens env show|edit                     redacted view / safe edit of /opt/lens/.env
//	lens tls status|reload|restart         Caddy controls
//	lens doctor                            read-only diagnostics (internal/doctor)
//
// Commands that need root re-run themselves (or the script they hand off to)
// under sudo with a fixed --preserve-env list, exactly as the bash CLI did,
// so an operator never has to remember which verbs need sudo. Commands that
// must run as the service user (lens token, lens submit, lens access) call
// AsServiceUser first.
//
// Every process and filesystem side effect goes through CLI's function fields
// so the dispatch is testable without root, systemd or sudo.
package opscli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/ElcanoTek/lens/internal/doctor"
)

// Service is the systemd unit the operator commands drive.
const Service = "lens.service"

// preserveEnv is what crosses sudo. The first seven are the bash CLI's list
// (update.sh reads them); LENS_APP_USER and LENS_HOME are added because the
// Go CLI re-executes itself under sudo, and the re-run must see the same
// install the operator pointed it at.
var preserveEnv = []string{
	"LENS_SRC_DIR", "LENS_APP_DIR", "LENS_UPDATE_YES", "LENS_UPDATE_BRANCH",
	"LENS_UPDATE_SKIP_DEEP", "LENS_KEEP_ROLLBACKS", "EDITOR",
	"LENS_APP_USER", "LENS_HOME",
}

// serviceUserEnv is forwarded explicitly (sudo and runuser both scrub the
// environment) when a command re-runs as the service user.
var serviceUserEnv = []string{
	"LENS_APP_DIR", "LENS_SRC_DIR", "LENS_APP_USER", "LENS_ACCESS_DB",
	"LENS_API_TOKEN_DB", "LENS_PUBLIC_URL",
}

// CLI holds every side effect of the operator commands. New fills each field
// with the real implementation; tests replace them.
type CLI struct {
	Getenv     func(string) string
	LookupEnv  func(string) (string, bool)
	Geteuid    func() int
	LookupUser func(name string) (uid, gid int, err error)
	Executable func() (string, error)
	// Exec replaces the current process with argv (resolved on PATH), after
	// changing to dir when dir is not empty. It returns only on failure.
	Exec func(argv, env []string, dir string) error
	// Run runs argv to completion with the CLI's stdio and returns its exit
	// status (127 when it cannot start).
	Run      func(ctx context.Context, argv []string) int
	LookPath func(string) (string, error)
	Chown    func(path string, uid, gid int) error
	Chmod    func(path string, mode os.FileMode) error
	Doctor   func(args []string) int
	// Root prefixes fixed host paths (/etc/caddy/...) for tests.
	Root string

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// New returns a CLI wired to the real machine.
func New() *CLI {
	c := &CLI{
		Getenv:     os.Getenv,
		LookupEnv:  os.LookupEnv,
		Geteuid:    os.Geteuid,
		LookupUser: lookupUser,
		Executable: executable,
		Exec:       execReplace,
		LookPath:   exec.LookPath,
		Chown:      os.Chown,
		Chmod:      os.Chmod,
		Doctor:     doctor.Main,
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
	}
	c.Run = c.runChild
	return c
}

// Main runs operator command cmd with args against the real machine and
// returns the process exit status. cmd is one of start, stop, restart,
// status, logs, update, rebuild, host, env, tls, doctor or help.
func Main(cmd string, args []string) int { return New().Main(cmd, args) }

// AsServiceUser makes sure the current lens invocation runs as the service
// account. When the caller already is that account it returns (false, 0) and
// the command proceeds in-process. Otherwise it re-executes
// `<this binary> args...` as the service user (runuser when root, sudo -u
// otherwise) from the Lens home, and only returns — (true, status) — when
// that fails. args are os.Args[1:], i.e. they start with the subcommand.
//
// The re-run's working directory is the app dir and LENS_HOME is set to it,
// because runuser keeps the caller's cwd (often /root, unreadable to the
// service user) and the Lens home defaults to the working directory. Resolve
// relative file arguments to absolute paths before calling.
func AsServiceUser(args []string) (done bool, code int) { return New().AsServiceUser(args) }

// AppDir is the Lens home the operator commands act on (LENS_APP_DIR,
// default /opt/lens).
func AppDir() string { return New().appDir() }

// ServiceUser is the service account (LENS_APP_USER, default lens).
func ServiceUser() string { return New().serviceUser() }

func (c *CLI) appDir() string  { return firstNonEmpty(c.Getenv("LENS_APP_DIR"), "/opt/lens") }
func (c *CLI) srcDir() string  { return firstNonEmpty(c.Getenv("LENS_SRC_DIR"), "/opt/lens-src") }
func (c *CLI) envFile() string { return filepath.Join(c.appDir(), ".env") }
func (c *CLI) serviceUser() string {
	return firstNonEmpty(c.Getenv("LENS_APP_USER"), "lens")
}

func (c *CLI) hostPath(p string) string {
	if c.Root == "" {
		return p
	}
	return filepath.Join(c.Root, p)
}

// Main dispatches one operator command.
func (c *CLI) Main(cmd string, args []string) int {
	switch cmd {
	case "start", "stop", "restart", "status":
		return c.execAsRoot("systemctl", cmd, Service)
	case "logs":
		if len(args) > 0 {
			return c.execAsRoot(append([]string{"journalctl", "-u", Service}, args...)...)
		}
		return c.execAsRoot("journalctl", "-fu", Service)
	case "update":
		if !c.haveCheckout() {
			return 1
		}
		return c.execAsRoot(append([]string{"bash", filepath.Join(c.srcDir(), "scripts", "update.sh")}, args...)...)
	case "rebuild":
		// Rebuild and restart whatever is currently checked out, without
		// fetching or fast-forwarding the branch. Useful after a manual
		// rollback or when you know the checkout is already correct.
		if !c.haveCheckout() {
			return 1
		}
		return c.execAsRoot(append([]string{"env", "LENS_UPDATE_NO_PULL=1", "LENS_UPDATE_YES=1",
			"bash", filepath.Join(c.srcDir(), "scripts", "update.sh")}, args...)...)
	case "host":
		return c.execAsRoot(append([]string{"bash", filepath.Join(c.srcDir(), "scripts", "host.sh")}, args...)...)
	case "doctor":
		// Doctor reads root-only state (sshd -T, .env, the auth databases)
		// and probes podman as the service user, so it runs as root like the
		// bash CLI's `sudo scripts/doctor.sh`.
		if c.Geteuid() != 0 {
			return c.reexecAsRoot("doctor", args)
		}
		return c.Doctor(args)
	case "env":
		return c.env(args)
	case "tls":
		return c.tls(args)
	case "help", "-h", "--help", "":
		Help(c.Stdout)
		return 0
	default:
		fmt.Fprintf(c.Stderr, "unknown command: %s\n", cmd)
		return 2
	}
}

func (c *CLI) haveCheckout() bool {
	if fi, err := os.Stat(filepath.Join(c.srcDir(), ".git")); err == nil && fi.IsDir() {
		return true
	}
	fmt.Fprintf(c.Stderr, "no source checkout at %s\n", c.srcDir())
	return false
}

// sudoPrefix is empty for root and the sudo invocation otherwise.
func (c *CLI) sudoPrefix() []string {
	if c.Geteuid() == 0 {
		return nil
	}
	return []string{"sudo", "--preserve-env=" + strings.Join(preserveEnv, ",")}
}

// execAsRoot replaces this process with argv, through sudo unless already
// root.
func (c *CLI) execAsRoot(argv ...string) int {
	return c.exec(append(c.sudoPrefix(), argv...), "")
}

// reexecAsRoot re-runs `lens <cmd> args...` under sudo.
func (c *CLI) reexecAsRoot(cmd string, args []string) int {
	exe, err := c.Executable()
	if err != nil {
		fmt.Fprintf(c.Stderr, "lens: cannot locate this binary: %v\n", err)
		return 1
	}
	return c.exec(append(append(c.sudoPrefix(), exe, cmd), args...), "")
}

func (c *CLI) exec(argv []string, dir string) int {
	if err := c.Exec(argv, os.Environ(), dir); err != nil {
		fmt.Fprintf(c.Stderr, "lens: %s: %v\n", argv[0], err)
		return 127
	}
	return 0
}

// AsServiceUser is the method form of the package-level AsServiceUser.
func (c *CLI) AsServiceUser(args []string) (done bool, code int) {
	name := c.serviceUser()
	uid, _, err := c.LookupUser(name)
	if err != nil {
		fmt.Fprintf(c.Stderr, "lens: service account %q missing — run bootstrap.sh\n", name)
		return true, 1
	}
	if c.Geteuid() == uid {
		return false, 0
	}
	exe, err := c.Executable()
	if err != nil {
		fmt.Fprintf(c.Stderr, "lens: cannot locate this binary: %v\n", err)
		return true, 1
	}
	app := c.appDir()
	envArgs := []string{"env", "LENS_HOME=" + app}
	for _, k := range serviceUserEnv {
		if v, ok := c.LookupEnv(k); ok {
			envArgs = append(envArgs, k+"="+v)
		}
	}
	var argv []string
	if c.Geteuid() == 0 {
		argv = []string{"runuser", "-u", name, "--"}
	} else {
		argv = []string{"sudo", "-u", name}
	}
	argv = append(append(append(argv, envArgs...), exe), args...)
	return true, c.exec(argv, app)
}

// Help prints the operator help text.
func Help(w io.Writer) {
	fmt.Fprint(w, `lens — Elcano Lens: domain/app analysis service and its operator CLI

Service
  lens start | stop | restart | status
  lens logs [journalctl args...]
  lens serve [--addr 127.0.0.1:8808]   run the dashboard/API/MCP server (systemd does this)

Deploy and maintain
  lens update [--yes]           fetch + staged Go build + restart; rollback on failure
                                (unattended: LENS_UPDATE_YES=1 lens update)
  lens rebuild                  rebuild + restart current checkout
                                (skip fetch — useful after a manual rollback)
  lens doctor [--json] [--strict] [--public-url https://HOST]
                                read-only diagnostics (including auth + runtime)
  lens host check|update|tools|upgrade [--yes] [--dry-run]
                                packages, optional Node/npm/Go, stable Fedora
  lens env [show]               print /opt/lens/.env (secrets redacted)
  lens env edit                 edit /opt/lens/.env in $EDITOR, then: lens restart
  lens tls status|reload|restart   Caddy controls (if installed)

Access and jobs
  lens access grant <email>     allow a central-auth account on this Lens instance
  lens access revoke <email>    revoke access and every local Lens session
  lens access list              list enabled central-auth accounts
  lens submit FILE [options]    queue a dashboard run from a file on this host
                                (--name, --categories JSON, --ctv, model overrides)
  lens token create --label NAME [--scope jobs:read|jobs:run]...
                                mint a scoped API token for Fleet (shown once)
  lens token list | revoke ID   show tokens (never secrets) / revoke one

Pipeline
  lens run [flags...]           run the analysis pipeline on input.csv (see lens run --help)
  lens manage setup|validate|progress|reset|reset-failed|stats|sample

  lens --version                print the build version and revision
`)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func lookupUser(name string) (uid, gid int, err error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	if uid, err = strconv.Atoi(u.Uid); err != nil {
		return 0, 0, err
	}
	if gid, err = strconv.Atoi(u.Gid); err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// executable is the binary's real path: /usr/local/bin/lens is a symlink to
// /opt/lens/bin/lens, and re-executions should name the release binary.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// execReplace is the production Exec: resolve argv[0] on PATH, change to dir
// (falling back to / when the caller cannot enter it), then execve.
func execReplace(argv, env []string, dir string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	if dir != "" {
		if err := os.Chdir(dir); err != nil {
			_ = os.Chdir("/")
		}
	}
	return syscall.Exec(path, argv, env) //nolint:gosec // operator command dispatch; argv, never a shell
}

func (c *CLI) runChild(ctx context.Context, argv []string) int {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // operator-chosen editor / fixed tools
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(c.Stderr, "lens: %s: %v\n", argv[0], err)
		return 127
	}
	return 0
}
