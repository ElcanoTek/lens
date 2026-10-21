// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RunOptions bounds one external command.
type RunOptions struct {
	Timeout time.Duration // zero means defaultCommandTimeout
	Dir     string        // working directory; "" inherits doctor's
}

// Runner executes name with args and returns its exit code and trimmed
// stdout. A command that cannot start or exceeds its timeout returns
// (127, ""), so a hung probe reads as a failure, never as success.
type Runner func(ctx context.Context, opts RunOptions, name string, args ...string) (int, string)

// Account is the subset of a passwd/group lookup doctor needs.
type Account struct {
	UID    int
	GID    int
	Groups []int // supplementary group ids
}

// FileInfo is the subset of stat(2) doctor needs.
type FileInfo struct {
	Mode  fs.FileMode
	UID   int
	GID   int
	IsDir bool
}

// Host is every side effect-free dependency on the machine. Zero fields fall
// back to the real implementation (see DefaultHost).
type Host struct {
	Run        Runner
	LookPath   func(name string) bool
	Geteuid    func() int
	LookupUser func(name string) (Account, error)
	Stat       func(path string) (FileInfo, error)
	SameFile   func(a, b string) bool
	DiskFree   func(path string) (uint64, error)
	Now        func() time.Time
	// Root prefixes fixed host paths (/etc/os-release, /etc/subuid,
	// /proc/<pid>/exe, /var/lib/lens/*, …) so tests can supply a fake tree.
	// App and Src from Options are never prefixed.
	Root string
}

// DefaultHost returns the real machine.
func DefaultHost() Host { return Host{}.withDefaults() }

func (h Host) withDefaults() Host {
	if h.Run == nil {
		h.Run = ExecRunner
	}
	if h.LookPath == nil {
		h.LookPath = func(name string) bool { _, err := exec.LookPath(name); return err == nil }
	}
	if h.Geteuid == nil {
		h.Geteuid = os.Geteuid
	}
	if h.LookupUser == nil {
		h.LookupUser = lookupUser
	}
	if h.Stat == nil {
		h.Stat = statFile
	}
	if h.SameFile == nil {
		h.SameFile = sameFile
	}
	if h.DiskFree == nil {
		h.DiskFree = diskFree
	}
	if h.Now == nil {
		h.Now = time.Now
	}
	return h
}

func (h Host) path(p string) string {
	if h.Root == "" {
		return p
	}
	return filepath.Join(h.Root, p)
}

// ExecRunner is the real Runner: bounded, stdin closed, stderr discarded.
func ExecRunner(ctx context.Context, opts RunOptions, name string, args ...string) (int, string) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed diagnostic commands; argv, never a shell
	cmd.Dir = opts.Dir
	// A killed command's grandchildren can hold the stdout pipe open; stop
	// waiting for them shortly after the kill.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return 127, ""
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), strings.TrimSpace(string(out))
		}
		return 127, ""
	}
	return 0, strings.TrimSpace(string(out))
}

func lookupUser(name string) (Account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return Account{}, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return Account{}, err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return Account{}, err
	}
	acct := Account{UID: uid, GID: gid}
	// Supplementary groups are best effort, like os.getgrouplist in Python.
	if ids, err := u.GroupIds(); err == nil {
		for _, s := range ids {
			if g, err := strconv.Atoi(s); err == nil {
				acct.Groups = append(acct.Groups, g)
			}
		}
	}
	return acct, nil
}

func statFile(path string) (FileInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return FileInfo{}, err
	}
	out := FileInfo{Mode: fi.Mode(), IsDir: fi.IsDir(), UID: -1, GID: -1}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		out.UID, out.GID = int(st.Uid), int(st.Gid)
	}
	return out, nil
}

func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

func diskFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // Bsize is a positive block size
}
