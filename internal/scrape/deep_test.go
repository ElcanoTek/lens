// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package scrape

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/lens/internal/scrape/webdriver"
	"github.com/ElcanoTek/lens/internal/scrape/webdriver/wdtest"
)

// fakePodman is a Runner that keeps a set of containers in memory.
type fakePodman struct {
	mu        sync.Mutex
	calls     [][]string
	existing  []string // what "ps -a" lists
	running   map[string]bool
	runFails  map[string]CommandResult // by pull policy
	imageHere bool
}

func newFakePodman() *fakePodman {
	return &fakePodman{running: map[string]bool{}, runFails: map[string]CommandResult{}}
}

func (f *fakePodman) Run(_ context.Context, argv []string, _ []string) (CommandResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	args := argv[1:]
	f.calls = append(f.calls, args)
	switch {
	case len(args) >= 2 && args[0] == "ps" && args[1] == "-a":
		return CommandResult{Stdout: strings.Join(f.existing, "\n")}, nil
	case len(args) >= 1 && args[0] == "ps":
		var names []string
		for n := range f.running {
			names = append(names, n)
		}
		return CommandResult{Stdout: strings.Join(names, "\n")}, nil
	case len(args) >= 1 && args[0] == "run":
		policy := strings.TrimPrefix(args[1], "--pull=")
		if res, ok := f.runFails[policy]; ok {
			return res, nil
		}
		for i, a := range args {
			if a == "--name" {
				f.running[args[i+1]] = true
			}
		}
		return CommandResult{Stdout: "abc123\n"}, nil
	case len(args) >= 1 && args[0] == "stop":
		delete(f.running, args[1])
		return CommandResult{}, nil
	case len(args) >= 2 && args[0] == "image" && args[1] == "exists":
		if f.imageHere {
			return CommandResult{}, nil
		}
		return CommandResult{ExitCode: 1}, nil
	}
	return CommandResult{}, nil
}

func (f *fakePodman) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func deepOptions(podman *fakePodman, endpoint string) Options {
	o := DefaultOptions()
	o.Mode = ModeDeep
	o.RequestTimeout = 9 * time.Second
	o.ChromeWaitAfterLoad = 0
	o.Runner = podman
	o.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	o.ChromeEndpoint = endpoint
	return o
}

func TestEachRunClaimsItsOwnNameAndPorts(t *testing.T) {
	c := New(deepOptions(newFakePodman(), ""))
	if err := c.claimPrivateContainer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("elcano-lens-chromedriver-%d", os.Getpid()); c.ContainerName() != want {
		t.Errorf("name %s", c.ContainerName())
	}
	port, vnc := c.ChromePorts()
	if port == 4444 || vnc == 7900 || vnc == 0 {
		t.Errorf("ports %d %d", port, vnc)
	}
	if c.ChromeEndpoint() != fmt.Sprintf("http://127.0.0.1:%d", port) {
		t.Errorf("endpoint %s", c.ChromeEndpoint())
	}
}

func TestVNCStaysOffWhenDisabled(t *testing.T) {
	o := deepOptions(newFakePodman(), "")
	o.ChromeVNCPort = 0
	c := New(o)
	if err := c.claimPrivateContainer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, vnc := c.ChromePorts(); vnc != 0 {
		t.Errorf("vnc %d", vnc)
	}
	if run := strings.Join(c.runCommand("always"), " "); strings.Contains(run, ":7900") {
		t.Errorf("run command maps VNC: %s", run)
	}
}

func TestOrphansFromDeadRunsAreRemoved(t *testing.T) {
	const alive, dead = 111, 222
	podman := newFakePodman()
	podman.existing = []string{
		fmt.Sprintf("elcano-lens-chromedriver-%d", alive),
		fmt.Sprintf("elcano-lens-chromedriver-%d", dead),
		"elcano-lens-chromedriver", // a keep_container instance: not ours to touch
		"something-else-333",
	}
	o := deepOptions(podman, "")
	o.ProcessAlive = func(pid int) bool { return pid != dead }
	if err := New(o).claimPrivateContainer(context.Background()); err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, cmd := range podman.commands() {
		if name, ok := strings.CutPrefix(cmd, "rm -f "); ok {
			removed = append(removed, name)
		}
	}
	if want := []string{fmt.Sprintf("elcano-lens-chromedriver-%d", dead)}; !slices.Equal(removed, want) {
		t.Errorf("removed %v", removed)
	}
}

func TestKeepContainerUsesTheConfiguredName(t *testing.T) {
	fake := wdtest.New(t)
	podman := newFakePodman()
	o := deepOptions(podman, fake.Endpoint)
	o.KeepContainer = true
	c := New(o)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if port, _ := c.ChromePorts(); c.ContainerName() != "elcano-lens-chromedriver" || port != 4444 {
		t.Errorf("name %s port %d", c.ContainerName(), port)
	}
	want := "run --pull=always --rm -d --name elcano-lens-chromedriver -p 4444:4444 -p 7900:7900 docker.io/selenium/standalone-chrome:latest"
	if !slices.Contains(podman.commands(), want) {
		t.Errorf("commands %v", podman.commands())
	}
	// A kept container outlives the run.
	_ = c.Close(context.Background())
	if slices.ContainsFunc(podman.commands(), func(s string) bool { return strings.HasPrefix(s, "stop") }) {
		t.Error("kept container was stopped")
	}
}

func TestDefaultDeepRunClaimsBeforeStartingAndStopsOnClose(t *testing.T) {
	fake := wdtest.New(t)
	podman := newFakePodman()
	c := New(deepOptions(podman, fake.Endpoint))
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("elcano-lens-chromedriver-%d", os.Getpid())
	if c.ContainerName() != name || !podman.running[name] {
		t.Fatalf("name %s, running %v", c.ContainerName(), podman.running)
	}
	if err := c.CreateSessionPool(context.Background(), 4); err != nil { // forced down to one
		t.Fatal(err)
	}
	if st := c.SessionStats(); st.Total != 1 {
		t.Errorf("pool %+v", st)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if podman.running[name] || fake.SessionCount() != 0 {
		t.Error("container or browser session left behind")
	}
}

func TestPullFailureFallsBackToTheLocalImage(t *testing.T) {
	fake := wdtest.New(t)
	podman := newFakePodman()
	podman.runFails["always"] = CommandResult{ExitCode: 125, Stderr: "Trying to pull...\nError: registry unreachable"}
	podman.imageHere = true
	c := New(deepOptions(podman, fake.Endpoint))
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(podman.commands(), func(s string) bool { return strings.HasPrefix(s, "run --pull=never") }) {
		t.Errorf("commands %v", podman.commands())
	}

	podman = newFakePodman()
	podman.runFails["always"] = CommandResult{ExitCode: 125, Stderr: "Error: short-name resolution enforced"}
	podman.imageHere = true
	err := New(deepOptions(podman, fake.Endpoint)).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "deep_scrape_image") {
		t.Errorf("short-name: %v", err)
	}

	podman = newFakePodman()
	podman.runFails["always"] = CommandResult{ExitCode: 125, Stderr: "Error: boom"}
	err = New(deepOptions(podman, fake.Endpoint)).Start(context.Background())
	if err == nil || err.Error() != "Failed to start Chrome container: Error: boom" {
		t.Errorf("no local image: %v", err)
	}
}

type missingPodman struct{}

func (missingPodman) Run(context.Context, []string, []string) (CommandResult, error) {
	return CommandResult{}, fmt.Errorf("exec: %w", os.ErrNotExist)
}

func TestMissingPodmanFailsStart(t *testing.T) {
	o := deepOptions(nil, "")
	o.Runner = missingPodman{}
	err := New(o).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Podman executable 'podman' not found.") {
		t.Errorf("got %v", err)
	}
}

func TestChromeContainerWaitRetriesConnectionReset(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wd/hub/status" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close() // connection reset by peer
			return
		}
		_, _ = w.Write([]byte(`{"value": {"ready": true}}`))
	}))
	defer srv.Close()
	o := deepOptions(newFakePodman(), srv.URL)
	o.ChromeStartupTimeout = 5 * time.Second
	c := New(o)
	if err := c.waitForContainerReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Errorf("%d attempts", attempts)
	}
}

func TestChromeContainerWaitTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"value": {"ready": false}}`))
	}))
	defer srv.Close()
	o := deepOptions(newFakePodman(), srv.URL)
	o.ChromeStartupTimeout = 30 * time.Millisecond
	o.Sleep = Sleep
	err := New(o).waitForContainerReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Timed out") {
		t.Errorf("got %v", err)
	}
}

// startedDeepClient is a deep client whose container "runs" in the fake
// podman and whose WebDriver endpoint is the fake.
func startedDeepClient(t *testing.T, fake *wdtest.Server, mutate func(*Options)) (*Client, *fakePodman) {
	t.Helper()
	podman := newFakePodman()
	o := deepOptions(podman, fake.Endpoint)
	o.KeepContainer = true
	if mutate != nil {
		mutate(&o)
	}
	c := New(o)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, podman
}

func TestDeepPassThroughTheFakeEndpoint(t *testing.T) {
	fake := wdtest.New(t)
	c, _ := startedDeepClient(t, fake, nil)
	ctx := context.Background()
	if err := c.CreateSessionPool(ctx, 1); err != nil {
		t.Fatal(err)
	}
	s, err := c.AcquireSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := c.Scrape(ctx, s, "https://example.com/")
	c.ReleaseSession(s)
	if !r.Success || r.Title != "Fake page" || r.Content != "Fake page Rendered body" || r.StatusCode != 200 || r.Mode != ModeDeep {
		t.Fatalf("result %+v", r)
	}

	var body map[string]any
	for _, req := range fake.Requests() {
		if req.Method == http.MethodPost && req.Path == "/wd/hub/session" {
			body = req.Body.(map[string]any)
			break
		}
	}
	capsMap := body["capabilities"].(map[string]any)["alwaysMatch"].(map[string]any)
	var args []string
	for _, a := range capsMap["goog:chromeOptions"].(map[string]any)["args"].([]any) {
		args = append(args, a.(string))
	}
	if !slices.Contains(args, "--headless=new") || !slices.Contains(args, "--disable-blink-features=AutomationControlled") ||
		!slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--user-agent=") }) {
		t.Errorf("args %v", args)
	}
	if capsMap["pageLoadStrategy"] != "eager" || fmt.Sprint(capsMap["timeouts"]) != "map[pageLoad:9000]" {
		t.Errorf("caps %v", capsMap)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.SessionCount() != 0 {
		t.Error("close left the browser session open")
	}
}

func TestDeepScrapeWrapsNavigationTimeouts(t *testing.T) {
	fake := wdtest.New(t)
	c, _ := startedDeepClient(t, fake, nil)
	defer func() { _ = c.Close(context.Background()) }()
	fake.Fail("POST", "/url", http.StatusInternalServerError, map[string]any{"value": map[string]any{"error": "timeout", "message": "page load"}})
	ctx := context.Background()
	_ = c.CreateSessionPool(ctx, 1)
	s, _ := c.AcquireSession(ctx)
	defer c.ReleaseSession(s)
	r := c.Scrape(ctx, s, "https://slow.example/")
	if r.Success || r.Error != "Chrome scraping failed: timeout: page load" {
		t.Fatalf("result %+v", r)
	}
}

func TestDeepScrapeRecoversWhenTheSessionIsLost(t *testing.T) {
	fake := wdtest.New(t)
	c, podman := startedDeepClient(t, fake, nil)
	defer func() { _ = c.Close(context.Background()) }()
	ctx := context.Background()
	_ = c.CreateSessionPool(ctx, 1)
	s, _ := c.AcquireSession(ctx)
	defer c.ReleaseSession(s)
	before := len(podman.commands())
	fake.ForgetSessions() // Chrome crashed: the endpoint forgot the session

	r := c.Scrape(ctx, s, "https://example.com/")
	if !r.Success || r.Title != "Fake page" || r.RetriesUsed != 1 {
		t.Fatalf("result %+v", r)
	}
	after := podman.commands()[before:]
	if !slices.Contains(after, "stop elcano-lens-chromedriver") ||
		!slices.ContainsFunc(after, func(s string) bool { return strings.HasPrefix(s, "run ") }) {
		t.Errorf("recovery commands %v", after)
	}
	if id := s.getDriver().ID(); id != "s2" {
		t.Errorf("session id %s", id)
	}
}

func TestDeepRedirectPolicy(t *testing.T) {
	fake := wdtest.New(t)
	fake.RedirectTo("https://parked-lander.com/x")
	c, _ := startedDeepClient(t, fake, nil)
	defer func() { _ = c.Close(context.Background()) }()
	ctx := context.Background()
	_ = c.CreateSessionPool(ctx, 1)
	s, _ := c.AcquireSession(ctx)
	r := c.Scrape(ctx, s, "https://example.com/")
	c.ReleaseSession(s)
	if r.Success || r.Error != "URL redirected to different host: parked-lander.com" {
		t.Fatalf("result %+v", r)
	}

	following, _ := startedDeepClient(t, fake, func(o *Options) { o.AllowCrossHostRedirects = true })
	defer func() { _ = following.Close(context.Background()) }()
	_ = following.CreateSessionPool(ctx, 1)
	s, _ = following.AcquireSession(ctx)
	r = following.Scrape(ctx, s, "https://example.com/")
	following.ReleaseSession(s)
	if !r.Success {
		t.Fatalf("result %+v", r)
	}
}

func TestCreateWebdriverQuitsWhenSetupFails(t *testing.T) {
	fake := wdtest.New(t)
	fake.Fail("POST", "/timeouts", http.StatusInternalServerError, map[string]any{"value": map[string]any{"error": "unknown error", "message": "x"}})
	c := New(deepOptions(newFakePodman(), fake.Endpoint))
	if _, err := c.createWebdriver(context.Background()); !webdriver.HasCode(err, webdriver.CodeUnknown) {
		t.Fatalf("got %v", err)
	}
	found := false
	for _, r := range fake.Requests() {
		if r.Method == http.MethodDelete && r.Path == "/wd/hub/session/s1" {
			found = true
		}
	}
	if !found || fake.SessionCount() != 0 {
		t.Error("browser session left behind")
	}
}
