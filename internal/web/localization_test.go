// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

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
		if !strings.Contains(signedOut, `lang=`+tc.lang+`&amp;next=%2F`) {
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
