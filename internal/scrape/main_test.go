// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package scrape

import (
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/ElcanoTek/lens/internal/logx"
)

func TestMain(m *testing.M) {
	logx.SetOutput(io.Discard, slog.LevelError)
	os.Exit(m.Run())
}
