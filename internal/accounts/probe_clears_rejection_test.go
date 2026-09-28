package accounts

// Issue #229's third and fourth required tests, exercised through ProbeNow
// — the exact path the dashboard's "check now" button (#192) calls. The
// pool-level mechanism (pool.ClearRecoveredWindowRejections) is pinned
// directly in internal/pool/window_recovery_test.go; these two prove it is
// actually wired into the probe path, end to end over HTTP — and,
// following the coordinator's live-daemon review, through the FAMILY
// probe specifically: the fake upstream here only ever answers the
// ordinary (cheap, non-fable) probe model with plain 5h/7d headers, the
// way a real Anthropic response does — the whole point of this fix is
// that clearing must NOT depend on the ordinary probe carrying fable
// evidence, since it never does.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

// rejectedAccount is healthy on its general windows (so ProbeNow never
// refuses it as a purchase — a fable rejection alone never makes wouldBill
// true, see readsSpent) and carries a live "7d-fable" rejection.
func rejectedAccount(name, upstream string) *pool.Account {
	a := claudeAccount(name)
	a.Upstream = upstream
	now := time.Now()
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.1, Source: "headers", ResetAt: now.Add(2 * time.Hour), FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 0.2, Source: "headers", ResetAt: now.Add(48 * time.Hour), FetchedAt: now},
	})
	return a
}

// familyAwareUpstream answers the ordinary probe model with plain,
// realistic 5h/7d headers ONLY — never a "7d_oi" (fable) header, matching
// real Anthropic responses to a non-fable model — and answers the fable
// family model (whichever one familyProbeModel resolves to) with
// fableHeaders. Any other model fails the test outright: nothing here
// should ever probe with anything else.
func familyAwareUpstream(t *testing.T, fableModel string, fableUtilization float64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := requestModel(t, r)
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.12")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", fmt.Sprint(time.Now().Add(2*time.Hour).Unix()))
		switch model {
		case fableModel:
			w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Status", "allowed")
			w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Utilization", fmt.Sprint(fableUtilization))
			w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Reset", fmt.Sprint(time.Now().Add(48*time.Hour).Unix()))
		default:
			// The ordinary probe model: no 7d_oi header at all, exactly as
			// a real, non-fable Anthropic response never carries one.
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Required test 3: a probe reading that shows the rejected window refilled
// clears the entry and wakes a parked request promptly. The evidence comes
// from the FAMILY probe (fableModel), never the ordinary one.
func TestProbeNowClearsWindowRejectionWhenFableReadsRefilled(t *testing.T) {
	a0 := claudeAccount("work")
	p0 := pool.New([]*pool.Account{a0}, time.Now())
	fableModel := fableFamilyModel(t, p0, a0)

	srv := familyAwareUpstream(t, fableModel, 0.15)
	a := rejectedAccount("work", srv.URL)
	p := pool.New([]*pool.Account{a}, time.Now())
	p.MarkWindowRejected(a, "7d-fable", time.Now().Add(time.Hour))

	if !a.WindowRejectedFor(fableModelName) {
		t.Fatal("precondition: the rejection must be live, or clearing it proves nothing")
	}

	wake := p.CapacitySignal()
	select {
	case <-wake.Ch():
		t.Fatal("capacity signal fired before ProbeNow was ever called")
	default:
	}

	billed, err := ProbeNow(context.Background(), p, srv.Client(), srv.URL, 30*time.Minute, "work", false, quietLogger())
	if err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	if billed {
		t.Error("billed = true on an account with no extra-usage opt-in and healthy general windows")
	}

	if a.WindowRejectedFor(fableModelName) {
		t.Error("still excluded for fable after the FAMILY probe measured fresh headroom for that window — " +
			"a manual free reset (#228) or an out-of-band provider reset (#135) would sit invisible " +
			"behind this until the (now much longer, issue #229) original deadline arrived on its own")
	}
	select {
	case <-wake.Ch():
	case <-time.After(time.Second):
		t.Fatal("ProbeNow did not signal capacity after clearing the rejection — a parked fable " +
			"request would not be woken promptly")
	}
}

// Required test 4: a probe that returns 200 but reads the rejected window
// still spent must not clear it. Judged from the window reading, never
// from the probe's own HTTP status.
func TestProbeNowDoesNotClearWindowRejectionWhenFableStillReadsSpent(t *testing.T) {
	a0 := claudeAccount("work")
	p0 := pool.New([]*pool.Account{a0}, time.Now())
	fableModel := fableFamilyModel(t, p0, a0)

	srv := familyAwareUpstream(t, fableModel, 1.0) // still fully spent
	a := rejectedAccount("work", srv.URL)
	p := pool.New([]*pool.Account{a}, time.Now())
	p.MarkWindowRejected(a, "7d-fable", time.Now().Add(time.Hour))

	wake := p.CapacitySignal()

	billed, err := ProbeNow(context.Background(), p, srv.Client(), srv.URL, 30*time.Minute, "work", false, quietLogger())
	if err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	if billed {
		t.Error("billed = true unexpectedly")
	}

	if !a.WindowRejectedFor(fableModelName) {
		t.Error("rejection cleared on a 200 whose own FAMILY reading still showed the family fully spent — " +
			"the HTTP status must never stand in for a window-specific measurement")
	}
	select {
	case <-wake.Ch():
		t.Error("capacity signalled for a rejection that was never actually cleared")
	default:
	}
}

// fableModelName is the fable-family model name used by pool's own
// fixtures (see internal/pool/family_test.go's fableModel); duplicated
// here as a plain string since accounts must not import pool's test-only
// identifiers. This is the REQUEST'S model (what a client would send),
// unrelated to familyProbeModel's own choice of PROBE model for that same
// family.
const fableModelName = "claude-opus-4-fable-preview"
