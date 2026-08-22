//go:build integration

package main

// The install path, run for real on each platform.
//
// Every bug this path has had was invisible to a unit test: task XML no test
// parsed, a shell wrapper that made "stop the task" and "stop the daemon"
// different events, a replacement that could not bind the port its
// predecessor still held. Three of them were in code that had been "fixed"
// the same day. The only thing that catches that class is installing the
// service, watching a daemon serve, replacing it, and taking it away again.
//
// Guarded twice — a build tag and SPILLWAY_INTEGRATION — because it registers
// a real service and starts a real daemon. It uses a label of its own so it
// cannot disturb the one on the machine, and its own config so it cannot
// reach anybody's accounts.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func requireIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SPILLWAY_INTEGRATION") != "1" {
		t.Skip("set SPILLWAY_INTEGRATION=1 to install a real service")
	}
}

// buildSpillway compiles the binary the service will run.
//
// Not os.Args[0]: under `go test` that is the test binary, and a service
// registered against it would run the test suite at every logon.
func buildSpillway(t *testing.T) string {
	t.Helper()
	name := "spillway"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	// Not t.TempDir(): that is removed when the test ends, and the service
	// definition outlives a failed run. A path that survives makes the
	// leftover diagnosable instead of mysterious.
	dir, err := os.MkdirTemp("", "spillway-itest-*")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("building spillway: %v", err)
	}
	old := selfPath
	selfPath = func() (string, error) { return bin, nil }
	t.Cleanup(func() { selfPath = old })
	return bin
}

// isolate points the service and the config somewhere harmless.
func isolate(t *testing.T) (adminAddr string) {
	t.Helper()

	old := serviceLabel
	serviceLabel = "dev.coderage.spillway-itest"
	t.Cleanup(func() { serviceLabel = old })

	// Ports nobody else is on, so a real daemon on this machine does not make
	// the test pass or fail for the wrong reason. Both are needed: proxy.port
	// is validated as 1-65535, so 0 is rejected rather than meaning "any".
	addr := freeAddr(t)
	proxyPort := freePort(t)

	dir := t.TempDir()
	cfg := filepath.Join(dir, "spillway.yaml")
	body := fmt.Sprintf(`upstream: https://api.anthropic.com
accounts:
    - name: itest
      type: claude-oauth
      accessToken: not-a-real-token
      expiresAt: 99999999999999
admin:
    addr: %s
proxy:
    port: %d
pool:
    probeOnStart: false
log:
    level: info
`, addr, proxyPort)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// The daemon is started by the platform's scheduler, not by us, so the
	// only way it sees this config is an environment variable the scheduler
	// passes on. setServiceEnv is where each platform arranges that.
	t.Setenv("SPILLWAY_CONFIG", cfg)
	return addr
}

func waitServing(t *testing.T, addr string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	c := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		resp, err := c.Get("http://" + addr + "/api/state")
		if err == nil {
			var v map[string]any
			json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// The whole lifecycle, in the order a user meets it.
func TestServiceLifecycle(t *testing.T) {
	requireIntegration(t)
	addr := isolate(t)
	t.Logf("built %s", buildSpillway(t))

	t.Cleanup(func() {
		// Whatever happened, do not leave a service behind.
		_ = serviceUninstall()
	})

	if err := serviceInstall(); err != nil {
		t.Fatalf("service install: %v", err)
	}
	if !waitServing(t, addr, 30*time.Second) {
		t.Fatalf("the service was installed but no daemon is serving on %s", addr)
	}
	first := daemonPID(t)
	if first == 0 {
		t.Fatal("could not find the daemon's pid; the platform helper needs fixing")
	}
	t.Logf("serving, pid %d", first)

	// status must agree that it is there.
	if err := serviceStatus(); err != nil {
		t.Errorf("service status: %v", err)
	}

	// The upgrade: reinstall over the running daemon. The process must be
	// replaced — not left alone, which serves the old binary for ever, and
	// not killed without a successor, which is an outage until next logon.
	if err := serviceInstall(); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if !waitServing(t, addr, 60*time.Second) {
		t.Fatal("nothing is serving after a reinstall: an upgrade would take the proxy down")
	}
	second := daemonPID(t)
	if second == first {
		t.Errorf("reinstall left pid %d alone, so an upgrade would keep serving the old binary", first)
	}
	t.Logf("replaced, pid %d", second)

	if err := serviceUninstall(); err != nil {
		t.Fatalf("service uninstall: %v", err)
	}
	// It has to actually stop, not merely be deregistered: a daemon with no
	// service left to stop it by can only be killed by hand.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if daemonPID(t) == 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Errorf("a daemon is still running after uninstall, with no service to stop it by")
}

// freeAddr and freePort hand out something nothing is listening on. Racy in
// principle; in practice the window is microseconds and the alternative is a
// hard-coded port that collides with the daemon on the developer's machine.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
