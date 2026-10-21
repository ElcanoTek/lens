// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package stores

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/logx"
)

func TestMain(m *testing.M) {
	logx.SetOutput(io.Discard, slog.LevelError)
	os.Exit(m.Run())
}

type sleeps struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.delays = append(s.delays, d)
	s.mu.Unlock()
	return ctx.Err()
}

const lookupJSON = `{"resultCount": 1, "results": [{
 "trackName": "Harbor Tides", "artistName": "Saltmarsh", "bundleId": "com.saltmarsh.tides",
 "primaryGenreName": "Weather", "genres": ["Weather", "Travel"],
 "averageUserRating": 4.56789, "userRatingCount": 12487,
 "description": "Know the tide.", "price": 1.99, "currency": "EUR",
 "contentAdvisoryRating": "4+", "version": "2.1", "releaseDate": "2020-01-01T00:00:00Z",
 "currentVersionReleaseDate": "2026-03-03T00:00:00Z", "trackViewUrl": "https://apps.apple.com/app/id1",
 "artworkUrl100": "https://img/100.png", "fileSizeBytes": "52428800", "supportedDevices": ["iPhone15"]}]}`

func iosServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func iosClient(srv *httptest.Server, s *sleeps) *IOSClient {
	o := DefaultIOSOptions()
	o.BaseURL = srv.URL + "/lookup"
	o.RequestDelay = 0
	o.Sleep = s.sleep
	return NewIOSClient(o)
}

func TestIOSLookupParsesTextJavascript(t *testing.T) {
	srv := iosServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lookup" || r.URL.Query().Get("id") != "389801252" {
			http.NotFound(w, r)
			return
		}
		// The iTunes API's real content type.
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = io.WriteString(w, lookupJSON)
	})
	c := iosClient(srv, &sleeps{})
	app := c.FetchAppMetadata(context.Background(), "389801252")
	if !app.Success || app.AppName != "Harbor Tides" || app.Developer != "Saltmarsh" || app.BundleID != "com.saltmarsh.tides" {
		t.Fatalf("app %+v", app)
	}
	if app.Rating != 4.57 || app.RatingCount != 12487 || app.Price != 1.99 || app.Currency != "EUR" ||
		app.FileSizeMB != 50 || app.IconURL != "https://img/100.png" || app.Platform != "ios" ||
		app.LastUpdated != "2026-03-03T00:00:00Z" || !slices.Equal(app.Genres, []string{"Weather", "Travel"}) {
		t.Errorf("app %+v", app)
	}
	want := "App Name: Harbor Tides\nDeveloper: Saltmarsh\nPrimary Category: Weather\n" +
		"All Categories: Weather, Travel\nRating: 4.6/5.0 (12,487 reviews)\nContent Rating: 4+\n\nDescription:\nKnow the tide."
	if app.ContentForLLM != want {
		t.Errorf("content:\n%s", app.ContentForLLM)
	}
	if st := c.Stats(); st.RequestCount != 1 || st.SuccessCount != 1 || st.SuccessRate != 100 {
		t.Errorf("stats %+v", st)
	}
}

func TestIOSNotFoundIsRetriedWithBackoff(t *testing.T) {
	var calls atomic.Int32
	srv := iosServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"resultCount": 0, "results": []}`)
	})
	s := &sleeps{}
	c := iosClient(srv, s)
	app := c.FetchAppMetadata(context.Background(), "1")
	if app.Success || app.Error != "App not found in App Store" || app.AppID != "1" || app.FetchedAt == "" {
		t.Fatalf("app %+v", app)
	}
	if calls.Load() != 3 || fmt.Sprint(s.delays) != "[2s 4s]" {
		t.Errorf("%d calls, delays %v", calls.Load(), s.delays)
	}
	if st := c.Stats(); st.RequestCount != 3 || st.ErrorCount != 1 || st.SuccessCount != 0 {
		t.Errorf("stats %+v", st)
	}
}

func TestIOSHTTPErrorAndBadJSON(t *testing.T) {
	status := http.StatusServiceUnavailable
	srv := iosServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "not json")
	})
	c := iosClient(srv, &sleeps{})
	if app := c.FetchAppMetadata(context.Background(), "1"); app.Error != "HTTP 503" {
		t.Errorf("app %+v", app)
	}
	status = http.StatusOK
	if app := c.FetchAppMetadata(context.Background(), "1"); !strings.HasPrefix(app.Error, "Unexpected error: ") {
		t.Errorf("app %+v", app)
	}
}

func TestIOSNetworkError(t *testing.T) {
	o := DefaultIOSOptions()
	o.MaxRetries = 1
	o.HTTPClient = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("refused") })}
	app := NewIOSClient(o).FetchAppMetadata(context.Background(), "1")
	if !strings.HasPrefix(app.Error, "Network error: ") || !strings.Contains(app.Error, "refused") {
		t.Errorf("app %+v", app)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRateLimitSpacesRequests(t *testing.T) {
	srv := iosServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, lookupJSON) })
	s := &sleeps{}
	o := DefaultIOSOptions()
	o.BaseURL = srv.URL
	o.RequestDelay = time.Hour // every request after the first must wait
	o.Sleep = s.sleep
	c := NewIOSClient(o)
	for range 3 {
		if app := c.FetchAppMetadata(context.Background(), "1"); !app.Success {
			t.Fatalf("app %+v", app)
		}
	}
	if len(s.delays) != 2 || s.delays[0] < 59*time.Minute {
		t.Errorf("rate-limit waits %v", s.delays)
	}
}

func TestRealRateLimitDelay(t *testing.T) {
	srv := iosServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, lookupJSON) })
	o := DefaultIOSOptions()
	o.BaseURL = srv.URL
	o.RequestDelay = 40 * time.Millisecond
	c := NewIOSClient(o)
	start := time.Now()
	c.FetchBatch(context.Background(), []string{"1", "2", "3"})
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Errorf("three requests took only %v", elapsed)
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	var inFlight, peak atomic.Int32
	srv := iosServer(t, func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		_, _ = io.WriteString(w, lookupJSON)
	})
	o := DefaultIOSOptions()
	o.BaseURL = srv.URL
	o.RequestDelay = 0
	o.MaxConcurrent = 2
	results := NewIOSClient(o).FetchBatch(context.Background(), []string{"1", "2", "3", "4", "5", "6"})
	for i, r := range results {
		if !r.Success || r.AppID != fmt.Sprint(i+1) {
			t.Errorf("result %d: %+v", i, r)
		}
	}
	if peak.Load() > 2 {
		t.Errorf("peak concurrency %d", peak.Load())
	}
}

func TestCancellationStopsRetries(t *testing.T) {
	srv := iosServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	o := DefaultIOSOptions()
	o.BaseURL = srv.URL
	o.RequestDelay = 0
	o.RetryDelay = time.Hour // real sleep: must be cut short by the context
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	app := NewIOSClient(o).FetchAppMetadata(ctx, "1")
	if app.Success || time.Since(start) > 5*time.Second {
		t.Errorf("app %+v after %v", app, time.Since(start))
	}
}

func TestOptionsFromConfig(t *testing.T) {
	cfg := config.Default()
	ios := IOSOptionsFromConfig(cfg)
	if ios.RequestDelay != 5*time.Second || ios.MaxConcurrent != 1 || ios.RetryDelay != 3*time.Second || ios.RequestTimeout != 30*time.Second {
		t.Errorf("ios %+v", ios)
	}
	android := AndroidOptionsFromConfig(cfg)
	if android.RequestDelay != 10*time.Second || android.MaxConcurrent != 1 || android.RetryDelay != 5*time.Second || android.UserAgent != DefaultAndroidUserAgent {
		t.Errorf("android %+v", android)
	}
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../htmldom/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func androidClient(srv *httptest.Server, s *sleeps) *AndroidClient {
	o := DefaultAndroidOptions()
	o.BaseURL = srv.URL + "/store/apps/details"
	o.RequestDelay = 0
	o.Sleep = s.sleep
	return NewAndroidClient(o)
}

func TestAndroidScrapesThePlayStorePage(t *testing.T) {
	page := readFixture(t, "play_full.html")
	srv := iosServer(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/store/apps/details" || q.Get("id") != "com.saltmarsh.tides" || q.Get("hl") != "en" || q.Get("gl") != "US" ||
			r.Header.Get("User-Agent") != DefaultAndroidUserAgent || r.Header.Get("Accept-Language") != "en-US,en;q=0.5" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, page)
	})
	app := androidClient(srv, &sleeps{}).FetchAppMetadata(context.Background(), "com.saltmarsh.tides")
	if !app.Success || app.AppName != "Harbor Tide Tables" || app.Developer != "Saltmarsh Software Ltd." ||
		app.Rating != 4.6 || app.RatingCount != 12487 || app.Downloads != "10,000+" || app.Platform != "android" {
		t.Fatalf("app %+v", app)
	}
	if app.PlayStoreURL != "https://play.google.com/store/apps/details?id=com.saltmarsh.tides" {
		t.Errorf("link %s", app.PlayStoreURL)
	}
}

func TestAndroidFailures(t *testing.T) {
	blocked := readFixture(t, "play_blocked.html")
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusNotFound, "", "App not found on Play Store"},
		{http.StatusTooManyRequests, "", "HTTP 429"},
		{http.StatusOK, blocked, "Blocked by Google Play Store"},
		{http.StatusOK, "<html><body><p>nothing here</p></body></html>",
			"Could not extract app name - page might be blocked or app not found"},
	}
	for _, c := range cases {
		var calls atomic.Int32
		srv := iosServer(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		})
		s := &sleeps{}
		app := androidClient(srv, s).FetchAppMetadata(context.Background(), "com.example")
		if app.Success || app.Error != c.want || app.PackageName != "com.example" {
			t.Errorf("%d: app %+v", c.status, app)
		}
		if calls.Load() != 3 || fmt.Sprint(s.delays) != "[2s 4s]" {
			t.Errorf("%d: %d calls, delays %v", c.status, calls.Load(), s.delays)
		}
	}
}

func TestIsBlockedPageLooksAtTheTopOnly(t *testing.T) {
	if !IsBlockedPage("<html>Our systems have detected unusual traffic</html>") {
		t.Error("block page not detected")
	}
	if IsBlockedPage(strings.Repeat("é", 1000) + "captcha") {
		t.Error("phrase past the first 1000 characters counted")
	}
}

func TestPythonFormatting(t *testing.T) {
	if got := thousands(1234567); got != "1,234,567" {
		t.Errorf("thousands %s", got)
	}
	if got := thousands(999); got != "999" {
		t.Errorf("thousands %s", got)
	}
	if got := formatRating(4.25); got != "4.2" { // round half to even, like Python
		t.Errorf("rating %s", got)
	}
	if v, ok := pyInt("١٢٣"); !ok || v != 123 {
		t.Errorf("arabic-indic digits %d %v", v, ok)
	}
	if v, ok := pyFloat("𝟒.𝟓"); !ok || v != 4.5 {
		t.Errorf("mathematical digits %v %v", v, ok)
	}
	if _, ok := pyFloat("4.6."); ok {
		t.Error("4.6. parsed")
	}
	long := strings.Repeat("é", 2005)
	if got := truncateDescription(long); got != strings.Repeat("é", 2000)+"..." {
		t.Errorf("truncated to %d runes", len([]rune(got)))
	}
}

func TestUnicodeAwarePatterns(t *testing.T) {
	// Python's \s matches a no-break space; Go's does not.
	if m := ratedRe.FindStringSubmatch("Rated 4.5 stars out of five stars"); m == nil || m[1] != "4.5" {
		t.Errorf("rated %v", m)
	}
	if m := lastUpdatedRes[0].FindStringSubmatch("Updated on Março 3, 2026"); m == nil || m[1] != "Março 3, 2026" {
		t.Errorf("updated %v", m)
	}
}
