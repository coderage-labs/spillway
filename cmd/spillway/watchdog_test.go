package main

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuffer guards a bytes.Buffer with a mutex: startupWatchdog logs from
// its own goroutine, and the test reads the buffer from the main test
// goroutine while that goroutine may still be running, which a plain
// bytes.Buffer does not allow race-free.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func bufLogger() (*slog.Logger, *safeBuffer) {
	buf := &safeBuffer{}
	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

// Issue #104: a hang anywhere between process start and the listeners
// binding used to be completely silent — launchd reported the daemon
// running and nothing answered. startupWatchdog is the loud-logging
// backstop for that. When the listeners bind well within the threshold, it
// must stay silent.
func TestStartupWatchdogSilentWhenBoundInTime(t *testing.T) {
	logger, buf := bufLogger()
	bound := make(chan struct{})
	close(bound) // already bound before the watchdog even starts waiting

	done := make(chan struct{})
	go func() {
		startupWatchdog(logger, bound, 50*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startupWatchdog did not return promptly when bound was already closed")
	}

	if s := buf.String(); s != "" {
		t.Errorf("watchdog logged something despite binding in time: %s", s)
	}
}

// If binding takes longer than the threshold, the watchdog must log loudly
// — that loud line is the entire fix for issue #104's "nothing answers and
// nothing says why" symptom.
func TestStartupWatchdogLogsLoudlyWhenBindingIsSlow(t *testing.T) {
	logger, buf := bufLogger()
	bound := make(chan struct{})

	done := make(chan struct{})
	go func() {
		startupWatchdog(logger, bound, 20*time.Millisecond)
		close(done)
	}()

	// Let the threshold elapse before ever binding.
	time.Sleep(120 * time.Millisecond)
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "not bound its listeners") {
		t.Fatalf("want a loud ERROR line once the threshold passes without binding, got: %q", buf.String())
	}

	close(bound)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startupWatchdog did not return after bound closed late")
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "exceeding the startup watchdog threshold") {
		t.Fatalf("want a follow-up WARN line once binding finally completes, got: %q", buf.String())
	}
}
