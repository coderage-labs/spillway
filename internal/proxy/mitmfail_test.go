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

// TestNormalizeHandshakeDetailStripsEphemeralPort: Go's http.Server prefixes
// every TLS handshake error with "http: TLS handshake error from
// <client-addr>: " — and <client-addr> carries the client's ephemeral
// source port, which is different on literally every attempt (a fresh
// CONNECT is a fresh TCP connection). Real messages captured from this
// package's own MITM path (TestStaleCAWarningNeedsBothRegenerationAndRecurrence's
// underlying rig) confirm the exact format below. Without stripping it, no
// two occurrences of the same underlying failure ever compare equal, which
// silently limits #64's dedup to a coincidence that never happens and
// defeats issue #66's stale-CA detector outright.
func TestNormalizeHandshakeDetailStripsEphemeralPort(t *testing.T) {
	a := normalizeHandshakeDetail("http: TLS handshake error from 127.0.0.1:58603: remote error: tls: bad certificate")
	b := normalizeHandshakeDetail("http: TLS handshake error from 127.0.0.1:58604: remote error: tls: bad certificate")
	if a != b {
		t.Fatalf("two occurrences of the same failure normalized to different strings: %q vs %q", a, b)
	}
	if a != "remote error: tls: bad certificate" {
		t.Errorf("normalized = %q, want the reason with the address prefix stripped", a)
	}

	// IPv6 loopback: the address form differs, but the invariant is the
	// same — two attempts differing only by ephemeral port must normalize
	// identically.
	c := normalizeHandshakeDetail("http: TLS handshake error from [::1]:58603: EOF")
	d := normalizeHandshakeDetail("http: TLS handshake error from [::1]:9999: EOF")
	if c != d || c != "EOF" {
		t.Errorf("IPv6 form: got %q and %q, want both %q", c, d, "EOF")
	}

	// A message with no such prefix (defensive: any other ErrorLog line)
	// passes through unchanged rather than being mangled.
	if got := normalizeHandshakeDetail("some other server error"); got != "some other server error" {
		t.Errorf("non-matching message was altered: %q", got)
	}
}

// TestNormalizeHandshakeDetailCollapsesAllThreeRealShapes exercises the
// exact three message shapes a real production log held (10,608 distinct
// raw detail strings across roughly 24 hours): a bare TLS-layer EOF, a
// bare TLS alert, and — the one the original fix missed — a network
// read/write error whose *net.OpError text carries a SECOND, independent
// ephemeral port ("read tcp <local>-><remote>: <reason>") that survives a
// normalization stripping only the leading address. About 2,100 of the
// ~10,600 real failures (roughly 20%) were this third shape, and every one
// of them carried a distinct port — so this case is asserted explicitly
// rather than folded into the pair above.
func TestNormalizeHandshakeDetailCollapsesAllThreeRealShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw1    string
		raw2    string
		wantOne string
	}{
		{
			name:    "bare EOF",
			raw1:    "http: TLS handshake error from 127.0.0.1:61001: EOF",
			raw2:    "http: TLS handshake error from 127.0.0.1:61002: EOF",
			wantOne: "EOF",
		},
		{
			name:    "bare TLS alert",
			raw1:    "http: TLS handshake error from 127.0.0.1:61003: local error: tls: bad record MAC",
			raw2:    "http: TLS handshake error from 127.0.0.1:61004: local error: tls: bad record MAC",
			wantOne: "local error: tls: bad record MAC",
		},
		{
			// The shape the original fix missed: a second address pair
			// nested inside the net.OpError text, past the leading one
			// handshakeErrFromAddr already strips.
			name:    "connection reset by peer (nested address)",
			raw1:    "http: TLS handshake error from 127.0.0.1:61018: read tcp 127.0.0.1:7654->127.0.0.1:61018: read: connection reset by peer",
			raw2:    "http: TLS handshake error from 127.0.0.1:61041: read tcp 127.0.0.1:7654->127.0.0.1:61041: read: connection reset by peer",
			wantOne: "read tcp <addr>-><addr>: read: connection reset by peer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := normalizeHandshakeDetail(tc.raw1)
			b := normalizeHandshakeDetail(tc.raw2)
			if a != b {
				t.Fatalf("two occurrences of the same failure normalized to different strings: %q vs %q", a, b)
			}
			if a != tc.wantOne {
				t.Errorf("normalized = %q, want %q", a, tc.wantOne)
			}
		})
	}
}

// ── issue #66: stale-CA stranded-client detection ──────────────────────────
//
// These exercise mitmFailLogger.Stranded() directly rather than through a
// full daemon: it is armed by activate() (main.go calls this only when
// mitm.CA.Regenerated is true) and fed by the exact same log() calls
// connect.go's slogWriter already makes on every MITM handshake failure.

// TestStrandedNeverArmedByHandshakeFailuresAlone: without a preceding CA
// regeneration (activate never called), no volume of handshake failures —
// not even the same pair recurring rapidly — may report Stranded. This is
// the ordinary-churn false positive the issue explicitly calls out.
func TestStrandedNeverArmedByHandshakeFailuresAlone(t *testing.T) {
	l := newMitmFailLoggerWindow(mfTestLogger(&bytes.Buffer{}), time.Hour)
	for i := 0; i < 5; i++ {
		l.log("api.anthropic.com", "remote error: tls: bad certificate")
	}
	if l.Stranded() {
		t.Error("Stranded() = true with no preceding CA regeneration — a handshake failure alone must never be evidence")
	}
}

// TestStrandedSingleFailureIsNotEvidence: a single handshake failure after a
// genuine regeneration is exactly the "client walked away mid-handshake"
// case connect.go already calls ordinary. One occurrence must not warn.
func TestStrandedSingleFailureIsNotEvidence(t *testing.T) {
	l := newMitmFailLoggerWindow(mfTestLogger(&bytes.Buffer{}), time.Hour)
	l.activate()
	l.log("api.anthropic.com", "remote error: tls: bad certificate")
	if l.Stranded() {
		t.Error("Stranded() = true after a single handshake failure — one failure is ordinary churn, not proof of a stranded client")
	}
}

// TestStrandedRecurringFailureAfterRegenerationWarns: the actual symptom —
// the identical (host, detail) failure recurring quickly, after a genuine
// regeneration — is exactly what a client stuck retrying against a replaced
// CA produces, and must warn.
func TestStrandedRecurringFailureAfterRegenerationWarns(t *testing.T) {
	l := newMitmFailLoggerWindow(mfTestLogger(&bytes.Buffer{}), time.Hour)
	l.activate()
	l.log("api.anthropic.com", "remote error: tls: bad certificate")
	l.log("api.anthropic.com", "remote error: tls: bad certificate") // recurs
	if !l.Stranded() {
		t.Error("Stranded() = false after the same MITM failure recurred post-regeneration — this is the symptom issue #66 warns about")
	}
}

// TestStrandedRequiresRegenerationFirst: recurring failures BEFORE
// activate() is ever called must not count — mirrors "failures before the
// regeneration are irrelevant" from the issue, and guards against a caller
// that races activate() after failures have already started arriving.
func TestStrandedRequiresRegenerationFirst(t *testing.T) {
	l := newMitmFailLoggerWindow(mfTestLogger(&bytes.Buffer{}), time.Hour)
	l.log("api.anthropic.com", "remote error: tls: bad certificate")
	l.log("api.anthropic.com", "remote error: tls: bad certificate")
	if l.Stranded() {
		t.Error("Stranded() = true from recurring failures with no regeneration ever recorded")
	}
	l.activate()
	if l.Stranded() {
		t.Error("activating after the fact must not retroactively treat old recurrences as evidence")
	}
}

// TestStrandedDoesNotLatchForever: the decay issue #66 explicitly asks for
// — once the recurring failure stops (every affected session restarted),
// the warning must clear itself rather than stay true for the life of the
// daemon.
func TestStrandedDoesNotLatchForever(t *testing.T) {
	l := newMitmFailLoggerStrandedWindows(mfTestLogger(&bytes.Buffer{}), time.Hour, time.Hour, 20*time.Millisecond)
	l.activate()
	l.log("api.anthropic.com", "remote error: tls: bad certificate")
	l.log("api.anthropic.com", "remote error: tls: bad certificate")
	if !l.Stranded() {
		t.Fatal("precondition: expected Stranded() = true right after the recurrence")
	}
	time.Sleep(40 * time.Millisecond)
	if l.Stranded() {
		t.Error("Stranded() = true after the stale window elapsed with no further failures — a warning must not latch forever (issue #66)")
	}
}

// TestStrandedRecurrenceMustBeWithinRecurWindow: two occurrences of the same
// pair separated by longer than the recur window are treated as unrelated —
// a genuinely stuck client retries far more often than that, so this guards
// against two coincidental one-off failures (e.g. a generic "EOF" on the
// same host, days apart) being mistaken for one client stuck retrying.
func TestStrandedRecurrenceMustBeWithinRecurWindow(t *testing.T) {
	l := newMitmFailLoggerStrandedWindows(mfTestLogger(&bytes.Buffer{}), time.Hour, 20*time.Millisecond, time.Hour)
	l.activate()
	l.log("api.anthropic.com", "EOF")
	time.Sleep(40 * time.Millisecond) // past the recur window
	l.log("api.anthropic.com", "EOF")
	if l.Stranded() {
		t.Error("Stranded() = true for two occurrences further apart than the recur window — that is two unrelated one-offs, not one stuck client")
	}
}
