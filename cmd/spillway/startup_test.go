//go:build !windows

package main

// Issue #259: bind first, initialise after.
//
// runServer is driven for real — real config, real pool, real request log,
// real proxy and admin listeners on ephemeral ports — with ONLY the secret
// store substituted (a slow in-memory one standing in for 11 keychain reads
// at 0.4-6.5s each) and the upstream pointed at a local stub. Nothing here
// touches the user's keychain, config or daemon.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/secrets"
)

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestListenerAcceptsBeforeInitAndHeldRequestIsServed(t *testing.T) {
	const initDelay = 1500 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfg := config.Defaults()
	proxyPort, adminPort := freeTCPPort(t), freeTCPPort(t)
	cfg.Proxy.Port = proxyPort
	cfg.Admin.Addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(adminPort))
	cfg.Upstream = upstream.URL
	off := false
	cfg.Pool.ProbeOnStart = &off
	cfg.Pool.CanaryInterval = "0"
	cfg.WatchConfig = &off
	cfg.Accounts = []config.AccountConfig{{
		Name: "work", Type: "claude-oauth",
		ExpiresAt: time.Now().Add(24 * time.Hour).UnixMilli(),
	}}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "spillway.yaml")
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPILLWAY_CONFIG", cfgPath)

	fake := secrets.NewFake()
	if err := fake.Set("work", secrets.Secrets{AccessToken: "tok", RefreshToken: "ref"}); err != nil {
		t.Fatal(err)
	}
	orig := serverSecrets
	serverSecrets = func() secrets.Store {
		time.Sleep(initDelay) // the slow keychain
		return fake
	}
	defer func() { serverSecrets = orig }()

	started := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- runServer([]string{"--config", cfgPath, "--log-file", filepath.Join(dir, "spillway.log")})
	}()

	proxyAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(proxyPort))
	adminBase := "http://127.0.0.1:" + strconv.Itoa(adminPort)

	// 1. The proxy listener accepts long before init completes.
	var conn net.Conn
	for {
		conn, err = net.DialTimeout("tcp", proxyAddr, 100*time.Millisecond)
		if err == nil {
			break
		}
		if time.Since(started) > initDelay/2 {
			t.Fatalf("proxy listener still refusing connections %v in, init takes %v: %v",
				time.Since(started), initDelay, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	conn.Close()
	acceptedAt := time.Since(started)
	if acceptedAt >= initDelay {
		t.Fatalf("listener accepted after %v, not before init finished (%v)", acceptedAt, initDelay)
	}

	// 2. /api/state says "starting" while init is running; other endpoints 503.
	resp, err := http.Get(adminBase + "/api/state")
	if err != nil {
		t.Fatalf("admin listener refused during init: %v", err)
	}
	var st struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || st.Status != "starting" {
		t.Fatalf("/api/state during init = %d %q, want 200 starting", resp.StatusCode, st.Status)
	}
	if r2, err := http.Get(adminBase + "/api/accounts"); err != nil {
		t.Fatal(err)
	} else {
		r2.Body.Close()
		if r2.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("/api/accounts during init = %d, want 503", r2.StatusCode)
		}
	}

	// 3. An inference request sent now is held, then served — not refused.
	body := `{"model":"claude-sonnet-4-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	reqStart := time.Now()
	r3, err := http.Post("http://"+proxyAddr+"/v1/messages", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("request during init failed (refused?): %v", err)
	}
	b, _ := io.ReadAll(r3.Body)
	r3.Body.Close()
	waited := time.Since(reqStart)
	if r3.StatusCode != http.StatusOK || !strings.Contains(string(b), `"ok":true`) {
		t.Fatalf("held request got %d %s, want 200 from the upstream stub", r3.StatusCode, b)
	}
	if waited < initDelay/3 {
		t.Errorf("request returned after %v: it was not held for init (init takes %v)", waited, initDelay)
	}

	// 4. Ready now: state flips.
	r4, err := http.Get(adminBase + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(r4.Body).Decode(&st)
	r4.Body.Close()
	if st.Status != "ready" {
		t.Errorf("/api/state after init = %q, want ready", st.Status)
	}

	// 5. Startup phases are logged with durations.
	logBytes, _ := os.ReadFile(filepath.Join(dir, "spillway.log"))
	for _, phase := range []string{"config", "log open", "bind", "secrets", "pool", "reqlog open", "CA", "ready"} {
		if !strings.Contains(string(logBytes), `phase="`+phase+`"`) && !strings.Contains(string(logBytes), "phase="+phase+" ") {
			t.Errorf("no startup phase line for %q in the log:\n%s", phase, logBytes)
		}
	}
	if !strings.Contains(string(logBytes), "elapsed=") {
		t.Error("startup phase lines carry no elapsed duration")
	}

	t.Logf("daemon log:\n%s", logBytes)
	// Stop the daemon the way launchd does.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runServer returned %v after SIGTERM", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runServer did not stop after SIGTERM")
	}
}
