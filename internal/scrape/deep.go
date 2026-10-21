// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package scrape

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ElcanoTek/lens/internal/scrape/webdriver"
)

// CommandResult is a finished command's output.
type CommandResult struct {
	Stdout, Stderr string
	ExitCode       int
}

// Runner runs a command. It returns an error only when the command could
// not run at all (binary missing, context done); a non-zero exit is a
// result.
type Runner interface {
	Run(ctx context.Context, argv []string, env []string) (CommandResult, error)
}

// ExecRunner runs commands with os/exec.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, argv []string, env []string) (CommandResult, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // podman binary and args come from operator config
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := CommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && ctx.Err() == nil {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

// PodmanAvailable reports whether the podman binary can be found, which is
// all the deep rung needs from the host (the WebDriver client is in-tree).
// The auto ladder skips the deep pass when it cannot.
func PodmanAvailable(binary string) bool {
	_, err := exec.LookPath(binary)
	return err == nil
}

func isNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}

// podmanEnv is the environment for podman. Rootless podman needs
// XDG_RUNTIME_DIR to find its runtime root (/run/user/<uid>); systemd
// services with User= don't get it set, so default it when the directory
// exists (it does once lingering is enabled for the service user —
// bootstrap/update handle that).
func podmanEnv() []string {
	env := os.Environ()
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		dir := fmt.Sprintf("/run/user/%d", os.Getuid())
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			env = append(env, "XDG_RUNTIME_DIR="+dir)
		}
	}
	return env
}

func (c *Client) podman(ctx context.Context, args ...string) (CommandResult, error) {
	return c.runner.Run(ctx, append([]string{c.opts.PodmanBinary}, args...), podmanEnv())
}

func freePort() (int, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("unexpected listener address")
	}
	return addr.Port, nil
}

// processAlive reports whether pid runs. A process owned by another user
// (EPERM) counts as alive: it is not ours to clean up after.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH)
}

// ContainerName is the Chrome container this client uses (after Start, the
// per-run name unless KeepContainer).
func (c *Client) ContainerName() string {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return c.containerName
}

// ChromePorts are the WebDriver and VNC host ports (VNC 0 when disabled).
func (c *Client) ChromePorts() (webdriverPort, vncPort int) {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return c.chromePort, c.vncPort
}

// ChromeEndpoint is the WebDriver base URL (without /wd/hub).
func (c *Client) ChromeEndpoint() string {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return c.endpoint
}

// claimPrivateContainer gives this run its own Chrome container and ports.
// The Selenium image serves one browser session, and a run stops its
// container when it finishes. With a shared fixed name and port, two jobs
// running at once would reuse and then kill each other's browser. Each run
// therefore gets <name>-<pid> on free ports. Containers left behind by runs
// that died are removed first. A persistent keep_container setup keeps the
// configured name and port.
func (c *Client) claimPrivateContainer(ctx context.Context) error {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	base := c.opts.ChromeContainerName
	c.removeOrphanedContainers(ctx, base)
	pick := c.opts.FreePort
	if pick == nil {
		pick = freePort
	}
	port, err := pick()
	if err != nil {
		return err
	}
	c.containerName = fmt.Sprintf("%s-%d", base, os.Getpid())
	c.chromePort = port
	if c.vncPort != 0 {
		if c.vncPort, err = pick(); err != nil {
			return err
		}
	}
	c.endpoint = c.defaultEndpoint()
	return nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (c *Client) removeOrphanedContainers(ctx context.Context, base string) {
	res, err := c.podman(ctx, "ps", "-a", "--format", "{{.Names}}")
	if err != nil {
		return
	}
	alive := c.opts.ProcessAlive
	if alive == nil {
		alive = processAlive
	}
	for _, name := range strings.Fields(res.Stdout) {
		suffix, ok := strings.CutPrefix(name, base+"-")
		if !ok || !allDigits(suffix) {
			continue
		}
		pid, err := strconv.Atoi(suffix)
		if err != nil || pid == os.Getpid() || alive(pid) {
			continue // that run is still alive
		}
		log.Info(fmt.Sprintf("Removing Chrome container %s left by a run that exited", name))
		_, _ = c.podman(ctx, "rm", "-f", name)
	}
}

func (c *Client) localImageExists(ctx context.Context) bool {
	res, err := c.podman(ctx, "image", "exists", c.opts.ChromeImage)
	return err == nil && res.ExitCode == 0
}

// isContainerRunning reports whether the container is up. Callers must
// hold cmu.
func (c *Client) isContainerRunning(ctx context.Context) (bool, error) {
	res, err := c.podman(ctx, "ps", "--format", "{{.Names}}")
	if err != nil {
		if isNotFound(err) {
			return false, fmt.Errorf("Podman executable '%s' not found.", c.opts.PodmanBinary) //nolint:revive,staticcheck // message kept from the Python release
		}
		return false, err
	}
	if res.ExitCode != 0 {
		return false, nil
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.TrimSpace(line) == c.containerName {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) runCommand(policy string) []string {
	args := []string{
		"run", "--pull=" + policy, "--rm", "-d",
		"--name", c.containerName,
		"-p", fmt.Sprintf("%d:4444", c.chromePort),
	}
	if c.vncPort != 0 {
		args = append(args, "-p", fmt.Sprintf("%d:7900", c.vncPort))
	}
	args = append(args, c.opts.ChromeExtraArgs...)
	return append(args, c.opts.ChromeImage)
}

func commandOutput(res CommandResult) string {
	if out := strings.TrimSpace(res.Stderr); out != "" {
		return out
	}
	return strings.TrimSpace(res.Stdout)
}

// ensureChromeContainer starts the Chrome container unless it is running,
// and waits for its WebDriver endpoint.
func (c *Client) ensureChromeContainer(ctx context.Context) error {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return c.ensureChromeContainerLocked(ctx)
}

func (c *Client) ensureChromeContainerLocked(ctx context.Context) error {
	running, err := c.isContainerRunning(ctx)
	if err != nil {
		return err
	}
	if running {
		log.Debug(fmt.Sprintf("Chromedriver container %s already running", c.containerName))
		c.containerStarted = true
		return nil
	}
	log.Info(fmt.Sprintf("Starting Chrome container %s using image %s on port %d",
		c.containerName, c.opts.ChromeImage, c.chromePort))

	res, err := c.podman(ctx, c.runCommand(c.opts.ChromePullPolicy)...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		output := commandOutput(res)
		if strings.Contains(strings.ToLower(output), "short-name resolution") {
			return errors.New("Failed to start Chrome container: Podman short-name enforcement blocked " + //nolint:staticcheck // message kept from the Python release
				"pulling the Chrome image. Set `deep_scrape_image` to a fully qualified reference such as " +
				"'docker.io/selenium/standalone-chrome:latest'.")
		}
		// A pre-seeded image should survive registry/userns pull failures:
		// retry once against the local copy before giving up.
		if c.opts.ChromePullPolicy == "never" || !c.localImageExists(ctx) {
			return fmt.Errorf("Failed to start Chrome container: %s", output) //nolint:staticcheck // message kept from the Python release
		}
		reason := "unknown error"
		if lines := strings.Split(output, "\n"); output != "" {
			reason = lines[len(lines)-1]
		}
		log.Warn(fmt.Sprintf("Pulling %s failed (%s); retrying with the locally cached image", c.opts.ChromeImage, reason))
		retry, err := c.podman(ctx, c.runCommand("never")...)
		if err != nil {
			return err
		}
		if retry.ExitCode != 0 {
			return fmt.Errorf("Failed to start Chrome container: %s", commandOutput(retry)) //nolint:staticcheck // message kept from the Python release
		}
	}
	c.containerStarted = true
	return c.waitForContainerReady(ctx)
}

func (c *Client) stopChromeContainer(ctx context.Context) error {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return c.stopChromeContainerLocked(ctx)
}

func (c *Client) stopChromeContainerLocked(ctx context.Context) error {
	c.containerStarted = false
	running, err := c.isContainerRunning(ctx)
	if err != nil {
		return err
	}
	if !running {
		return nil
	}
	res, err := c.podman(ctx, "stop", c.containerName)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		log.Warn(fmt.Sprintf("Failed to stop Chrome container %s: %s", c.containerName, commandOutput(res)))
	}
	return nil
}

var statusClient = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second}

// waitForContainerReady polls the WebDriver status endpoint until it says
// ready or the startup timeout passes. Callers hold cmu.
func (c *Client) waitForContainerReady(ctx context.Context) error {
	statusURL := c.endpoint + "/wd/hub/status"
	deadline := time.Now().Add(c.opts.ChromeStartupTimeout)
	for time.Now().Before(deadline) {
		ready, err := pollStatus(ctx, statusURL)
		if err == nil && ready {
			log.Info(fmt.Sprintf("Chromedriver container %s is ready", c.containerName))
			return nil
		}
		if err != nil {
			log.Debug(fmt.Sprintf("Chromedriver container %s not ready yet (%s). Retrying...", c.containerName, err))
		}
		// The Python release spun without pausing while the endpoint
		// answered "not ready"; pausing either way is kinder to the box.
		if err := c.sleep(ctx, time.Second); err != nil {
			return err
		}
	}
	return errors.New("Timed out waiting for Chrome container to become ready") //nolint:staticcheck // message kept from the Python release
}

func pollStatus(ctx context.Context, statusURL string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return false, err
	}
	resp, err := statusClient.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, err
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false, err
	}
	ready := true
	if m, ok := payload.(map[string]any); ok {
		if value, ok := m["value"].(map[string]any); ok {
			if r, ok := value["ready"]; ok {
				ready = truthy(r)
			}
		}
	}
	return ready, nil
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	}
	return true
}

func (c *Client) createWebdriver(ctx context.Context) (*webdriver.Session, error) {
	// pageLoadStrategy "eager" returns from navigation at DOMContentLoaded
	// instead of the full load event. Ad-heavy pages keep loading trackers
	// and creatives for minutes and blow the renderer timeout even though
	// the article DOM rendered long ago; ChromeWaitAfterLoad still gives
	// client-side JS a beat to paint.
	caps := webdriver.ChromeCapabilities(webdriver.ChromeOptions{
		Args: []string{
			"--headless=new",
			"--disable-gpu",
			"--no-sandbox",
			"--disable-dev-shm-usage",
			"--remote-allow-origins=*",
			"--user-agent=" + c.opts.UserAgent,
			// Basic stealth: hide the automation fingerprints that trivially
			// give away headless Chrome (navigator.webdriver, the automation
			// infobar, and headless Chrome's default 800x600 window).
			"--disable-blink-features=AutomationControlled",
			"--window-size=1920,1080",
			"--lang=en-US",
		},
		ExcludeSwitches:  []string{"enable-automation"},
		PageLoadStrategy: "eager",
		PageLoadTimeout:  c.opts.RequestTimeout,
	})
	driver, err := webdriver.NewSession(ctx, c.ChromeEndpoint()+"/wd/hub", caps)
	if err != nil {
		return nil, err
	}
	if err := driver.SetPageLoadTimeout(ctx, c.opts.RequestTimeout); err != nil {
		// Never leave a browser session behind in the single-session image.
		c.safeQuit(ctx, driver)
		return nil, err
	}
	return driver, nil
}

func (c *Client) safeQuit(ctx context.Context, driver *webdriver.Session) {
	if driver == nil {
		return
	}
	if err := driver.Quit(context.WithoutCancel(ctx)); err != nil {
		log.Debug(fmt.Sprintf("Ignoring error while quitting driver: %s", err))
	}
}

func (c *Client) scrapeWithChrome(ctx context.Context, s *Session, target string, timeout time.Duration, allowRecovery bool) (*fetched, error) {
	if s == nil {
		return nil, errors.New("no scraper session")
	}
	driver := s.getDriver()
	if driver == nil {
		var err error
		if driver, err = c.createWebdriver(ctx); err != nil {
			return nil, err
		}
		s.setDriver(driver)
	}
	page, err := c.chromeFetch(ctx, driver, target, timeout)
	if err == nil {
		return page, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var wdErr *webdriver.Error
	if !errors.As(err, &wdErr) {
		return nil, err // the redirect policy, not the browser
	}
	if allowRecovery && c.shouldRestartChrome(ctx, err) {
		log.Warn(fmt.Sprintf("Chrome driver error encountered (%s). Attempting recovery...", err))
		if err := c.recoverChromeSession(ctx, s, err); err != nil {
			return nil, err
		}
		page, err := c.scrapeWithChrome(ctx, s, target, timeout, false)
		if page != nil {
			page.retries++
		}
		return page, err
	}
	return nil, fmt.Errorf("Chrome scraping failed: %w", err) //nolint:staticcheck // message kept from the Python release
}

func (c *Client) chromeFetch(ctx context.Context, driver *webdriver.Session, target string, timeout time.Duration) (*fetched, error) {
	if err := driver.SetPageLoadTimeout(ctx, timeout); err != nil {
		return nil, err
	}
	if err := driver.Navigate(ctx, target); err != nil {
		return nil, err
	}
	if c.opts.ChromeWaitAfterLoad > 0 {
		if err := c.sleep(ctx, c.opts.ChromeWaitAfterLoad); err != nil {
			return nil, err
		}
	}
	finalURL := target
	if !c.opts.AllowCrossHostRedirects {
		current, err := driver.CurrentURL(ctx)
		if err != nil {
			return nil, err
		}
		if err := c.ensureSameHost(target, current); err != nil {
			return nil, err
		}
		finalURL = current
	}
	html, err := driver.PageSource(ctx)
	if err != nil {
		return nil, err
	}
	return &fetched{Page: ParseHTML(target, html), status: http.StatusOK, finalURL: finalURL}, nil
}

// restartSignatures are driver errors that mean the browser itself is gone.
var restartSignatures = []string{
	"invalid session id",
	"chrome not reachable",
	"chrome failed to start",
	"disconnected",
	"cannot find session",
}

func (c *Client) shouldRestartChrome(ctx context.Context, err error) bool {
	message := strings.ToLower(err.Error())
	for _, sig := range restartSignatures {
		if strings.Contains(message, sig) {
			return true
		}
	}
	c.cmu.Lock()
	defer c.cmu.Unlock()
	running, qerr := c.isContainerRunning(ctx)
	if qerr != nil {
		return true // if podman cannot be queried, assume a restart is needed
	}
	return !running
}

func (c *Client) recoverChromeSession(ctx context.Context, s *Session, cause error) error {
	c.safeQuit(ctx, s.getDriver())
	s.setDriver(nil)
	c.cmu.Lock()
	log.Warn(fmt.Sprintf("Restarting Chrome container %s after failure: %s", c.containerName, cause))
	err := c.stopChromeContainerLocked(ctx)
	if err == nil {
		err = c.ensureChromeContainerLocked(ctx)
	}
	c.cmu.Unlock()
	if err != nil {
		return err
	}
	driver, err := c.createWebdriver(ctx)
	if err != nil {
		return err
	}
	s.setDriver(driver)
	return nil
}
