// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package i18n

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestRequestPreferenceAndSafeCookie(t *testing.T) {
	for _, tc := range []struct{ query, cookie, browser, want string }{
		{"lang=es-MX&ui_locales=pt", "pt", "en", "es"},
		{"ui_locales=fr+pt-BR+es", "es", "en", "pt"},
		{"lang=unsupported", "pt-PT", "es", "pt"},
		{"", "", "en;q=0.2,es-AR;q=0.9,pt;q=0.5", "es"},
		{"", "", "pt;q=0,es;q=invalid,fr", "en"},
	} {
		r := httptest.NewRequest(http.MethodGet, "https://lens.example/?"+tc.query, nil)
		r.Header.Set("Accept-Language", tc.browser)
		if tc.cookie != "" {
			r.AddCookie(&http.Cookie{Name: "elcano_language", Value: tc.cookie})
		}
		if got := RequestLanguage(r); got != tc.want {
			t.Errorf("%+v: %q", tc, got)
		}
	}
	recorder := httptest.NewRecorder()
	Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if Language(w) != "pt" {
			t.Fatal(Language(w))
		}
	})).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "https://lens.example/?lang=pt-BR", nil))
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal(cookies)
	}
	c := cookies[0]
	if c.Name != "elcano_language" || c.Value != "pt" || c.Domain != "" || c.Path != "/" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie %+v", c)
	}
}

func TestDisplayLanguageLeavesAuthenticationRequestUnchanged(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://lens.example/auth/callback?state=opaque%2bstate&code=one-time", nil)
	r.Header.Set("Accept-Language", "en")
	w := httptest.NewRecorder()
	displayWriter, displayRequest := WithDisplayLanguage(w, r, "pt-BR")
	if Language(displayWriter) != "pt" || RequestLanguage(displayRequest) != "pt" {
		t.Fatal("presentation layers disagree")
	}
	if RequestLanguage(r) != "en" || Language(w) != "en" || r.URL.RawQuery != "state=opaque%2bstate&code=one-time" || displayRequest.URL != r.URL {
		t.Fatal("display override changed authentication request or original writer")
	}
	if ignoredWriter, ignoredRequest := WithDisplayLanguage(w, r, "//evil.example"); ignoredWriter != w || ignoredRequest != r {
		t.Fatal("unsupported display language accepted")
	}
}

func TestLanguageURLPreservesOpaqueQueryFields(t *testing.T) {
	for _, language := range []string{"es", "pt"} {
		source := "/jobs/a%2Fb?q=alpha;beta&opaque=100%done&%zz=opaque&filter=a%26b&filter=c&empty=&flag&&l%61ng=en&%6cang=pt#saved"
		want := "/jobs/a%2Fb?q=alpha;beta&opaque=100%done&%zz=opaque&filter=a%26b&filter=c&empty=&flag&&lang=" + language + "#saved"
		if got := URL(source, language+"-XX", "lang"); got != want {
			t.Fatalf("unrelated URL data changed: %q, want %q", got, want)
		}
		if got := URL(source, "unsupported", "lang"); got != source {
			t.Fatal("unsupported locale changed URL")
		}
	}
	if got := URL("https://auth.example/authorize?state=A%2bB%2fC%3d&ui%5flocales=en", "pt", "ui_locales"); got != "https://auth.example/authorize?state=A%2bB%2fC%3d&ui_locales=pt" {
		t.Fatal("OAuth state encoding changed", got)
	}
}

func TestCatalogsKeepMessageSlotsAndOpaqueValues(t *testing.T) {
	slots := regexp.MustCompile(`\{[0-9]+\}|%(?:\.[0-9]+)?[sdqvf]`)
	for language, catalog := range languageCatalogs {
		if language == "en" {
			continue
		}
		if len(catalog) < 250 {
			t.Fatalf("%s catalog incomplete: %d", language, len(catalog))
		}
		for source, target := range catalog {
			if strings.TrimSpace(target) == "" {
				t.Errorf("%s empty %q", language, source)
			}
			a, b := slots.FindAllString(source, -1), slots.FindAllString(target, -1)
			sort.Strings(a)
			sort.Strings(b)
			if !reflect.DeepEqual(a, b) {
				t.Errorf("%s slots %q -> %q", language, source, target)
			}
			if _, ok := languageCatalogs["es"][source]; !ok {
				t.Errorf("es missing %q", source)
			}
			if _, ok := languageCatalogs["pt"][source]; !ok {
				t.Errorf("pt missing %q", source)
			}
		}
	}
	if got := T("es", "Deleted {0}", "{1}<customer>.csv"); got != "Se ha eliminado {1}<customer>.csv" {
		t.Fatal(got)
	}
	if got := Message("pt", "Invalid model id: customer/model"); got != "ID de modelo inválido: customer/model" {
		t.Fatal(got)
	}
	if got := Message("es", "customer text should stay unchanged"); got != "customer text should stay unchanged" {
		t.Fatal(got)
	}
	for _, tc := range []struct{ lang, want string }{{"en", "12,345"}, {"es", "12.345"}, {"pt", "12\u00a0345"}} {
		if got := Number(12345, tc.lang); got != tc.want {
			t.Errorf("%s: %q", tc.lang, got)
		}
		if tc.lang != "en" && Number(1234, tc.lang) != "1234" {
			t.Fatal("four-digit counts must match Intl grouping")
		}
		if got := Number("12345", tc.lang); got != "12345" {
			t.Fatalf("identifier changed: %q", got)
		}
	}
}
