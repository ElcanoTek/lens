// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
//
// Cases ported from python-dotenv 1.2.3 tests/test_main.py (BSD-3-Clause;
// see LICENSE in this directory) and Lens's tests/test_dotenv_envfile.py.

package dotenv

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// clearEnv empties the process environment for the test and restores it
// afterwards, like mock.patch.dict(os.environ, env, clear=True).
func clearEnv(t *testing.T, env map[string]string) {
	t.Helper()
	saved := os.Environ()
	os.Clearenv()
	for k, v := range env {
		_ = os.Setenv(k, v)
	}
	t.Cleanup(func() {
		os.Clearenv()
		for _, kv := range saved {
			k, v, _ := strings.Cut(kv, "=")
			_ = os.Setenv(k, v)
		}
	})
}

func envMap() map[string]string {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func asMap(pairs []Pair) map[string]any {
	m := map[string]any{}
	for _, p := range pairs {
		if p.Value == nil {
			m[p.Key] = nil
		} else {
			m[p.Key] = *p.Value
		}
	}
	return m
}

func TestValuesInterpolation(t *testing.T) {
	cases := []struct {
		env         map[string]string
		in          string
		interpolate bool
		want        map[string]any
	}{
		// Defined in environment, with and without interpolation
		{map[string]string{"b": "c"}, "a=$b", false, map[string]any{"a": "$b"}},
		{map[string]string{"b": "c"}, "a=$b", true, map[string]any{"a": "$b"}},
		{map[string]string{"b": "c"}, "a=${b}", false, map[string]any{"a": "${b}"}},
		{map[string]string{"b": "c"}, "a=${b}", true, map[string]any{"a": "c"}},
		{map[string]string{"b": "c"}, "a=${b:-d}", false, map[string]any{"a": "${b:-d}"}},
		{map[string]string{"b": "c"}, "a=${b:-d}", true, map[string]any{"a": "c"}},
		// Defined in file
		{nil, "b=c\na=${b}", true, map[string]any{"a": "c", "b": "c"}},
		// Undefined
		{nil, "a=${b}", true, map[string]any{"a": ""}},
		{nil, "a=${b:-d}", true, map[string]any{"a": "d"}},
		// With quotes
		{map[string]string{"b": "c"}, `a="${b}"`, true, map[string]any{"a": "c"}},
		{map[string]string{"b": "c"}, "a='${b}'", true, map[string]any{"a": "c"}},
		// With surrounding text
		{map[string]string{"b": "c"}, "a=x${b}y", true, map[string]any{"a": "xcy"}},
		// Self-referential
		{map[string]string{"a": "b"}, "a=${a}", true, map[string]any{"a": "b"}},
		{nil, "a=${a}", true, map[string]any{"a": ""}},
		{map[string]string{"a": "b"}, "a=${a:-c}", true, map[string]any{"a": "b"}},
		{nil, "a=${a:-c}", true, map[string]any{"a": "c"}},
		// Reused
		{map[string]string{"b": "c"}, "a=${b}${b}", true, map[string]any{"a": "cc"}},
		// Re-defined and used in file
		{map[string]string{"b": "c"}, "b=d\na=${b}", true, map[string]any{"a": "d", "b": "d"}},
		{nil, "a=b\na=c\nd=${a}", true, map[string]any{"a": "c", "d": "c"}},
		{nil, "a=b\nc=${a}\nd=e\nc=${d}", true, map[string]any{"a": "b", "c": "e", "d": "e"}},
		// A bare key earlier in the file resolves to "", not the default.
		{nil, "a\nb=${a:-x}", true, map[string]any{"a": nil, "b": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			clearEnv(t, tc.env)
			got, err := Values(strings.NewReader(tc.in), tc.interpolate)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(asMap(got), tc.want) {
				t.Errorf("got %v, want %v", asMap(got), tc.want)
			}
		})
	}
}

func TestValuesOrderIsDictOrder(t *testing.T) {
	got, err := Values(strings.NewReader("a=1\nb=2\na=3\n"), false)
	if err != nil {
		t.Fatal(err)
	}
	want := []Pair{{"a", sp("3")}, {"b", sp("2")}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestValuesFileAndReadFile(t *testing.T) {
	p := writeEnv(t, "a=b")
	for _, f := range []func(string) ([]Pair, error){ReadFile, func(p string) ([]Pair, error) { return ValuesFile(p, false) }} {
		got, err := f(p)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(asMap(got), map[string]any{"a": "b"}) {
			t.Errorf("got %v", asMap(got))
		}
	}
	if _, err := ReadFile(filepath.Join(t.TempDir(), "nx")); err == nil {
		t.Error("missing file should be an error for ReadFile")
	}
}

func TestInvalidUTF8IsAnError(t *testing.T) {
	if _, err := Parse(strings.NewReader("a=\xff\n")); err == nil {
		t.Error("want error")
	}
}

func TestUniversalNewlines(t *testing.T) {
	got, err := Values(strings.NewReader("a=\"x\r\ny\"\r\nb=c\r"), false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(asMap(got), map[string]any{"a": "x\ny", "b": "c"}) {
		t.Errorf("got %v", asMap(got))
	}
}

func TestLoadExistingFile(t *testing.T) {
	clearEnv(t, nil)
	if err := Load(writeEnv(t, "a=b"), false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(envMap(), map[string]string{"a": "b"}) {
		t.Errorf("env = %v", envMap())
	}
}

func TestLoadDisabled(t *testing.T) {
	for _, flag := range []string{"true", "yes", "1", "t", "y", "True", "Yes", "TRUE", "YES", "T", "Y"} {
		t.Run(flag, func(t *testing.T) {
			clearEnv(t, map[string]string{"PYTHON_DOTENV_DISABLED": flag})
			if err := Load(writeEnv(t, "a=b"), false); err != nil {
				t.Fatal(err)
			}
			ok, err := LoadReader(strings.NewReader("a=b"), false)
			if err != nil || ok {
				t.Fatalf("LoadReader = %v, %v", ok, err)
			}
			if !reflect.DeepEqual(envMap(), map[string]string{"PYTHON_DOTENV_DISABLED": flag}) {
				t.Errorf("env = %v", envMap())
			}
		})
	}
}

func TestLoadEnabled(t *testing.T) {
	for _, flag := range []string{"", "false", "no", "0", "f", "n", "False", "No", "FALSE", "NO", "F", "N"} {
		t.Run(flag, func(t *testing.T) {
			clearEnv(t, map[string]string{"PYTHON_DOTENV_DISABLED": flag})
			ok, err := LoadReader(strings.NewReader("a=b"), false)
			if err != nil || !ok {
				t.Fatalf("LoadReader = %v, %v", ok, err)
			}
			if !reflect.DeepEqual(envMap(), map[string]string{"PYTHON_DOTENV_DISABLED": flag, "a": "b"}) {
				t.Errorf("env = %v", envMap())
			}
		})
	}
}

func TestLoadDoesntDisableItself(t *testing.T) {
	clearEnv(t, nil)
	ok, err := LoadReader(strings.NewReader("PYTHON_DOTENV_DISABLED=true"), false)
	if err != nil || !ok {
		t.Fatalf("LoadReader = %v, %v", ok, err)
	}
	if !reflect.DeepEqual(envMap(), map[string]string{"PYTHON_DOTENV_DISABLED": "true"}) {
		t.Errorf("env = %v", envMap())
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	clearEnv(t, nil)
	if err := Load(filepath.Join(t.TempDir(), ".does_not_exist"), false); err != nil {
		t.Fatal(err)
	}
	if err := Load(t.TempDir(), false); err != nil { // a directory
		t.Fatal(err)
	}
	if len(envMap()) != 0 {
		t.Errorf("env = %v", envMap())
	}
}

func TestLoadUnreadableFileIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("root can read 0000 files")
	}
	p := writeEnv(t, "a=b")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	if err := Load(p, false); err == nil {
		t.Error("want permission error")
	}
}

func TestLoadOverride(t *testing.T) {
	cases := []struct {
		content  string
		override bool
		want     map[string]string
	}{
		{"a=b", false, map[string]string{"a": "c"}},
		{"a=b", true, map[string]string{"a": "b"}},
		// redefine var used in file
		{"a=b\nd=\"${a}\"", false, map[string]string{"a": "c", "d": "c"}},
		{"a=b\nd=\"${a}\"", true, map[string]string{"a": "b", "d": "b"}},
		// bare keys never set anything
		{"x\na=b", true, map[string]string{"a": "b"}},
	}
	for _, tc := range cases {
		clearEnv(t, map[string]string{"a": "c"})
		if err := Load(writeEnv(t, tc.content), tc.override); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(envMap(), tc.want) {
			t.Errorf("%q override=%v: env = %v, want %v", tc.content, tc.override, envMap(), tc.want)
		}
	}
}

func TestLoadReaderUTF8(t *testing.T) {
	clearEnv(t, nil)
	ok, err := LoadReader(strings.NewReader("a=\u00e0"), false)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if os.Getenv("a") != "\u00e0" {
		t.Errorf("a = %q", os.Getenv("a"))
	}
	ok, err = LoadReader(strings.NewReader("# nothing\n"), false)
	if err != nil || ok {
		t.Errorf("empty file: %v %v", ok, err)
	}
}

func prepareHierarchy(t *testing.T) (root, leaf string) {
	t.Helper()
	root = t.TempDir()
	leaf = filepath.Join(root, "child1", "child2", "child3", "child4")
	if err := os.MkdirAll(leaf, 0o750); err != nil {
		t.Fatal(err)
	}
	return root, leaf
}

func TestFind(t *testing.T) {
	root, leaf := prepareHierarchy(t)
	t.Chdir(leaf)
	if got := Find(); got != "" {
		t.Skipf("a .env exists above the temp dir (%s); cannot test the not-found case", got)
	}
	p := filepath.Join(root, ".env")
	if err := os.WriteFile(p, []byte("TEST=test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(p)
	got, _ := filepath.EvalSymlinks(Find())
	if got != want {
		t.Errorf("Find() = %q, want %q", got, want)
	}
	// A directory named .env is not a match.
	if err := os.Mkdir(filepath.Join(leaf, ".env"), 0o750); err != nil {
		t.Fatal(err)
	}
	if got, _ := filepath.EvalSymlinks(FindFrom(leaf, ".env")); got != want {
		t.Errorf("FindFrom skipped dir: %q", got)
	}
}

func TestSetKey(t *testing.T) {
	cases := []struct{ before, key, value, after string }{
		{"", "a", "", "a=''\n"},
		{"", "a", "b", "a='b'\n"},
		{"", "a", "'b'", "a='\\'b\\''\n"},
		{"", "a", `"b"`, "a='\"b\"'\n"},
		{"", "a", "b'c", "a='b\\'c'\n"},
		{"", "a", `b"c`, "a='b\"c'\n"},
		{"a=b", "a", "c", "a='c'\n"},
		{"a=b\n", "a", "c", "a='c'\n"},
		{"a=b\n\n", "a", "c", "a='c'\n\n"},
		{"a=b\nc=d", "a", "e", "a='e'\nc=d"},
		{"a=b\nc=d\ne=f", "c", "g", "a=b\nc='g'\ne=f"},
		{"a=b\n", "c", "d", "a=b\nc='d'\n"},
		{"a=b", "c", "d", "a=b\nc='d'\n"},
		{"", "a", `b\c`, "a='b\\\\c'\n"},
		{"", "a", `b\`, "a='b\\\\'\n"},
		{"", "a", `b\'c`, "a='b\\\\\\'c'\n"},
	}
	for _, tc := range cases {
		p := writeEnv(t, tc.before)
		if err := SetKey(p, tc.key, tc.value, SetKeyOptions{}); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(p)
		if string(got) != tc.after {
			t.Errorf("SetKey(%q, %q, %q) wrote %q, want %q", tc.before, tc.key, tc.value, got, tc.after)
		}
	}
}

func TestSetKeyOptions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nx")
	if err := SetKey(p, "foo", "bar", SetKeyOptions{Quote: QuoteAuto, Export: true}); err != nil {
		t.Fatal(err)
	}
	if err := SetKey(p, "b", "x y", SetKeyOptions{Quote: QuoteAuto}); err != nil {
		t.Fatal(err)
	}
	if err := SetKey(p, "c", "x y", SetKeyOptions{Quote: QuoteNever}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "export foo=bar\nb='x y'\nc=x y\n" {
		t.Errorf("wrote %q", got)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("new file mode %v", st.Mode().Perm())
	}
}

func TestSetKeyRoundTrips(t *testing.T) {
	for _, value := range []string{`C:\Users`, `C:\Users\`, `\d+`, `back\`, `a\'b`, "it's", `say "hi"`, `a\nb`, "plain", ""} {
		p := writeEnv(t, "")
		if err := SetKey(p, "a", value, SetKeyOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := SetKey(p, "b", "sentinel", SetKeyOptions{}); err != nil {
			t.Fatal(err)
		}
		got, err := ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		m := asMap(got)
		if m["a"] != value || m["b"] != "sentinel" {
			t.Errorf("round trip of %q gave %v", value, m)
		}
	}
}

func TestSetKeyPreservesFileMode(t *testing.T) {
	p := writeEnv(t, "a=x\n")
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := SetKey(p, "a", "y", SetKeyOptions{}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
}

func TestSetKeySymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.env")
	if err := os.WriteFile(target, []byte("a=x\n"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".env")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := SetKey(link, "a", "y", SetKeyOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "a=x\n" {
		t.Errorf("target changed: %q", got)
	}
	st, _ := os.Lstat(link)
	if st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0o600 {
		t.Errorf("link not replaced by a 0600 file: %v", st.Mode())
	}

	// Symlink to a missing file, not followed.
	link2 := filepath.Join(dir, "two.env")
	missing := filepath.Join(dir, "nx")
	if err := os.Symlink(missing, link2); err != nil {
		t.Fatal(err)
	}
	if err := SetKey(link2, "a", "x", SetKeyOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("missing target was created")
	}
	if got, _ := os.ReadFile(link2); string(got) != "a='x'\n" {
		t.Errorf("wrote %q", got)
	}

	// Followed.
	link3 := filepath.Join(dir, "three.env")
	if err := os.Symlink(target, link3); err != nil {
		t.Fatal(err)
	}
	if err := SetKey(link3, "a", "y", SetKeyOptions{FollowSymlinks: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "a='y'\n" {
		t.Errorf("target = %q", got)
	}
	if st, _ := os.Lstat(link3); st.Mode()&os.ModeSymlink == 0 {
		t.Error("followed link was replaced")
	}
}

func TestSetKeyPermissionError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("root can access files even with 000 permissions")
	}
	p := writeEnv(t, "")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	if err := SetKey(p, "a", "b", SetKeyOptions{}); err == nil {
		t.Error("want error")
	}
	_ = os.Chmod(p, 0o600)
	if got, _ := os.ReadFile(p); len(got) != 0 {
		t.Errorf("file changed: %q", got)
	}
}

// From tests/test_dotenv_envfile.py: the shapes bootstrap writes.
func TestBootstrapWrittenValuesRoundTrip(t *testing.T) {
	for _, value := range []string{
		"sk-or-v1-0123456789abcdef",
		"MCowBQYDK2VwAyEAq8+/mXk3c2VjcmV0LWtleS1ub3QtcmVhbA==",
		"https://lens.example.com",
		"$(touch /tmp/pwned)",
		"${HOME}", // stays literal: callers read with interpolate=false
		"with spaces # and a hash",
		"it's quoted",
		"",
	} {
		p := writeEnv(t, "# operator comment\nOTHER=kept\n")
		if err := SetKey(p, "OPENROUTER_API_KEY", value, SetKeyOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := SetKey(p, "LENS_AUTH_MODE", "central", SetKeyOptions{}); err != nil {
			t.Fatal(err)
		}
		got, err := ValuesFile(p, false)
		if err != nil {
			t.Fatal(err)
		}
		want := []Pair{{"OTHER", sp("kept")}, {"OPENROUTER_API_KEY", sp(value)}, {"LENS_AUTH_MODE", sp("central")}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %v", value, asMap(got))
		}
		raw, _ := os.ReadFile(p)
		if !strings.HasPrefix(string(raw), "# operator comment\n") {
			t.Errorf("comment lost: %q", raw)
		}
	}
}

func TestHandEditedEnvironmentFileShapes(t *testing.T) {
	p := writeEnv(t, "# Lens settings\n"+
		"AUTH_SIGNING_PUBKEY=abc+/=\n"+
		"LENS_PUBLIC_URL=\"https://lens.example.com\"  # trailing comment\n"+
		"export LENS_ACCESS_DB=/var/lib/lens/access.db\n"+
		"TYPESAFE_API_KEY=\n"+
		"  INDENTED = value with spaces  \n"+
		"BARE_FLAG\n"+
		"\r\n"+
		"MULTI=\"line one\\nline two\"\n")
	got, err := ValuesFile(p, false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"AUTH_SIGNING_PUBKEY": "abc+/=",
		"LENS_PUBLIC_URL":     "https://lens.example.com",
		"LENS_ACCESS_DB":      "/var/lib/lens/access.db",
		"TYPESAFE_API_KEY":    "",
		"INDENTED":            "value with spaces",
		"BARE_FLAG":           nil,
		"MULTI":               "line one\nline two",
	}
	if !reflect.DeepEqual(asMap(got), want) {
		t.Errorf("got %v", asMap(got))
	}
}
