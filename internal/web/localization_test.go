// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Keep authentication cookies while refusing the optional display preference,
// as a browser that blocks preference storage does. No browser script runs in
// these HTTP requests, so localStorage cannot rescue a missing URL language.
type noLanguageCookieJar struct{ http.CookieJar }

func (j noLanguageCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	var allowed []*http.Cookie
	for _, cookie := range cookies {
		if cookie.Name != "elcano_language" {
			allowed = append(allowed, cookie)
		}
	}
	j.CookieJar.SetCookies(u, allowed)
}

func TestAuthNavigationWithoutPreferenceCookies(t *testing.T) {
	for _, tc := range []struct{ query, language, title string }{
		{"en", "en", "Your files"},
		{"es-MX", "es", "Tus archivos"},
		{"pt-BR", "pt", "Os seus ficheiros"},
		{"//evil.example/\"<script>", "en", "Your files"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			h := newHarness(t, false)
			h.client.Jar = noLanguageCookieJar{h.client.Jar}
			for _, route := range []string{"/login", "/auth/login"} {
				response, _ := h.get(route + "?" + url.Values{"lang": {tc.query}, "next": {"https://evil.example/"}}.Encode())
				if response.StatusCode != http.StatusSeeOther || location(response) != "/?lang="+tc.language {
					t.Fatalf("%s redirect: %d %q", route, response.StatusCode, location(response))
				}
				page, body := h.get(location(response))
				if page.StatusCode != http.StatusOK || !strings.Contains(body, `<html lang="`+tc.language+`">`) || !strings.Contains(body, tc.title) {
					t.Fatalf("%s lost language on the final dashboard", route)
				}
			}
			// Submit the action actually rendered in the dashboard, not a URL
			// built by the test. This also covers the native no-script form.
			_, body := h.get("/?lang=" + url.QueryEscape(tc.query))
			form := regexp.MustCompile(`<form method="post" action="([^"]+)" class="[^"]*js-confirm-sign-out`).FindStringSubmatch(body)
			if form == nil || form[1] != "/logout?lang="+tc.language {
				t.Fatalf("logout form did not carry the normalized language: %v", form)
			}
			logout := h.postForm(form[1], nil)
			if logout.StatusCode != http.StatusSeeOther || location(logout) != "https://auth.elcanotek.com/logout?lang="+tc.language {
				t.Fatalf("Auth handoff: %d %q", logout.StatusCode, location(logout))
			}
			u, _ := url.Parse(h.ts.URL)
			for _, cookie := range h.client.Jar.Cookies(u) {
				if cookie.Name == "elcano_language" {
					t.Fatal("test unexpectedly persisted the language preference")
				}
			}
		})
	}
}

func TestLocalizedDashboardPreservesJobsFilesAndPayloads(t *testing.T) {
	h := newHarness(t, false)
	h.writeInput("customer.csv", "Domain\nexample.com\n")
	h.postForm("/jobs", url.Values{"input_file": {"customer.csv"}, "name": {"Customer label"}, "custom_categories": {spec}})
	before, err := os.ReadFile(h.dirs.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ lang, title, queued, field string }{{"en", "Your files", "queued", "Field required"}, {"es", "Tus archivos", "en cola", "Campo obligatorio"}, {"pt", "Os seus ficheiros", "em fila", "Campo obrigatório"}} {
		response, body := h.get("/?lang=" + tc.lang)
		for _, want := range []string{`<html lang="` + tc.lang + `">`, tc.title, tc.queued, "customer.csv", "Customer label", "Educational"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing %q", tc.lang, want)
			}
		}
		if response.Header.Get("Content-Language") != tc.lang {
			t.Fatal(response.Header)
		}
		_, live := h.get("/api/jobs/live?lang=" + tc.lang + "&job_id=" + h.jobs()[0].ID)
		if !strings.Contains(live, `"status":"queued"`) || !strings.Contains(live, `"input_file":"customer.csv"`) {
			t.Fatal(live)
		}
		_, invalid := h.send(http.MethodPost, "/jobs?lang="+tc.lang, strings.NewReader(""), "application/x-www-form-urlencoded")
		if !strings.Contains(invalid, tc.field) || !strings.Contains(invalid, `"type":"missing"`) || !strings.Contains(invalid, `"loc":["body","input_file"]`) {
			t.Fatal(invalid)
		}
		_, signedOut := h.get("/signed-out?lang=" + tc.lang)
		if !strings.Contains(signedOut, `next=%2F&amp;lang=`+tc.lang) {
			t.Fatal("sign-in link lost language")
		}
	}
	after, err := os.ReadFile(h.dirs.StatePath())
	if err != nil || string(before) != string(after) {
		t.Fatal("reading localized UI changed persisted jobs")
	}
}

func TestPreferenceCookieAndLocalizedFileAction(t *testing.T) {
	h := newHarness(t, false)
	h.writeInput("raw.csv", "Domain\nexample.com\n")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, h.ts.URL+"/files/batch-delete?lang=pt", strings.NewReader(url.Values{"destination": {"inputs"}, "filenames": {"raw.csv"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reply, _ := roundTrip(t, h.client, req)
	target, err := url.Parse(reply.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if target.Query().Get("files_message") != "Foi eliminado 1 ficheiro" {
		t.Fatal(target)
	}
	response, body := h.get("/?lang=es-MX")
	if !strings.Contains(body, `<html lang="es">`) || !strings.Contains(response.Header.Get("Set-Cookie"), "elcano_language=es;") {
		t.Fatal(response.Header)
	}
}

func TestLocaleFormattingAndProviderNames(t *testing.T) {
	now := time.Date(2026, 11, 10, 17, 8, 0, 0, time.UTC)
	if got := displayTimestamp(now, "es"); got != "10/11/2026 17:08 UTC" {
		t.Fatal(got)
	}
	if got := displayUploaded("today 17:08", now, "pt"); got != "hoje 17:08" {
		t.Fatal(got)
	}
	if got := displayCost("<$0.001+", "pt"); got != "<0,001 US$+" {
		t.Fatal(got)
	}
	if got := displayModelLabel("Customer Model · $0.10/M in · $0.20/M out · search from $5/1K requests", "es"); got != "Customer Model · 0,10 US$/M de entrada · 0,20 US$/M de salida · búsqueda desde 5 US$/1000 solicitudes" {
		t.Fatal(got)
	}
	if got := displayModelLabel("Customer-owned label", "pt"); got != "Customer-owned label" {
		t.Fatal(got)
	}
}

func TestHTTPDeploymentKeepsLanguageAcrossFormActions(t *testing.T) {
	// This harness is an explicitly supported plain-HTTP deployment. Browser
	// forms omit lang, so the server must receive the saved display preference.
	h := newHarness(t, false)
	h.writeInput("customer.csv", "Domain\nexample.com\n")
	response, _ := h.get("/?lang=es")
	if strings.Contains(response.Header.Get("Set-Cookie"), "Secure") {
		t.Fatal("HTTP preference cookie would be discarded")
	}
	result := h.postForm("/files/batch-delete", url.Values{"destination": {"inputs"}, "filenames": {"customer.csv"}})
	target, err := url.Parse(result.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if target.Query().Get("files_message") != "Se ha eliminado 1 archivo" {
		t.Fatal("form action lost locale", target)
	}
	_, body := h.get(target.String())
	if !strings.Contains(body, `<html lang="es">`) || !strings.Contains(body, "Se ha eliminado 1 archivo") {
		t.Fatal("post-action page lost locale")
	}
}
