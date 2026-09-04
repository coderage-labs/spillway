package accounts

// Tests for issue #90's re-probe: an exhausted account is never selected
// for real traffic, so nothing else ever learns its bench was wrong. These
// exercise ProbeIdle (the same probeOnStart/probeInterval machinery that
// already fills idle tanks) against an account that starts StateExhausted.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

// A re-probe that comes back healthy must clear the exhaustion immediately
// — the live-observed case (a restart found the account StateOK, serving,
// with its weekly window at 17% remaining, days before its recorded
// deadline) without needing a restart to discover it.
func TestReprobeOfExhaustedAccountThatRecoversClearsExhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.1")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.17")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_ok"}`))
	}))
	defer srv.Close()

	a := pool.NewAccount("recovers", pool.SourceYAML, "tok", "", 0, srv.URL)
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, time.Now())
	p.MarkExhausted(a, time.Now().Add(3*24*time.Hour))

	if a.State() != pool.StateExhausted {
		t.Fatal("precondition: account must start exhausted")
	}

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, 30*time.Minute, quietLogger())

	if a.State() != pool.StateOK {
		t.Fatalf("State() = %v, want StateOK: a healthy re-probe must clear the bench", a.State())
	}
	if !a.ExhaustedUntil().IsZero() {
		t.Errorf("ExhaustedUntil = %v, want zero after recovery", a.ExhaustedUntil())
	}
}

// A re-probe that is rejected again must extend the bench to the fresh
// deadline and must NOT re-fire on the very next sweep: the backoff it sets
// is exactly what stops a genuinely spent account from being probed every
// tick.
func TestReprobeRejectedAgainExtendsBenchAndBacksOff(t *testing.T) {
	var hits int32
	// Truncated to the second: reset headers are unix seconds, so the
	// reconstructed time.Unix(sec, 0) never carries sub-second precision.
	soon := time.Now().Add(90 * time.Minute).Truncate(time.Second)
	far := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "1")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", fmt.Sprint(soon.Unix()))
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.9")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", fmt.Sprint(far.Unix()))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"still exhausted"}}`))
	}))
	defer srv.Close()

	a := pool.NewAccount("still-spent", pool.SourceYAML, "tok", "", 0, srv.URL)
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, time.Now())
	p.MarkExhausted(a, time.Now().Add(time.Hour))

	staleAfter := 30 * time.Minute
	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, staleAfter, quietLogger())

	if hits != 1 {
		t.Fatalf("expected exactly one probe request, got %d", hits)
	}
	if a.State() != pool.StateExhausted {
		t.Fatalf("State() = %v, want StateExhausted: a rejected re-probe must not clear the bench", a.State())
	}
	if got := a.ExhaustedUntil(); !got.Equal(soon) {
		t.Errorf("ExhaustedUntil = %v, want %v (5h's soonest reset, not 7d's)", got, soon)
	}
	next := a.NextProbeAt()
	if next.IsZero() || next.Before(time.Now().Add(staleAfter-time.Second)) {
		t.Fatalf("NextProbeAt = %v, want >= now+staleAfter (%v) so the backoff actually holds it off", next, staleAfter)
	}

	// Immediately sweeping again must NOT re-fire: this is the "no tight
	// loop" guarantee — the backoff set above must actually be honoured by
	// needsProbe, not just recorded and ignored.
	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, staleAfter, quietLogger())
	if hits != 1 {
		t.Fatalf("a second sweep inside the backoff window re-probed the account: hits = %d, want 1", hits)
	}
}

// The backoff gate must bind even once the reading is stale enough that
// the ORDINARY staleness check alone would allow a re-probe — otherwise a
// backoff that only ever matters while the reading happens to still be
// fresh isn't actually preventing anything a plain staleAfter check
// wouldn't already have prevented.
func TestNeedsProbeRespectsBackoffEvenPastOrdinaryStaleness(t *testing.T) {
	a := pool.NewAccount("backed-off", pool.SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, time.Now())
	staleAfter := 30 * time.Minute

	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.5, FetchedAt: time.Now().Add(-2 * time.Hour)},
	})
	p.MarkReprobeRejected(a, time.Now().Add(3*time.Hour), staleAfter)
	// Grow it past staleAfter, the way a second rejected re-probe would.
	p.MarkReprobeRejected(a, time.Now().Add(3*time.Hour), staleAfter)

	if needsProbe(a, false, staleAfter) {
		t.Fatal("needsProbe = true, want false: NextProbeAt's backoff must hold even though " +
			"the reading is already stale enough for the ordinary check alone to allow a re-probe")
	}
}

// A probe must never be a purchase, including a re-probe of an exhausted
// account (issue #90 explicitly extends, not replaces, that rule): an
// account whose window is fully spent AND whose extra usage is enabled must
// not be probed while its own recorded reset is still in the future.
//
// The overage opt-in below is load-bearing since issue #152. It is what
// makes this probe a purchase; without it the provider answers 429 for
// free, and the probe is exactly the re-verification #90 asks for — see
// TestExhaustedAccountThatCannotBillIsStillReprobed.
func TestReprobeNeverBillsAnExhaustedAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a would-bill exhausted account must never be probed")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	now := time.Now()
	a := pool.NewAccount("would-bill", pool.SourceYAML, "tok", "", 0, srv.URL)
	a.Type = "claude-oauth"
	yes := true
	a.SetAllowOverage(&yes)
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 1, ResetAt: now.Add(2 * time.Hour), FetchedAt: now.Add(-6 * time.Hour)},
		{Name: "7d", Limit: 1, Used: 1, ResetAt: now.Add(9 * time.Hour), FetchedAt: now.Add(-6 * time.Hour)},
	})
	p := pool.New([]*pool.Account{a}, time.Now())
	p.MarkExhausted(a, now.Add(2*time.Hour))

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, 30*time.Minute, quietLogger())

	if a.State() != pool.StateExhausted {
		t.Fatalf("State() = %v, want unchanged StateExhausted (no probe ran)", a.State())
	}
}
