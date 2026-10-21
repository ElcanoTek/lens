// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"os"
	"testing"
)

func TestSmokeRender(t *testing.T) {
	h := newHarness(t, false)
	h.writeInput("list.csv", "Domain\nexample.com\n")
	resp, body := h.get("/")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if p := os.Getenv("WEB_SMOKE_OUT"); p != "" {
		_ = os.WriteFile(p, []byte(body), 0o600)
	}
}
