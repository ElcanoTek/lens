// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package deploytest holds the hermetic tests for the bash deployment
// scripts (install.sh, scripts/*.sh, scripts/lib/*.sh). The scripts stay
// bash, like Fleet's; these tests source them in a scratch tree with stub
// tools on PATH, so nothing touches the host's packages, sshd, hostname or
// systemd. It has no non-test code.
package deploytest
