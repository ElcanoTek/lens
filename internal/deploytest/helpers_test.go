// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package deploytest

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repo is the repository root (this file lives in internal/deploytest).
func repo(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func lib(t *testing.T) string { return filepath.Join(repo(t), "scripts/lib/deploy.sh") }

func read(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repo(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type result struct {
	code           int
	stdout, stderr string
}

type opt func(*exec.Cmd)

func stdin(s string) opt { return func(c *exec.Cmd) { c.Stdin = strings.NewReader(s) } }

// env replaces the whole environment.
func env(kv ...string) opt { return func(c *exec.Cmd) { c.Env = kv } }

// extend adds to the inherited environment.
func extend(kv ...string) opt { return func(c *exec.Cmd) { c.Env = append(os.Environ(), kv...) } }

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs %s", tool)
		}
	}
}

func bashPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("needs bash")
	}
	return p
}

// bash runs `bash -euc script test args...`.
func bash(t *testing.T, script string, args []string, opts ...opt) result {
	t.Helper()
	return runCmd(t, append([]string{bashPath(t), "-euc", script, "test"}, args...), opts...)
}

func runCmd(t *testing.T, argv []string, opts ...opt) result {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	for _, o := range opts {
		o(cmd)
	}
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run %v: %v", argv, err)
		}
		code = ee.ExitCode()
	}
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// stub writes an executable shell script named name into dir.
func stub(t *testing.T, dir, name, body string) {
	t.Helper()
	write(t, filepath.Join(dir, name), "#!/bin/sh\n"+body+"\n", 0o755)
}

// linkTools symlinks real tools into dir, for tests that run with a PATH
// holding nothing but stubs.
func linkTools(t *testing.T, dir string, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("needs %s", tool)
		}
		if err := os.Symlink(p, filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
}

// between returns src[from:to) by marker strings, failing when either is gone.
func between(t *testing.T, src, from, to string) string {
	t.Helper()
	i := strings.Index(src, from)
	if i < 0 {
		t.Fatalf("marker %q not found", from)
	}
	j := strings.Index(src[i:], to)
	if j < 0 {
		t.Fatalf("marker %q not found after %q", to, from)
	}
	return src[i : i+j]
}
