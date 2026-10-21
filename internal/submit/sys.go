// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package submit

import (
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

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

// execReplace resolves argv[0] on PATH and execve's it.
func execReplace(argv, env []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, env) //nolint:gosec // fixed sudo re-run of this binary; argv, never a shell
}
