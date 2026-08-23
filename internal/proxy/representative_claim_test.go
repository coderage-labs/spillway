package proxy

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
)

// syncBuffer is a mutex-guarded bytes.Buffer: slog writes to it from the
// server's request goroutine while the test reads it from the test
// goroutine, and a plain bytes.Buffer races under `go test -race` even
// when the reads happen to land after every write in practice.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newClaimRig builds a one-account proxy whose upstream sends
// respHeaders on every response, with a logger writing to a buffer the
// test can inspect — the real h.logger the Handler is actually
// constructed with, not a decoy, so a passing assertion here means the
// production code path (proxy.go's call to h.claims.check) really ran.
func newClaimRig(t *testing.T, respHeaders map[string]string) (front *httptest.Server, log *syncBuffer) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range respHeaders {
			w.Header().Set(k, v)
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Defaults()
	cfg.Upstream = upstream.URL
	cfg.Pool.ExhaustedMode = "fail"
	p := pool.New([]*pool.Account{pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, "")}, time.Now())

	log = &syncBuffer{}
	// nil HandlerOptions defaults to LevelInfo — the same default the
	// running daemon uses — so this proves Warn/Info actually surface
	// without needing Debug turned on, and that Debug-only output (the
	// existing "quota signal" line) would NOT appear this way.
	logger := slog.New(slog.NewTextHandler(log, nil))
	h, err := NewHandler(&cfg, logger, p)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	front = httptest.NewServer(h)
	t.Cleanup(front.Close)
	return front, log
}

// postAndWait issues one request and blocks until this handler's serve()
// has fully finished logging it server-side, not just until the client got
// its response.
//
// proxy.go's serve() writes its trailing `msg=request` summary line AFTER
// route() (and so after h.claims.check, called from inside route()) has
// already run — but also after the response bytes are already flushed to
// the client, which is what postMessages waits for (see waitForEntry in
// quota_e2e_test.go for the same gap on the request-log hook). Polling for
// that trailing line, rather than asserting immediately, is what makes the
// count in the dedup test deterministic instead of racy on a loaded runner.
func postAndWait(t *testing.T, url string, log *syncBuffer, wantRequests int) {
	t.Helper()
	resp := postMessages(t, url, testBody)
	resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if strings.Count(log.String(), "msg=request") >= wantRequests {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d requests to finish server-side logging; got:\n%s",
				wantRequests, log.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A response with no Representative-Claim header at all (the ordinary case
// — e.g. every Haiku response, per issue #25) must produce no
// representative-claim log line whatsoever: absence is not a finding.
func TestRepresentativeClaimAbsentHeaderIsNoOp(t *testing.T) {
	front, log := newClaimRig(t, nil)
	postAndWait(t, front.URL, log, 1)

	if strings.Contains(log.String(), "representative-claim") {
		t.Errorf("log mentions representative-claim with no header present:\n%s", log.String())
	}
}

// The one value issue #53 actually measured live: "five_hour" on a fable
// request. #24's static guess for a fable model is {5h, 7d, 7d-fable} —
// candidates, not a single answer — and "five_hour" translates to "5h",
// which IS one of those candidates. That must log nothing: #24's own
// decision ("deprioritising on any governing candidate is defensible")
// means this is a confirmation of the guess, not evidence against it.
func TestRepresentativeClaimMatchingClaimLogsNothing(t *testing.T) {
	front, log := newClaimRig(t, map[string]string{
		"Anthropic-Ratelimit-Unified-Representative-Claim": "five_hour",
	})
	resp := postMessages(t, front.URL, fableReqBody)
	resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(log.String(), "msg=request") < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if strings.Contains(log.String(), "representative-claim") {
		t.Errorf("matching claim logged something, want nothing:\n%s", log.String())
	}
}

// A claim value this package has no translation for must be logged as
// unknown — recognisably different from a mismatch, and it must not crash
// or silently disappear.
func TestRepresentativeClaimUnrecognisedValueLogsAsUnknown(t *testing.T) {
	front, log := newClaimRig(t, map[string]string{
		"Anthropic-Ratelimit-Unified-Representative-Claim": "some_future_bucket_name",
	})
	postAndWait(t, front.URL, log, 1)

	out := log.String()
	if !strings.Contains(out, "representative-claim") || !strings.Contains(out, "unrecognised") {
		t.Fatalf("want an 'unrecognised' representative-claim line, got:\n%s", out)
	}
	if strings.Contains(out, "mismatch") {
		t.Errorf("an unrecognised value must not be logged as a mismatch:\n%s", out)
	}
	if !strings.Contains(out, "some_future_bucket_name") || !strings.Contains(out, "claude-sonnet-4-6") {
		t.Errorf("log should name both the claim and the model asked:\n%s", out)
	}
}

// Same (model, claim) pair recurring across requests must log once, not
// once per request — the flood guard issue #53 asks for so a busy,
// steadily-disagreeing account doesn't bury the one useful line.
func TestRepresentativeClaimDedupSuppressesRepeat(t *testing.T) {
	front, log := newClaimRig(t, map[string]string{
		"Anthropic-Ratelimit-Unified-Representative-Claim": "some_future_bucket_name",
	})

	for i := 1; i <= 3; i++ {
		postAndWait(t, front.URL, log, i)
	}

	n := strings.Count(log.String(), "representative-claim")
	if n != 1 {
		t.Errorf("representative-claim logged %d times for 3 identical requests, want 1:\n%s", n, log.String())
	}
}

// TestRepresentativeClaimMismatchLogsOnceWithBothValues exercises the
// branch where a RECOGNISED claim resolves to a window #24's static guess
// did not predict.
//
// This cannot be produced through the real Claude spec today: the only
// measured translation is "five_hour" -> "5h", and claudeGoverningWindows
// always includes "5h" in its predicted set for every model, fable
// included — so a real response can never disagree by this package's own
// definition of mismatch (claim window absent from the predicted set).
// That is a property of today's single-entry translation table, not
// something this test should paper over by inventing an unmeasured
// header spelling (the brief is explicit: don't).
//
// So this calls the observer directly — the same production method
// proxy.go wires into the real request path (see the full-stack tests
// above for proof that wiring is live) — with a synthetic
// provider.Spec standing in for a future, more complete translation
// table. It still uses a real slog.Logger, captured to a buffer, not a
// decoupled test double, and it is single-goroutine and synchronous, so
// no wait/poll is needed the way the HTTP-path tests above do.
func TestRepresentativeClaimMismatchLogsOnceWithBothValues(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	spec := provider.Spec{
		GoverningWindows: func(model string) []string { return []string{"7d"} }, // deliberately excludes "5h"
	}
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")

	o := newRepresentativeClaimObserver()
	o.check(logger, spec, "claude-sonnet-4-6", h)

	out := buf.String()
	if !strings.Contains(out, "representative-claim") || !strings.Contains(out, "mismatch") {
		t.Fatalf("want a mismatch line, got:\n%s", out)
	}
	if !strings.Contains(out, "five_hour") {
		t.Errorf("log should name the claim value:\n%s", out)
	}
	if !strings.Contains(out, "7d") {
		t.Errorf("log should name what the static map predicted:\n%s", out)
	}
	if strings.Contains(out, "unrecognised") {
		t.Errorf("a recognised value's mismatch must not also read as unrecognised:\n%s", out)
	}

	// Repeating the identical (model, claim) pair must not log again.
	buf.Reset()
	o.check(logger, spec, "claude-sonnet-4-6", h)
	if buf.Len() != 0 {
		t.Errorf("repeat of the same (model, claim) pair logged again: %q", buf.String())
	}
}
