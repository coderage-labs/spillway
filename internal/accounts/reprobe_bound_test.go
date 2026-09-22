package accounts

// Issue #190: the re-probe backoff hid a bought reset.
//
// Live case. A user bought a free reset on two accounts and spillway did not
// notice for hours. Both read 7d = 1.00, state = exhausted, neither with
// allowOverage — so a probe on either is free, the provider refuses it with a
// 429 and charges nothing. probeInterval was 30m; the last probes were 6h, 8h,
// ~21h and ~22h old. Every rejected re-probe had doubled probeBackoff toward
// its 24h ceiling, and needsProbe declines while now is before NextProbeAt.
// Restarting the daemon (nextProbeAt is memory-only) probed both within
// seconds and both read 7d = 0: the resets were real and invisible.
//
// The fix keeps the growth where a probe is a purchase and caps it where the
// provider can only refuse — freeReprobeCap, four probeIntervals. Both
// directions are proven separately below, because a change that only bounded
// the free case, or only preserved the billable one, would look correct from
// one side and be half wrong.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

// growBackoffToCeiling drives the backoff to its 24h ceiling the way a run of
// rejected re-probes does, all of them recorded as of `at` — so the resulting
// deadline is `at` plus 24h and the test can place the start of the backoff
// in the past without waiting for it (issues #98/#134: no wall-clock waits,
// no wall-clock assertions).
//
// The bench each rejection sets runs a day out from `at`, so an `at` in the
// recent past still leaves the account genuinely StateExhausted now, which is
// the state the live incident was in.
func growBackoffToCeiling(p *pool.Pool, a *pool.Account, base time.Duration, at time.Time) {
	// 30m, 1h, 2h, 4h, 8h, 16h, then clamped to 24h.
	for i := 0; i < 7; i++ {
		p.MarkReprobeRejectedAtForTest(a, at.Add(24*time.Hour), base, at)
	}
}

// refusingUpstream fails the test if it is contacted at all. For the cases
// whose whole assertion is that no probe went out.
func refusingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("probe sent to an account that must not have been probed")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The bound itself, as a computed time rather than an elapsed one. The cap is
// four probeIntervals measured from where the backoff STARTED (next minus
// backoff), and it can only ever bring a re-probe forward.
func TestFreeReprobeBackoffIsCappedAtFourProbeIntervals(t *testing.T) {
	staleAfter := 30 * time.Minute
	start := time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		backoff    time.Duration
		staleAfter time.Duration
		want       time.Time
	}{
		{"inside the cap, deadline untouched", time.Hour, staleAfter, start.Add(time.Hour)},
		{"exactly the cap, deadline untouched", 2 * time.Hour, staleAfter, start.Add(2 * time.Hour)},
		{"past the cap, pulled back to start+cap", 8 * time.Hour, staleAfter, start.Add(2 * time.Hour)},
		{"at #90's 24h ceiling, pulled back to start+cap", 24 * time.Hour, staleAfter, start.Add(2 * time.Hour)},
		{"a 5m interval scales the cap down with it", 24 * time.Hour, 5 * time.Minute, start.Add(20 * time.Minute)},
		{"a 4h interval scales the cap up with it", 24 * time.Hour, 4 * time.Hour, start.Add(16 * time.Hour)},
		{"startup-only probing has no cadence to scale from", 24 * time.Hour, 0, start.Add(24 * time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := start.Add(tc.backoff)
			got := boundedNextProbe(next, tc.backoff, tc.staleAfter)
			if !got.Equal(tc.want) {
				t.Errorf("boundedNextProbe(start+%v, %v, staleAfter=%v) = %v, want %v",
					tc.backoff, tc.backoff, tc.staleAfter, got.UTC(), tc.want.UTC())
			}
			if got.After(next) {
				t.Errorf("bounded deadline %v is LATER than the recorded one %v: this may only bring a re-probe forward",
					got.UTC(), next.UTC())
			}
		})
	}
}

// Direction one: a free probe's backoff stays bounded. Both edges of the
// bound, on the same account, with the clock injected — so this asserts where
// the gate lapses rather than that some duration elapsed.
func TestFreeReprobeBackoffLapsesAtTheBoundNotAtTheStoredDeadline(t *testing.T) {
	staleAfter := 30 * time.Minute
	base := time.Now()

	a := spentAccount(t, "bought-a-reset", "")
	p := pool.New([]*pool.Account{a}, base)
	if a.CanOverage(p.AllowOverage()) {
		t.Fatal("precondition: this account must not be able to bill, or the bound under test never applies")
	}
	if !readsSpent(a, base) {
		t.Fatal("precondition: the stored reading must say the probe's own window is spent")
	}

	growBackoffToCeiling(p, a, staleAfter, base)
	if got, want := a.NextProbeAt(), base.Add(24*time.Hour); !got.Equal(want) {
		t.Fatalf("precondition: NextProbeAt = %v, want %v (#90's ceiling) — otherwise there is no long backoff to bound",
			got, want)
	}

	// Inside the cap: the backoff still does its job. Four probeIntervals is
	// a bound, not an abolition.
	if needsProbeAt(a, p.AllowOverage(), staleAfter, base.Add(90*time.Minute)) {
		t.Error("re-probed 90m into a 2h bound: the cap must not collapse the backoff to nothing")
	}
	// Past the cap, and still 22 hours short of the recorded deadline. This
	// is the whole issue: the reset was bought, the probe is free, and the
	// only thing between spillway and finding out was a backoff earned by
	// refusals that cost nothing to collect.
	if !needsProbeAt(a, p.AllowOverage(), staleAfter, base.Add(2*time.Hour+time.Minute)) {
		t.Error("still refusing to re-probe past the bound: a free probe cannot be worth spacing out for 24 hours")
	}
}

// Direction two: a billable probe's backoff still grows, unbounded, exactly
// as #90 built it. The control at the end is what stops this passing for the
// wrong reason — without it, the age cap alone could be producing the skip.
func TestBillableReprobeBackoffIsStillUnbounded(t *testing.T) {
	staleAfter := 30 * time.Minute
	base := time.Now()

	a := pool.NewAccount("spent-and-billable", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetAllowOverage(boolPtr(true))
	// Older than billedProbeAge (24h at this interval), so the age cap has
	// already had its say and the backoff is the only gate left.
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.1, Source: "headers",
			ResetAt: base.Add(2 * time.Hour), FetchedAt: base.Add(-30 * time.Hour)},
		{Name: "7d", Limit: 1, Used: 1.0, Source: "headers",
			ResetAt: base.Add(24 * time.Hour), FetchedAt: base.Add(-30 * time.Hour)},
	})
	p := pool.New([]*pool.Account{a}, base)
	if !wouldBill(a, p.AllowOverage(), base) {
		t.Fatal("precondition: this probe must be a purchase, or the test proves nothing")
	}

	growBackoffToCeiling(p, a, staleAfter, base)

	// Three hours in — past the free cap, nowhere near the recorded deadline.
	if needsProbeAt(a, p.AllowOverage(), staleAfter, base.Add(3*time.Hour)) {
		t.Error("bounded the backoff on an account whose probe is charged: #90's growth is correct where asking costs money")
	}

	// Control: with the backoff gone, the same account at the same moment IS
	// probed. So the skip above came from the unbounded backoff, not from the
	// money guard or the age cap quietly doing the work.
	p.ClearExhausted(a)
	if next, backoff := a.ReprobeSchedule(); !next.IsZero() || backoff != 0 {
		t.Fatalf("precondition: ClearExhausted left next=%v backoff=%v, want both zero", next, backoff)
	}
	if !needsProbeAt(a, p.AllowOverage(), staleAfter, base.Add(3*time.Hour)) {
		t.Fatal("control failed: without the backoff this account is not probed either, so the assertion above " +
			"was not testing the backoff at all")
	}
}

// The live scenario, end to end: an account spent, rejected repeatedly into
// #90's 24h backoff six hours ago, whose window then refills out of band. One
// ordinary probe sweep has to find it — within the bound, not 18 hours later
// when the recorded deadline arrives.
func TestABoughtResetIsFoundWithinTheBoundNotAtTheStoredDeadline(t *testing.T) {
	staleAfter := 30 * time.Minute
	base := time.Now()

	var hits atomic.Int32
	srv := refilledUpstream(t, &hits)

	a := spentAccount(t, "metawin", srv.URL)
	p := pool.New([]*pool.Account{a}, base)
	growBackoffToCeiling(p, a, staleAfter, base.Add(-6*time.Hour))

	if a.State() != pool.StateExhausted {
		t.Fatalf("precondition: State = %v, want StateExhausted", a.State())
	}
	if got, want := a.NextProbeAt(), base.Add(18*time.Hour); !got.Equal(want) {
		t.Fatalf("precondition: NextProbeAt = %v, want %v — the recorded deadline must still be far away", got, want)
	}
	// Before the fix this was the end of it: needsProbe saw now < NextProbeAt
	// and returned false on every sweep until the daemon was restarted.

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, staleAfter, quietLogger())

	if got := hits.Load(); got != 1 {
		t.Fatalf("probe requests = %d, want exactly 1: the bought reset went unnoticed", got)
	}
	if a.State() != pool.StateOK {
		t.Errorf("State = %v, want StateOK: the probe found the account refilled and must un-bench it", a.State())
	}
	for _, w := range a.QuotaWindows() {
		if w.Name == "7d" && w.Used/w.Limit > 0.5 {
			t.Errorf("7d still reads %.2f: the probe ran but its fresh reading was not stored", w.Used/w.Limit)
		}
	}
}

// ...and the same sweep must NOT fire while the account is inside the bound.
// The cap replaces a day of silence with two hours of it; it does not replace
// the backoff with a tight loop, which is the thing #90 exists to prevent.
func TestFreeReprobeStillHoldsInsideTheBound(t *testing.T) {
	staleAfter := 30 * time.Minute
	base := time.Now()

	srv := refusingUpstream(t)
	a := spentAccount(t, "recently-rejected", srv.URL)
	p := pool.New([]*pool.Account{a}, base)
	// Rejected half an hour ago: well inside the two-hour cap.
	growBackoffToCeiling(p, a, staleAfter, base.Add(-30*time.Minute))

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, staleAfter, quietLogger())
}

// The rule that outranks all of this (issue #34, and §6.21 "a probe must
// never be a purchase"): no account without an explicit opt-in may become
// billable through this path, and one WITH an opt-in must not be dragged into
// a charge by the bound. Same shape as the scenario above — a backoff started
// six hours ago, a reading old enough to clear the age cap — except extra
// usage is on, so the probe is a purchase and the full backoff still stands.
func TestBoundNeverProbesAnAccountThatWouldBeCharged(t *testing.T) {
	staleAfter := 30 * time.Minute
	base := time.Now()

	srv := refusingUpstream(t)
	a := pool.NewAccount("opted-in", pool.SourceYAML, "tok", "", 0, srv.URL)
	a.Type = "claude-oauth"
	a.SetAllowOverage(boolPtr(true))
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.1, Source: "headers",
			ResetAt: base.Add(2 * time.Hour), FetchedAt: base.Add(-30 * time.Hour)},
		{Name: "7d", Limit: 1, Used: 1.0, Source: "headers",
			ResetAt: base.Add(24 * time.Hour), FetchedAt: base.Add(-30 * time.Hour)},
	})
	p := pool.New([]*pool.Account{a}, base)
	growBackoffToCeiling(p, a, staleAfter, base.Add(-6*time.Hour))

	// Precondition that makes this test mean something: the bound, if it were
	// applied here, would have let this probe through hours ago. The only
	// thing holding it back is that a billable probe is not bounded.
	next, backoff := a.ReprobeSchedule()
	if bounded := boundedNextProbe(next, backoff, staleAfter); !bounded.Before(base) {
		t.Fatalf("precondition: the bound would fall at %v, not before %v — it is not what is being held back", bounded, base)
	}

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, staleAfter, quietLogger())
}
