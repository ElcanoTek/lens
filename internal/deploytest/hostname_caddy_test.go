// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package deploytest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const vultrHosts = "127.0.0.1   localhost localhost.localdomain\n" +
	"::1         localhost localhost.localdomain\n" +
	"127.0.0.1 vultr.guest vultr\n"

const preserveCfg = "# The hostname is set by hand; do not let cloud-init reset it from Vultr metadata.\n" +
	"preserve_hostname: true\nmanage_etc_hosts: false\n"

type hostBox struct {
	out, name, hosts, cfg string
}

// setHostname runs lens_set_hostname on a scratch box with a stub hostnamectl.
func setHostname(t *testing.T, fqdn, current string, hostnamectl bool, hosts string, kv ...string) hostBox {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	name := filepath.Join(dir, "hostname")
	hostsFile := filepath.Join(dir, "hosts")
	cloud := filepath.Join(dir, "cloud")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cloud, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, name, current+"\n", 0o644)
	write(t, hostsFile, hosts, 0o644)
	linkTools(t, bin, "awk", "cat", "mkdir")
	if hostnamectl {
		stub(t, bin, "hostnamectl", `case "$1" in --static) cat '`+name+`' ;; set-hostname) printf '%s\n' "$2" > '`+name+`' ;; esac`)
	}
	r := runCmd(t, []string{bashPath(t), "-c", `set -euo pipefail; . "$1"; info() { echo "INFO $*"; }; ok() { echo "OK $*"; }; ` +
		`warn() { echo "WARN $*"; }; lens_set_hostname "$2"; echo "RC=$?"`, "test", filepath.Join(repo(t), "scripts/lib/hostname.sh"), fqdn},
		env(append([]string{"PATH=" + bin, "LENS_HOSTS_FILE=" + hostsFile, "LENS_CLOUD_DIR=" + cloud}, kv...)...))
	if r.code != 0 || !strings.Contains(r.stdout, "RC=0") {
		t.Fatalf("%+v", r)
	}
	b := hostBox{out: r.stdout, name: strings.TrimSpace(readFile(t, name)), hosts: readFile(t, hostsFile)}
	if cfg := filepath.Join(cloud, "cloud.cfg.d/99-preserve-hostname.cfg"); exists(cfg) {
		b.cfg = readFile(t, cfg)
	}
	return b
}

func TestSetHostnameRenamesAVultrDefaultOnce(t *testing.T) {
	b := setHostname(t, "Lens.Example.com", "vultr.guest", true, vultrHosts)
	want := "127.0.0.1   localhost localhost.localdomain\n::1         localhost localhost.localdomain\n127.0.0.1 lens.example.com lens\n"
	if b.name != "lens.example.com" || b.cfg != preserveCfg || b.hosts != want {
		t.Fatalf("%+v", b)
	}
	again := setHostname(t, "lens.example.com", "lens.example.com", true, b.hosts)
	if again.hosts != b.hosts || !strings.Contains(again.out, "hostname already lens.example.com") {
		t.Fatalf("%+v", again)
	}
}

func TestSetHostnameNeverRewritesALocalhostLine(t *testing.T) {
	b := setHostname(t, "lens.example.com", "localhost", true, "127.0.0.1 localhost\n::1 localhost\n")
	if b.name != "lens.example.com" ||
		b.hosts != "127.0.0.1 localhost\n::1 localhost\n127.0.0.1 lens.example.com lens\n::1 lens.example.com lens\n" {
		t.Fatalf("%+v", b)
	}
}

func TestSetHostnameLeavesADeliberateName(t *testing.T) {
	for _, current := range []string{"yoga", "fedora", "lens.client.example"} {
		b := setHostname(t, "lens.example.com", current, true, vultrHosts)
		if b.name != current || b.hosts != vultrHosts || b.cfg != "" || !strings.Contains(b.out, "hostnamectl set-hostname lens.example.com") {
			t.Fatalf("%s: %+v", current, b)
		}
	}
}

func TestSetHostnameSkips(t *testing.T) {
	for _, tc := range []struct {
		fqdn        string
		hostnamectl bool
		kv          []string
	}{
		{"lens.example.com", true, []string{"LENS_SET_HOSTNAME=0"}},
		{"localhost", true, nil},
		{"10.0.0.5", true, nil},
		{"lens.example.com", false, nil},
		{"lens.example.com", true, []string{"DRY_RUN=1"}},
	} {
		b := setHostname(t, tc.fqdn, "vultr.guest", tc.hostnamectl, vultrHosts, tc.kv...)
		if b.name != "vultr.guest" || b.hosts != vultrHosts || b.cfg != "" {
			t.Fatalf("%+v: %+v", tc, b)
		}
	}
}

func TestSetHostnameForceRenamesADeliberateName(t *testing.T) {
	if b := setHostname(t, "lens.example.com", "yoga", true, vultrHosts, "LENS_SET_HOSTNAME=1"); b.name != "lens.example.com" {
		t.Fatalf("%+v", b)
	}
}

func TestSetHostnameKeepsOtherLoopbackAliases(t *testing.T) {
	b := setHostname(t, "lens.example.com", "vultr.guest", true, "127.0.0.1 localhost\n127.0.0.1 vultr.guest vultr myalias # note\n")
	if b.name != "lens.example.com" || b.hosts != "127.0.0.1 localhost\n127.0.0.1 lens.example.com lens myalias # note\n" {
		t.Fatalf("%+v", b)
	}
}

// caddyHelper sources scripts/lib/caddy-site.sh and runs script with args.
func caddyHelper(t *testing.T, script string, args ...string) result {
	t.Helper()
	return runCmd(t, append([]string{bashPath(t), "-euo", "pipefail", "-c", `source "$1"; ` + script, "bash",
		filepath.Join(repo(t), "scripts/lib/caddy-site.sh")}, args...))
}

const caddyMarker = "# Caddy site block for Lens, imported by /etc/caddy/Caddyfile via"

// The marker is also a compatibility identifier for files earlier releases
// installed; changing both copies would strand those files.
func TestCaddyTemplateContractMatchesInstaller(t *testing.T) {
	template, boot := read(t, "deploy/lens.caddy"), read(t, "scripts/bootstrap.sh")
	r := caddyHelper(t, `printf "%s\n" "$LENS_CADDY_MARKER"`)
	if r.code != 0 || strings.TrimRight(r.stdout, "\n") != caddyMarker {
		t.Fatalf("%+v", r)
	}
	if strings.SplitN(template, "\n", 2)[0] != caddyMarker {
		t.Fatal("deploy/lens.caddy must start with the marker")
	}
	m := regexp.MustCompile(`lens_caddy_adapted_has_site "\$adapted" "\$HOSTNAME_FOR_TLS" "([^"]+)"`).FindStringSubmatch(boot)
	if m == nil {
		t.Fatal("bootstrap no longer checks the rendered site's upstream")
	}
	ups := regexp.MustCompile(`(?m)^\s*reverse_proxy\s+(\S+)\s*$`).FindAllStringSubmatch(template, -1)
	if len(ups) != 1 || ups[0][1] != m[1] {
		t.Fatalf("template upstreams %v, bootstrap expects %s", ups, m[1])
	}
}

func TestCaddyPlanUsesALoadedGlob(t *testing.T) {
	for _, tc := range []struct{ importLine, target, addImport string }{
		{"import Caddyfile.d/*.caddyfile", "Caddyfile.d/lens.caddyfile", "0"},
		{"", "conf.d/lens.caddy", "1"},
		{`import "Caddyfile.d/*.caddyfile"`, "Caddyfile.d/lens.caddyfile", "0"},
		{"import ./conf.d/*", "conf.d/lens.caddy", "0"},
		{"import conf.d/*.caddy", "conf.d/lens.caddy", "0"},
	} {
		dir := t.TempDir()
		caddyfile := filepath.Join(dir, "Caddyfile")
		write(t, caddyfile, tc.importLine+"\n", 0o644)
		r := caddyHelper(t, `lens_caddy_plan "$2"; printf "%s\n%s\n" "$LENS_CADDY_TARGET" "$LENS_CADDY_ADD_IMPORT"`, caddyfile)
		if r.code != 0 || r.stdout != filepath.Join(dir, tc.target)+"\n"+tc.addImport+"\n" {
			t.Fatalf("%+v: %+v", tc, r)
		}
	}
}

func TestCaddyPlanResolvesAbsoluteImport(t *testing.T) {
	dir := t.TempDir()
	caddyfile := filepath.Join(dir, "Caddyfile")
	write(t, caddyfile, `import "`+filepath.Join(dir, "different", "*.caddyfile")+`"`+"\n", 0o644)
	r := caddyHelper(t, `lens_caddy_plan "$2"; printf "%s\n%s\n" "$LENS_CADDY_TARGET" "$LENS_CADDY_ADD_IMPORT"`, caddyfile)
	if r.code != 0 || r.stdout != filepath.Join(dir, "different/lens.caddyfile")+"\n0\n" {
		t.Fatalf("%+v", r)
	}
}

func TestCaddyPlanPreservesLoadedManualCopyAndRemovesOnlyOwnedOrphan(t *testing.T) {
	for _, orphanImported := range []bool{false, true} {
		dir := t.TempDir()
		caddyfile := filepath.Join(dir, "Caddyfile")
		imports := "import Caddyfile.d/*.caddyfile\n"
		if orphanImported {
			imports += "import conf.d/*.caddy\n"
		}
		write(t, caddyfile, imports, 0o644)
		loaded, orphan, unrelated := filepath.Join(dir, "Caddyfile.d/lens.caddyfile"), filepath.Join(dir, "conf.d/lens.caddy"), filepath.Join(dir, "conf.d/other.caddy")
		content := caddyMarker + "\nlens.example {\n}\n"
		write(t, loaded, content, 0o644)
		write(t, orphan, content, 0o644)
		write(t, unrelated, "other.example {\n}\n", 0o644)
		r := caddyHelper(t, `lens_caddy_plan "$2"; printf "%s\n%s\n" "$LENS_CADDY_TARGET" "$LENS_CADDY_ADD_IMPORT"; lens_caddy_remove_stale "$2" "$LENS_CADDY_TARGET"`, caddyfile)
		if r.code != 0 || r.stdout != loaded+"\n0\n"+orphan+"\n" {
			t.Fatalf("%+v", r)
		}
		if readFile(t, loaded) != content || exists(orphan) || !exists(unrelated) {
			t.Fatal("wrong files kept/removed")
		}
	}
}

func TestCaddyCleanupRemovesOwnedOldHostnameButNotUnmarkedFile(t *testing.T) {
	dir := t.TempDir()
	caddyfile := filepath.Join(dir, "Caddyfile")
	write(t, caddyfile, "import Caddyfile.d/*.caddyfile\n", 0o644)
	unmarked, old := filepath.Join(dir, "conf.d/lens.caddy"), filepath.Join(dir, "conf.d/old.caddy")
	write(t, unmarked, "lens.example {\n}\n", 0o644)
	write(t, old, caddyMarker+"\nother.example {\n}\n", 0o644)
	if r := caddyHelper(t, `lens_caddy_plan "$2"; lens_caddy_remove_stale "$2" "$LENS_CADDY_TARGET"`, caddyfile); r.code != 0 {
		t.Fatal(r.stderr)
	}
	if !exists(unmarked) || exists(old) {
		t.Fatal("cleanup removed the wrong file")
	}
}

func TestCaddyAdaptedCheckRequiresHostMatcherAndLensProxy(t *testing.T) {
	requireTools(t, "jq")
	adapted := filepath.Join(t.TempDir(), "adapted.json")
	payload := func(host, dial string) string {
		raw, _ := json.Marshal(map[string]any{
			"email": "lens.example",
			"apps": map[string]any{"http": map[string]any{"servers": map[string]any{"srv0": map[string]any{"routes": []any{
				map[string]any{
					"match": []any{map[string]any{"host": []string{host}}},
					"handle": []any{map[string]any{"handler": "subroute", "routes": []any{map[string]any{"handle": []any{
						map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": dial}}},
					}}}}},
				},
			}}}}},
		})
		return string(raw)
	}
	script := `lens_caddy_adapted_has_site "$2" lens.example 127.0.0.1:8808`
	for _, tc := range []struct {
		host, dial string
		ok         bool
	}{{"other.example", "127.0.0.1:8808", false}, {"lens.example", "127.0.0.1:8808", true}, {"lens.example", "127.0.0.1:9999", false}} {
		write(t, adapted, payload(tc.host, tc.dial), 0o644)
		if r := caddyHelper(t, script, adapted); (r.code == 0) != tc.ok {
			t.Fatalf("%+v: %+v", tc, r)
		}
	}
}

func TestBootstrapUsesTheTestedCaddyHelpersBeforeOpeningFirewall(t *testing.T) {
	text := read(t, "scripts/bootstrap.sh")
	for _, want := range []string{`lens_caddy_plan "$caddyfile"`, `lens_caddy_remove_stale "$caddyfile"`, `lens_caddy_adapted_has_site "$adapted"`} {
		if !strings.Contains(text, want) {
			t.Errorf("bootstrap lost %q", want)
		}
	}
	if strings.Index(text, "caddy validate --config") > strings.Index(text, "firewall-cmd --add-service") {
		t.Error("Caddy must validate before the firewall opens")
	}
}
