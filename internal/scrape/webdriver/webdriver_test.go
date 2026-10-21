// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package webdriver_test

import (
	"context"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/lens/internal/scrape/webdriver"
	"github.com/ElcanoTek/lens/internal/scrape/webdriver/wdtest"
)

func caps() map[string]any {
	return webdriver.ChromeCapabilities(webdriver.ChromeOptions{
		Args:             []string{"--headless=new"},
		ExcludeSwitches:  []string{"enable-automation"},
		PageLoadStrategy: "eager",
		PageLoadTimeout:  12 * time.Second,
	})
}

func TestSessionLifecycle(t *testing.T) {
	fake := wdtest.New(t)
	ctx := context.Background()
	driver, err := webdriver.NewSession(ctx, fake.Endpoint+"/wd/hub", caps())
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.SetPageLoadTimeout(ctx, 7500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := driver.Navigate(ctx, "https://example.com/"); err != nil {
		t.Fatal(err)
	}
	if u, err := driver.CurrentURL(ctx); err != nil || u != "https://example.com/" {
		t.Fatalf("current url %q %v", u, err)
	}
	if src, err := driver.PageSource(ctx); err != nil || src != wdtest.Page {
		t.Fatalf("page source %q %v", src, err)
	}
	if err := driver.Quit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := driver.Quit(ctx); err != nil { // idempotent: no second DELETE
		t.Fatal(err)
	}

	var methods []string
	for _, r := range fake.Requests() {
		methods = append(methods, r.Method+" "+r.Path)
	}
	want := []string{
		"POST /wd/hub/session",
		"POST /wd/hub/session/s1/timeouts",
		"POST /wd/hub/session/s1/url",
		"GET /wd/hub/session/s1/url",
		"GET /wd/hub/session/s1/source",
		"DELETE /wd/hub/session/s1",
	}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("requests %v", methods)
	}
	if got := fake.Requests()[1].Body; !reflect.DeepEqual(got, map[string]any{"pageLoad": float64(7500)}) {
		t.Errorf("timeouts body %v", got)
	}
	if fake.SessionCount() != 0 {
		t.Error("session left open")
	}
	if err := driver.Navigate(ctx, "https://example.com/"); !webdriver.HasCode(err, webdriver.CodeInvalidSessionID) {
		t.Errorf("navigate after quit: %v", err)
	}
}

func TestNewSessionPayload(t *testing.T) {
	fake := wdtest.New(t)
	ctx := context.Background()
	driver, err := webdriver.NewSession(ctx, fake.Endpoint+"/wd/hub", caps())
	if err != nil {
		t.Fatal(err)
	}
	_ = driver.Quit(ctx)
	want := map[string]any{
		"capabilities": map[string]any{
			"alwaysMatch": map[string]any{
				"browserName":      "chrome",
				"pageLoadStrategy": "eager",
				"timeouts":         map[string]any{"pageLoad": float64(12000)},
				"goog:chromeOptions": map[string]any{
					"args":                   []any{"--headless=new"},
					"excludeSwitches":        []any{"enable-automation"},
					"useAutomationExtension": false,
				},
			},
			"firstMatch": []any{map[string]any{}},
		},
	}
	if got := fake.Requests()[0].Body; !reflect.DeepEqual(got, want) {
		t.Errorf("payload %#v", got)
	}
}

func TestW3CErrorsKeepTheirCode(t *testing.T) {
	for _, code := range []string{"timeout", "invalid session id", "unknown error", "chrome not reachable"} {
		fake := wdtest.New(t)
		fake.Fail("POST", "/url", http.StatusInternalServerError, map[string]any{"value": map[string]any{"error": code, "message": "boom"}})
		ctx := context.Background()
		driver, err := webdriver.NewSession(ctx, fake.Endpoint+"/wd/hub", caps())
		if err != nil {
			t.Fatal(err)
		}
		err = driver.Navigate(ctx, "https://slow.example/")
		if !webdriver.HasCode(err, code) {
			t.Errorf("%s: got %v", code, err)
		}
		// The code leads the message, so the scraper's restart signatures match.
		if err == nil || err.Error() != code+": boom" {
			t.Errorf("%s: message %v", code, err)
		}
		_ = driver.Quit(ctx)
	}
}

func TestSessionNotCreated(t *testing.T) {
	fake := wdtest.New(t)
	fake.Fail("POST", "", http.StatusInternalServerError,
		map[string]any{"value": map[string]any{"error": "session not created", "message": "Chrome failed to start"}})
	_, err := webdriver.NewSession(context.Background(), fake.Endpoint+"/wd/hub", caps())
	if !webdriver.HasCode(err, webdriver.CodeSessionNotCreated) || !strings.Contains(err.Error(), "Chrome failed to start") {
		t.Errorf("got %v", err)
	}
}

func TestNonJSONGatewayErrorIsAWebDriverError(t *testing.T) {
	fake := wdtest.New(t)
	fake.Fail("GET", "/source", http.StatusBadGateway, []byte("<html>Bad Gateway</html>"))
	ctx := context.Background()
	driver, err := webdriver.NewSession(ctx, fake.Endpoint+"/wd/hub", caps())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.PageSource(ctx); !webdriver.HasCode(err, webdriver.CodeUnknown) || !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("got %v", err)
	}
	_ = driver.Quit(ctx)
}

func TestUnreachableEndpointIsAWebDriverError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	_, err = webdriver.NewSession(context.Background(), "http://"+addr+"/wd/hub", caps(),
		webdriver.WithCommandTimeout(2*time.Second))
	if !webdriver.HasCode(err, webdriver.CodeUnknown) || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("got %v", err)
	}
}

func TestProxyEnvironmentIsIgnored(t *testing.T) {
	// A proxy pointed at a closed port would fail every command if honoured.
	t.Setenv("http_proxy", "http://127.0.0.1:9")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("no_proxy", "")
	t.Setenv("NO_PROXY", "")
	fake := wdtest.New(t)
	ctx := context.Background()
	driver, err := webdriver.NewSession(ctx, fake.Endpoint+"/wd/hub", caps())
	if err != nil {
		t.Fatal(err)
	}
	if src, err := driver.PageSource(ctx); err != nil || src != wdtest.Page {
		t.Errorf("page source %q %v", src, err)
	}
	_ = driver.Quit(ctx)
}

func TestCancelledContextStopsACommand(t *testing.T) {
	fake := wdtest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	driver, err := webdriver.NewSession(ctx, fake.Endpoint+"/wd/hub", caps())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := driver.Navigate(ctx, "https://example.com/"); err == nil {
		t.Error("navigation ran on a cancelled context")
	}
	_ = driver.Quit(context.Background())
}
