// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package deploytest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDig answers like dig +noall +comments +answer, from FAKE_<TYPE>
// (addresses), FAKE_<TYPE>_STATUS, FAKE_<TYPE>_RC, FAKE_TC and FAKE_CNAME.
// It refuses a query that could reach the box's own resolver or ~/.digrc.
const fakeDig = `r=0 tcp=0 at=0 type=
for a in "$@"; do
  case "$a" in -r) r=1 ;; +tcp) tcp=1 ;; @1.1.1.1) at=1 ;; A|AAAA) type=$a ;; esac
done
if [ $r != 1 ] || [ $tcp != 1 ] || [ $at != 1 ]; then echo "stub dig: not a public TCP query: $*" >&2; exit 64; fi
eval "rc=\${FAKE_${type}_RC:-0} status=\${FAKE_${type}_STATUS:-NOERROR} recs=\${FAKE_${type}:-}"
if [ "$rc" != 0 ]; then echo ';; communications error to 1.1.1.1#53: timed out'; exit "$rc"; fi
flags="qr rd ra"; [ "${FAKE_TC:-0}" = 1 ] && flags="qr tc rd ra"
echo ';; Got answer:'
echo ";; ->>HEADER<<- opcode: QUERY, status: $status, id: 4242"
echo ";; flags: $flags; QUERY: 1, ANSWER: 1, AUTHORITY: 0, ADDITIONAL: 1"
echo
echo ';; OPT PSEUDOSECTION:'
echo '; EDNS: version: 0, flags:; udp: 1232'
echo ';; ANSWER SECTION:'
name=lens.example.com.
if [ -n "${FAKE_CNAME:-}" ]; then printf '%s\t300\tIN\tCNAME\t%s\n' "$name" "$FAKE_CNAME"; name=$FAKE_CNAME; fi
for ip in $recs; do printf '%s\t300\tIN\t%s\t%s\n' "$name" "$type" "$ip"; done`

const fakeCurl = `case "$*" in
  *api6.ipify.org*) [ -n "${FAKE_IP6:-}" ] || exit 7; printf '%s' "$FAKE_IP6"; exit "${FAKE_IP6_RC:-0}" ;;
  *api.ipify.org*) [ -n "${FAKE_IP4:-}" ] || exit 7; printf '%s' "$FAKE_IP4"; exit "${FAKE_IP4_RC:-0}" ;;
  *) exit 2 ;;
esac`

// dnsPrecheck runs lens_dns_precheck lens.example.com with stub dig and curl.
func dnsPrecheck(t *testing.T, withDig bool, kv ...string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	linkTools(t, bin, "sed", "head", "grep", "awk", "sort", "tr")
	if withDig {
		stub(t, bin, "dig", fakeDig)
	}
	stub(t, bin, "curl", fakeCurl)
	r := runCmd(t, []string{bashPath(t), "-c", `set -euo pipefail; . "$1"; ok() { echo "OK $*"; }; warn() { echo "WARN $*"; }; ` +
		`if lens_dns_precheck lens.example.com; then echo RC=0; else echo RC=1; fi`, "test", filepath.Join(repo(t), "scripts/lib/dnscheck.sh")},
		env(append([]string{"PATH=" + bin}, kv...)...))
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	return r.stdout + r.stderr
}

func TestDNSPrecheckAsksPublicDNSAndComparesEveryRecord(t *testing.T) {
	box := []string{"FAKE_IP4=203.0.113.7", "FAKE_IP6=2001:DB8::7"}
	for _, tc := range []struct {
		name string
		kv   []string
		want []string
		not  string
	}{
		{"one A record on this box", []string{"FAKE_A=203.0.113.7"},
			[]string{"OK DNS resolves correctly (203.0.113.7)", "RC=0"}, "WARN"},
		{"A and AAAA on this box", []string{"FAKE_A=203.0.113.7", "FAKE_AAAA=2001:db8::7"},
			[]string{"OK DNS resolves correctly (203.0.113.7, 2001:db8::7)", "RC=0"}, "WARN"},
		{"IPv6 only", []string{"FAKE_AAAA=2001:db8::7"},
			[]string{"OK DNS resolves correctly (2001:db8::7)", "RC=0"}, "WARN"},
		{"through a CNAME", []string{"FAKE_CNAME=edge.example.net.", "FAKE_A=203.0.113.7"},
			[]string{"OK DNS resolves correctly (203.0.113.7)", "RC=0"}, "WARN"},
		{"a second A record elsewhere", []string{"FAKE_A=203.0.113.7 198.51.100.9"},
			[]string{"WARN DNS mismatch: lens.example.com → 198.51.100.9, 203.0.113.7, this box → 203.0.113.7", "RC=1"}, "OK"},
		{"an AAAA record elsewhere", []string{"FAKE_A=203.0.113.7", "FAKE_AAAA=2001:db8::7 2001:db8::99"},
			[]string{"WARN DNS mismatch: lens.example.com AAAA → 2001:db8::7, 2001:db8::99, this box → 2001:db8::7", "RC=1"}, "OK"},
		{"no records yet", nil,
			[]string{"WARN lens.example.com doesn't resolve in public DNS yet", "Add an A record: lens.example.com → 203.0.113.7", "RC=1"}, "OK"},
		{"NXDOMAIN", []string{"FAKE_A_STATUS=NXDOMAIN", "FAKE_AAAA_STATUS=NXDOMAIN"},
			[]string{"doesn't resolve in public DNS yet", "RC=1"}, "OK"},
		{"SERVFAIL is not 'no record'", []string{"FAKE_A_STATUS=SERVFAIL"},
			[]string{"WARN could not check public DNS for lens.example.com", "RC=1"}, "doesn't resolve"},
		{"a failed AAAA lookup", []string{"FAKE_A=203.0.113.7", "FAKE_AAAA_STATUS=SERVFAIL"},
			[]string{"the AAAA lookup at 1.1.1.1 failed", "RC=1"}, "OK"},
		{"a timeout", []string{"FAKE_A_RC=9"},
			[]string{"WARN could not check public DNS", "RC=1"}, "doesn't resolve"},
		{"a truncated reply", []string{"FAKE_A=203.0.113.7", "FAKE_TC=1"},
			[]string{"WARN could not check public DNS", "RC=1"}, "OK"},
		{"another spelling of this box's IPv6", []string{"FAKE_A=203.0.113.7", "FAKE_AAAA=2001:db8::7",
			"FAKE_IP6=2001:0DB8:0000:0000:0000:0000:0000:0007"},
			[]string{"OK DNS resolves correctly (203.0.113.7, 2001:db8::7)", "RC=0"}, "WARN"},
		{"a box address cut short by a failed transfer", []string{"FAKE_A=203.0.113.7", "FAKE_IP4_RC=18"},
			[]string{"WARN could not learn this box's public IPv4", "RC=1"}, "OK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := dnsPrecheck(t, true, append(append([]string{}, box...), tc.kv...)...)
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("want %q in:\n%s", w, out)
				}
			}
			if strings.Contains(out, tc.not) {
				t.Errorf("did not want %q in:\n%s", tc.not, out)
			}
		})
	}
}

func TestDNSPrecheckWithoutTheBoxAddress(t *testing.T) {
	out := dnsPrecheck(t, true, "FAKE_A=203.0.113.7", "FAKE_AAAA=2001:db8::7")
	for _, w := range []string{
		"WARN could not learn this box's public IPv4; check that lens.example.com → 203.0.113.7 is this box.",
		"WARN lens.example.com has AAAA 2001:db8::7 but this box has no public IPv6",
		"RC=1",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("want %q in:\n%s", w, out)
		}
	}
}

func TestDNSPrecheckWithoutDigSaysItDidNotCheck(t *testing.T) {
	out := dnsPrecheck(t, false, "FAKE_IP4=203.0.113.7")
	if !strings.Contains(out, "WARN dig is not installed, so public DNS for lens.example.com was not checked") ||
		!strings.Contains(out, "RC=1") || strings.Contains(out, "doesn't resolve") {
		t.Fatal(out)
	}
}

func TestBootstrapChecksPublicDNSBeforeCaddy(t *testing.T) {
	src := read(t, "scripts/bootstrap.sh")
	step := between(t, src, `step "7/7`, "dnf install -y caddy")
	if !strings.Contains(step, `lens_dns_precheck "$HOSTNAME_FOR_TLS" || true`) || strings.Contains(step, "dig +short") {
		t.Fatalf("step 7 must use lens_dns_precheck, not the box's resolver:\n%s", step)
	}
	if !strings.Contains(between(t, src, "PKGS=(", ")"), "bind-utils") {
		t.Fatal("bootstrap must install bind-utils (dig) for the DNS check")
	}
}

func TestIPv6SpellingsCompareEqual(t *testing.T) {
	r := runCmd(t, []string{bashPath(t), "-c", `. "$1"; shift; for a in "$@"; do printf '%s=%s\n' "$a" "$(lens_ip6_full "$a")"; done`, "test",
		filepath.Join(repo(t), "scripts/lib/dnscheck.sh"),
		"2001:db8::7", "2001:DB8:0:0:0:0:0:7", "2001:db8:0:0:1::1", "::", "::1", "fe80::", "1:2:3:4:5:6:7:8",
		"203.0.113.7", "::ffff:192.0.2.1", "1:2:3:4:5:6:7:8:9", "2001:db8::g",
		"2001:db8::7:", "1:2:3:4:5:6:7:8:", ":1:2:3:4:5:6:7:8", "1:2:3:4::5:6:7:8", "1::2::3"}, env())
	want := "2001:db8::7=2001:0db8:0000:0000:0000:0000:0000:0007\n" +
		"2001:DB8:0:0:0:0:0:7=2001:0db8:0000:0000:0000:0000:0000:0007\n" +
		"2001:db8:0:0:1::1=2001:0db8:0000:0000:0001:0000:0000:0001\n" +
		"::=0000:0000:0000:0000:0000:0000:0000:0000\n" +
		"::1=0000:0000:0000:0000:0000:0000:0000:0001\n" +
		"fe80::=fe80:0000:0000:0000:0000:0000:0000:0000\n" +
		"1:2:3:4:5:6:7:8=0001:0002:0003:0004:0005:0006:0007:0008\n" +
		// Not plain IPv6: compared as given.
		"203.0.113.7=203.0.113.7\n::ffff:192.0.2.1=::ffff:192.0.2.1\n1:2:3:4:5:6:7:8:9=1:2:3:4:5:6:7:8:9\n2001:db8::g=2001:db8::g\n" +
		// Malformed: never normalized into a valid address.
		"2001:db8::7:=2001:db8::7:\n1:2:3:4:5:6:7:8:=1:2:3:4:5:6:7:8:\n:1:2:3:4:5:6:7:8=:1:2:3:4:5:6:7:8\n" +
		"1:2:3:4::5:6:7:8=1:2:3:4::5:6:7:8\n1::2::3=1::2::3\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("%+v\nwant:\n%s", r, want)
	}
}
