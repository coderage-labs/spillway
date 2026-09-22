package accounts

// Issue #192: "look again now".
//
// Two gates stand between a user who knows the state changed and a spillway
// that does not: #90's re-probe backoff (bounded by #190) and #152's
// billed-probe rationing. Every test here plants the gate it is about,
// asserts the gate is REALLY shut — needsProbeAt is false at the same
// instant, with the same arguments — and only then forces the probe. Without
// that precondition a passing test proves nothing: a probe that runs because
// nothing was stopping it looks exactly like one that runs because the
// bypass works.
//
// No wall-clock durations are asserted anywhere (#98, #134): the gates are
// placed with injected clocks and read back as computed times.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

// billableSpentAccount is an account whose probe IS a purchase: extra usage
// explicitly opted in, its own governing window at 100%, and the reset it
// named still ahead. fetchedAgo places the reading's age relative to now, so
// a caller can sit inside or outside billedProbeAge without waiting.
func billableSpentAccount(t *testing.T, name, upstream string, now time.Time, fetchedAgo time.Duration) *pool.Account {
	t.Helper()
	a := pool.NewAccount(name, pool.SourceYAML, "tok", "", 0, upstream)
	a.Type = "claude-oauth"
	a.SetAllowOverage(boolPtr(true))
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.1, Source: "headers",
			ResetAt: now.Add(2 * time.Hour), FetchedAt: now.Add(-fetchedAgo)},
		{Name: "7d", Limit: 1, Used: 1.0, Source: "headers",
			ResetAt: now.Add(24 * time.Hour), FetchedAt: now.Add(-fetchedAgo)},
	})
	return a
}

// Gate one. An account rejected repeatedly into #90's 24-hour backoff, whose
// window has since refilled out of band — the live incident, verbatim, with
// #190's cap already applied so what is bypassed here is the SHORTEST form of
// the gate rather than a version of it nothing else had bounded.
func TestProbeNowRunsWhileTheReprobeBackoffStillHolds(t *testing.T) {
	staleAfter := 30 * time.Minute
	base := time.Now()

	var hits atomic.Int32
	srv := refilledUpstream(t, &hits)

	a := spentAccount(t, "bought-a-reset", srv.URL)
	p := pool.New([]*pool.Account{a}, base)
	// Rejected into #90's 24h ceiling half an hour ago. #190 caps a FREE
	// probe's backoff at four probeIntervals, so the effective deadline is
	// 1h30m out rather than 23h30m — and that bound is the gate this test is
	// about. Placing the rejections at an injected instant rather than
	// waiting keeps both deadlines computed, never elapsed (#98, #134).
	growBackoffToCeiling(p, a, staleAfter, base.Add(-30*time.Minute))

	// The gate, planted and read back as computed times.
	if got, want := a.NextProbeAt(), base.Add(23*time.Hour+30*time.Minute); !got.Equal(want) {
		t.Fatalf("precondition: NextProbeAt = %v, want %v — there is no backoff to bypass", got, want)
	}
	next, backoff := a.ReprobeSchedule()
	if got, want := boundedNextProbe(next, backoff, staleAfter), base.Add(90*time.Minute); !got.Equal(want) {
		t.Fatalf("precondition: #190's bounded deadline = %v, want %v — it must still be ahead of now", got, want)
	}
	// And so the scheduled sweep really does decline right now. Without this
	// the test passes on an account nothing was stopping.
	if needsProbeAt(a, p.AllowOverage(), staleAfter, base) {
		t.Fatal("precondition: the scheduled sweep would probe this account anyway, so forcing proves nothing")
	}

	billed, err := ProbeNow(context.Background(), p, srv.Client(), srv.URL,
		staleAfter, "bought-a-reset", false, quietLogger())
	if err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	if billed {
		t.Error("billed = true on an account with no extra-usage opt-in: this probe cannot cost anything")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("probe requests = %d, want exactly 1: the forced probe did not bypass the backoff", got)
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

// Gate two. A billable account whose reading is an hour old, so #152's age
// cap rations it for another 23 hours. Forced with the acknowledgement, it
// must go — the rationing exists because nobody was there to ask.
func TestProbeNowRunsWhileTheBilledProbeAgeCapWouldRationIt(t *testing.T) {
	staleAfter := 30 * time.Minute
	base := time.Now()

	var hits atomic.Int32
	srv := refilledUpstream(t, &hits)

	a := billableSpentAccount(t, "paid-and-spent", srv.URL, base, time.Hour)
	p := pool.New([]*pool.Account{a}, base)

	// The gate: this probe is a purchase, and the reading is far younger
	// than billedProbeAge, so the sweep declines. Both halves asserted —
	// without the second, a skip could be coming from somewhere else.
	if !wouldBill(a, p.AllowOverage(), base) {
		t.Fatal("precondition: this probe must be a purchase, or the age cap under test never applies")
	}
	if got := billedProbeAge(staleAfter); got <= time.Hour {
		t.Fatalf("precondition: billedProbeAge = %v, want more than the 1h reading age", got)
	}
	if needsProbeAt(a, p.AllowOverage(), staleAfter, base) {
		t.Fatal("precondition: the scheduled sweep would probe this account anyway, so forcing proves nothing")
	}
	// No re-probe backoff at all here, so the skip above is the age cap and
	// nothing else.
	if next, backoff := a.ReprobeSchedule(); !next.IsZero() || backoff != 0 {
		t.Fatalf("precondition: ReprobeSchedule = (%v, %v), want zero — gate one must not be what is being tested", next, backoff)
	}

	billed, err := ProbeNow(context.Background(), p, srv.Client(), srv.URL,
		staleAfter, "paid-and-spent", true, quietLogger())
	if err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	if !billed {
		t.Error("billed = false on a forced probe of a spent, opted-in account: the caller was not told it paid")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("probe requests = %d, want exactly 1: the forced probe did not bypass the age cap", got)
	}
}

// The money rule (#139's shape, applied to the probe): refused by default,
// proceeds on an explicit force. Both directions on the same account in the
// same test, so the refusal cannot be an account that could never be probed.
func TestProbeNowRefusesABillableProbeUntilItIsForced(t *testing.T) {
	staleAfter := 30 * time.Minute
	base := time.Now()

	var hits atomic.Int32
	srv := refilledUpstream(t, &hits)

	a := billableSpentAccount(t, "paid-and-spent", srv.URL, base, 30*time.Hour)
	p := pool.New([]*pool.Account{a}, base)
	if !wouldBill(a, p.AllowOverage(), base) {
		t.Fatal("precondition: this probe must be a purchase, or there is nothing to refuse")
	}

	billed, err := ProbeNow(context.Background(), p, srv.Client(), srv.URL,
		staleAfter, "paid-and-spent", false, quietLogger())
	if !errors.Is(err, pool.ErrProbeWouldBill) {
		t.Fatalf("unforced probe of a billable account: err = %v, want ErrProbeWouldBill", err)
	}
	if billed {
		t.Error("billed = true on a probe that was refused: nothing was bought")
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("probe requests = %d, want 0: the refusal must happen before anything is sent", got)
	}
	// The message has to name the account: it is relayed verbatim by the CLI
	// and rendered by the dashboard, and "would spend money" on its own does
	// not tell a four-account user which one.
	if err != nil && !containsAll(err.Error(), "paid-and-spent", "extra usage") {
		t.Errorf("refusal message %q does not name the account and the reason", err)
	}

	billed, err = ProbeNow(context.Background(), p, srv.Client(), srv.URL,
		staleAfter, "paid-and-spent", true, quietLogger())
	if err != nil {
		t.Fatalf("forced probe of a billable account: %v", err)
	}
	if !billed {
		t.Error("billed = false on a forced probe that was refused a moment ago for costing money")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("probe requests after forcing = %d, want exactly 1", got)
	}
}

// Issue #34 and §6.21, the rule that outranks the rest: nothing becomes
// billable through this path without an explicit opt-in. Two accounts, both
// with their governing window at 100% and their reset still ahead — the
// shape that WOULD bill if extra usage were permitted — neither opted in.
// Both must probe for free, unforced, and report billed = false.
func TestProbeNowNeverBillsAnAccountWithoutAnExplicitOptIn(t *testing.T) {
	staleAfter := 30 * time.Minute
	base := time.Now()

	cases := []struct {
		name         string
		allowAccount *bool
		poolAllows   bool
	}{
		// The default: nothing said anywhere.
		{"no opt-in at all", nil, false},
		// The pool-wide switch is on but this account has never had an
		// overage header confirmed, so CanOverage still says no — the
		// "user has singled nothing out" case.
		{"pool-wide default with no confirming header", nil, true},
		// Explicitly opted OUT, which must outrank the pool-wide yes.
		{"explicitly opted out", boolPtr(false), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := refilledUpstream(t, &hits)

			a := spentAccount(t, "never-invited", srv.URL)
			if tc.allowAccount != nil {
				a.SetAllowOverage(tc.allowAccount)
			}
			p := pool.New([]*pool.Account{a}, base)
			if tc.poolAllows {
				// Apply is the only way the pool-wide money switch is ever
				// set, in tests as in the daemon (issue #13 made it the one
				// synchronised writer). Nothing else in Settings matters
				// here: the probe's money rule reads CanOverage and the
				// stored windows, never the threshold.
				p.Apply(pool.Settings{AllowOverage: true})
			}
			if p.AllowOverage() != tc.poolAllows {
				t.Fatalf("precondition: pool AllowOverage = %v, want %v", p.AllowOverage(), tc.poolAllows)
			}
			// The precondition that makes this test about the opt-in and not
			// about the window: the reading says spent, so the ONLY thing
			// standing between this account and a charge is CanOverage.
			if !readsSpent(a, base) {
				t.Fatal("precondition: the stored reading must claim the probe's own window is spent")
			}

			billed, err := ProbeNow(context.Background(), p, srv.Client(), srv.URL,
				staleAfter, "never-invited", false, quietLogger())
			if err != nil {
				t.Fatalf("ProbeNow refused a free probe: %v", err)
			}
			if billed {
				t.Error("billed = true on an account nobody opted in: spillway must never spend uninvited (#34)")
			}
			if got := hits.Load(); got != 1 {
				t.Fatalf("probe requests = %d, want exactly 1 (free, so nothing should refuse it)", got)
			}
		})
	}
}

// A forced probe is not a general override. It unlocks the charge and
// nothing else — an account that cannot usefully be probed is still refused,
// with or without force, and no request is sent.
func TestProbeNowRefusesAnAccountThatCannotBeProbed(t *testing.T) {
	base := time.Now()
	for _, tc := range []struct {
		name  string
		setup func(*pool.Account)
		want  string
	}{
		// The exact phrasing matters, not just the word: there is a second
		// disabled check further down (after EnsureFresh, for a credential
		// that died during the refresh) whose message also contains
		// "disabled". Asserting the loose word let the guard under test be
		// deleted while this still passed.
		{"parked", func(a *pool.Account) { a.Park() }, `"off-duty" is parked`},
		{"disabled", func(a *pool.Account) { a.Disable() }, `"off-duty" is disabled`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := refusingUpstream(t)
			a := pool.NewAccount("off-duty", pool.SourceYAML, "tok", "", 0, srv.URL)
			a.Type = "claude-oauth"
			tc.setup(a)
			p := pool.New([]*pool.Account{a}, base)

			for _, force := range []bool{false, true} {
				_, err := ProbeNow(context.Background(), p, srv.Client(), srv.URL,
					30*time.Minute, "off-duty", force, quietLogger())
				if err == nil {
					t.Fatalf("force=%v: probing a %s account was allowed", force, tc.name)
				}
				if !containsAll(err.Error(), "off-duty", tc.want) {
					t.Errorf("force=%v: err = %q, want it to name the account and say %q", force, err, tc.want)
				}
			}
		})
	}
}

// An unknown name must never become a probe of something else, and must not
// be reported as a successful no-op.
func TestProbeNowRefusesAnUnknownAccount(t *testing.T) {
	srv := refusingUpstream(t)
	a := spentAccount(t, "real", srv.URL)
	p := pool.New([]*pool.Account{a}, time.Now())

	_, err := ProbeNow(context.Background(), p, srv.Client(), srv.URL,
		30*time.Minute, "typo", true, quietLogger())
	if err == nil {
		t.Fatal("probing an account that does not exist returned no error")
	}
	if !containsAll(err.Error(), "typo") {
		t.Errorf("err = %q, want it to name what was asked for", err)
	}
}

// A forced probe that is rejected again must still record #90's backoff: the
// button exists to look sooner, not to remove the thing that stops a tight
// loop. Without this, a user holding down "check now" would probe every click
// forever and the next scheduled sweep would too.
func TestAForcedProbeRejectedAgainStillGrowsTheBackoff(t *testing.T) {
	staleAfter := 30 * time.Minute
	base := time.Now()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.10")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "1.0")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", "1800600000")
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"quota"}}`))
	}))
	t.Cleanup(srv.Close)

	a := spentAccount(t, "still-spent", srv.URL)
	p := pool.New([]*pool.Account{a}, base)
	p.MarkExhausted(a, base.Add(time.Hour))
	if a.State() != pool.StateExhausted {
		t.Fatalf("precondition: State = %v, want StateExhausted — reprobeOutcome only runs for a benched account", a.State())
	}
	if next, _ := a.ReprobeSchedule(); !next.IsZero() {
		t.Fatalf("precondition: NextProbeAt = %v, want zero so the growth below is this probe's doing", next)
	}

	if _, err := ProbeNow(context.Background(), p, srv.Client(), srv.URL,
		staleAfter, "still-spent", false, quietLogger()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("probe requests = %d, want exactly 1", got)
	}
	next, backoff := a.ReprobeSchedule()
	if backoff != staleAfter {
		t.Errorf("probeBackoff = %v, want %v: a forced probe that was refused must still space the next one",
			backoff, staleAfter)
	}
	if next.IsZero() {
		t.Error("NextProbeAt is zero after a rejected forced probe: the backoff was not recorded at all")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
