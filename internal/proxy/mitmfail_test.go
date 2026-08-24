package proxy

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func mfTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestMitmFailLoggerCollapsesRepeats is the test issue #64 asks for
// directly: a permanently failing client (measured live at ~7/min,
// producing 11,665 identical lines) must not be able to produce one log
// line per attempt. A long window means none of these repeats should log
// again after the first.
func TestMitmFailLoggerCollapsesRepeats(t *testing.T) {
	var buf bytes.Buffer
	l := newMitmFailLoggerWindow(mfTestLogger(&buf), time.Hour)

	const attempts = 11665
	for i := 0; i < attempts; i++ {
		l.log("api.anthropic.com", "remote error: tls: bad certificate")
	}

	got := strings.Count(buf.String(), "mitm connection failed")
	if got != 1 {
		t.Fatalf("got %d log lines for %d identical failures, want exactly 1", got, attempts)
	}
}

// TestMitmFailLoggerDistinguishesHostAndDetail: suppressing one repeating
// failure must not swallow a genuinely different one, on another host or
// with another detail on the same host.
func TestMitmFailLoggerDistinguishesHostAndDetail(t *testing.T) {
	var buf bytes.Buffer
	l := newMitmFailLoggerWindow(mfTestLogger(&buf), time.Hour)

	l.log("api.anthropic.com", "remote error: tls: bad certificate")
	l.log("api.anthropic.com", "remote error: tls: bad certificate") // repeat, suppressed
	l.log("api.anthropic.com", "EOF")                                // different detail, same host
	l.log("other.example.com", "remote error: tls: bad certificate") // different host, same detail

	got := strings.Count(buf.String(), "mitm connection failed")
	if got != 3 {
		t.Fatalf("got %d log lines, want 3 (one per distinct host+detail pair)", got)
	}
}

// TestMitmFailLoggerLogsAgainPastWindow: once the window elapses, the next
// occurrence logs again — the failure must not go permanently silent, and
// the re-logged line should carry how many were suppressed in between.
func TestMitmFailLoggerLogsAgainPastWindow(t *testing.T) {
	var buf bytes.Buffer
	l := newMitmFailLoggerWindow(mfTestLogger(&buf), 10*time.Millisecond)

	l.log("api.anthropic.com", "remote error: tls: bad certificate")
	l.log("api.anthropic.com", "remote error: tls: bad certificate") // suppressed, within window

	time.Sleep(20 * time.Millisecond)
	l.log("api.anthropic.com", "remote error: tls: bad certificate") // window elapsed: logs again

	got := strings.Count(buf.String(), "mitm connection failed")
	if got != 2 {
		t.Fatalf("got %d log lines, want 2 (one at start of each window)", got)
	}
	if !strings.Contains(buf.String(), "suppressed_repeats=1") {
		t.Errorf("second log line should report the 1 suppressed repeat, got: %s", buf.String())
	}
}

// TestMitmFailLoggerFirstOccurrenceHasNoSuppressedCount: the very first
// occurrence of a pair must log plainly, with no misleading
// suppressed_repeats=0 noise.
func TestMitmFailLoggerFirstOccurrenceHasNoSuppressedCount(t *testing.T) {
	var buf bytes.Buffer
	l := newMitmFailLoggerWindow(mfTestLogger(&buf), time.Hour)
	l.log("api.anthropic.com", "remote error: tls: bad certificate")

	if strings.Contains(buf.String(), "suppressed_repeats") {
		t.Errorf("first occurrence should not mention suppressed_repeats, got: %s", buf.String())
	}
}
