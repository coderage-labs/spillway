package main

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/admin"
)

func TestMaintenanceStartsOnlyAfterReady(t *testing.T) {
	ready := make(chan struct{})
	var ran atomic.Bool
	done := make(chan struct{})
	go func() {
		startMaintenance(slog.New(slog.NewTextHandler(io.Discard, nil)), ready,
			func(time.Time) (int64, int64, error) { ran.Store(true); return 0, 0, nil })
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	if ran.Load() {
		t.Fatal("maintenance ran before init was ready (it can starve the quota seed)")
	}
	close(ready)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("maintenance never ran after ready")
	}
	if !ran.Load() {
		t.Error("maintenance did not run after ready")
	}
}

func TestInitDeadlineExitsWhenInitHangs(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ph := newStartupPhases()
	ph.begin("secrets")
	exited := make(chan struct{})
	go initDeadline(logger, make(chan struct{}), ph, 50*time.Millisecond, func() { close(exited) })
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("a hung init never caused an exit")
	}
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "secrets") {
		t.Errorf("log = %q, want an ERROR naming the phase", buf.String())
	}
}

func TestInitDeadlineQuietWhenReady(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	var exited atomic.Bool
	initDeadline(slog.New(slog.NewTextHandler(io.Discard, nil)), ready, newStartupPhases(), 20*time.Millisecond,
		func() { exited.Store(true) })
	time.Sleep(60 * time.Millisecond)
	if exited.Load() {
		t.Error("exited despite init finishing")
	}
}

func TestInitTimeoutIsLongerThanRunWait(t *testing.T) {
	if initTimeout <= runWaitTimeout {
		t.Errorf("initTimeout %v must exceed run's wait %v", initTimeout, runWaitTimeout)
	}
}

// A daemon that answers /api/state "starting" but 503s /api/accounts gets
// the starting marker, not the unreachable one.
func TestStatuslineShowsStartingNotUnreachable(t *testing.T) {
	srv := httptest.NewServer(admin.NewStarting())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	cfgPath := t.TempDir() + "/spillway.yaml"
	if err := os.WriteFile(cfgPath, []byte("admin:\n  addr: "+host+"\nproxy:\n  host: 127.0.0.1\n  port: 61989\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPILLWAY_CONFIG", cfgPath)
	t.Setenv("NO_COLOR", "1")
	setAttachEnv(t, "")
	mustBeAttached(t, false)

	got := captureStdout(t, func() {
		if err := runStatusline([]string{"--always"}); err != nil {
			t.Errorf("runStatusline: %v", err)
		}
	})
	if got != "⛁ starting" {
		t.Errorf("line = %q, want %q", got, "⛁ starting")
	}
}
