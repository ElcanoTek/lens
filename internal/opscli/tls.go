// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package opscli

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// caddySites are where bootstrap may have written the Lens site block
// (scripts/lib/caddy-site.sh picks a directory the Caddyfile already
// imports: conf.d/*.caddy, or stock Fedora's Caddyfile.d/*.caddyfile). The
// bash CLI only looked at conf.d, so on stock Fedora `lens tls status`
// reported "No Caddy site block" for a working site.
var caddySites = []string{"/etc/caddy/conf.d/lens.caddy", "/etc/caddy/Caddyfile.d/lens.caddyfile"}

func (c *CLI) tls(args []string) int {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	if _, err := c.LookPath("caddy"); err != nil {
		fmt.Fprintln(c.Stderr, "Caddy not installed — re-run bootstrap with a hostname to set up TLS")
		return 1
	}
	switch sub {
	case "status":
		return c.tlsStatus()
	case "reload":
		return c.execAsRoot("caddy", "reload", "--config", "/etc/caddy/Caddyfile")
	case "restart":
		return c.execAsRoot("systemctl", "restart", "caddy.service")
	default:
		fmt.Fprint(c.Stdout, `lens tls status     show hostname + cert expiry + Caddy state
lens tls reload     reload /etc/caddy/Caddyfile
lens tls restart    restart Caddy
`)
		return 0
	}
}

func (c *CLI) tlsStatus() int {
	site := ""
	for _, p := range caddySites {
		if fi, err := os.Stat(c.hostPath(p)); err == nil && fi.Mode().IsRegular() {
			site = c.hostPath(p)
			break
		}
	}
	if site == "" {
		fmt.Fprintf(c.Stdout, "No Caddy site block at %s\n", strings.Join(caddySites, " or "))
	} else {
		host := siteHost(site)
		fmt.Fprintf(c.Stdout, "Host: %s\n", firstNonEmpty(host, "<unconfigured>"))
		if host != "" {
			if err := describeCert(context.Background(), c, host); err != nil {
				fmt.Fprintf(c.Stdout, "(could not reach %s:443 — check DNS + firewall)\n", host)
			}
		}
	}
	// Informational: an inactive Caddy prints "inactive" and that is the
	// answer, not an error of this command.
	c.Run(context.Background(), []string{"systemctl", "is-active", "caddy.service"})
	return 0
}

// siteHost returns the site address of the first top-level block in a Caddy
// site file: the first line that starts with neither whitespace, a brace nor
// a comment and opens a block ("lens.example.com {").
func siteHost(path string) string {
	f, err := os.Open(path) //nolint:gosec // fixed Caddy site paths
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.ContainsRune(" \t{}#", rune(line[0])) || !strings.Contains(line, "{") {
			continue
		}
		return strings.TrimSpace(strings.Replace(line, " {", "", 1))
	}
	return ""
}

// describeCert prints the served certificate's subject, issuer and validity
// window (what `openssl s_client | openssl x509 -subject -issuer -dates`
// printed), plus whether it verifies for host.
func describeCert(ctx context.Context, c *CLI, host string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config:    &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
	}
	// A verifying handshake. When verification fails, the error still
	// carries the served chain, so an expired, self-signed or wrong-name
	// certificate is shown (like openssl s_client did) and its failure is
	// reported instead of aborting.
	var (
		certs     []*x509.Certificate
		verifyErr error
	)
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
	var cve *tls.CertificateVerificationError
	switch {
	case err == nil:
		defer conn.Close()
		tconn, ok := conn.(*tls.Conn)
		if !ok {
			return fmt.Errorf("not a TLS connection")
		}
		certs = tconn.ConnectionState().PeerCertificates
	case errors.As(err, &cve):
		certs, verifyErr = cve.UnverifiedCertificates, cve.Err
	default:
		return err
	}
	if len(certs) == 0 {
		return fmt.Errorf("no certificate served")
	}
	leaf := certs[0]
	const layout = "Jan _2 15:04:05 2006 GMT"
	fmt.Fprintf(c.Stdout, "subject=%s\nissuer=%s\nnotBefore=%s\nnotAfter=%s\n",
		leaf.Subject, leaf.Issuer, leaf.NotBefore.UTC().Format(layout), leaf.NotAfter.UTC().Format(layout))
	if verifyErr != nil {
		fmt.Fprintf(c.Stdout, "verify: FAILED (%v)\n", verifyErr)
	} else {
		fmt.Fprintf(c.Stdout, "verify: ok (expires in %d days)\n", int(time.Until(leaf.NotAfter).Hours()/24))
	}
	return nil
}
