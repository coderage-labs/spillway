package pool

// Issue #194: windowRejected was an UNCAPPED hard exclusion that nothing
// could re-measure.
//
// Four things held at once. Selection dropped the account for the family
// (SelectExcept's usable), so no ordinary traffic re-measured it; the probe
// could not reach it either (probeModel is a fixed non-fable model and
// accounts.readsSpent skips families the probe never engages); the visible
// QuotaWindow was FORGED at Used/Limit = 1.0 and stamped "headers", so the
// dashboard showed the exclusion restating itself as though a provider had
// measured it; and `until` was trusted verbatim, so a bad epoch parse or a
// far-future org-level reset pinned the family out for as long as the header
// claimed — memory-only state, escapable only by restarting the daemon.
//
// These pin the three halves of the fix: the cap on the row, the TTL on the
// exclusion, and the source marker on the forged reading. Clocks are
// injected throughout (markWindowRejectedAt) so every assertion is an exact
// computed time — nothing here waits, and nothing asserts an elapsed
// duration (issues #98, #134).

import (
	"testing"
	"time"
)

// A year-out claim must not become a year-long synthetic row. #90 clamped
// MarkExhausted with maxExhaustedHorizon for exactly this; this path never
// got it.
func TestMarkWindowRejectedCapsAbsurdUntil(t *testing.T) {
	now := time.Now().Round(0)
	for _, tc := range []struct {
		name   string
		claim  time.Time
		capped bool
	}{
		{"a year out", now.Add(365 * 24 * time.Hour), true},
		// A 10-digit epoch read as milliseconds (or vice versa) is the
		// canonical bad parse: it lands centuries away.
		{"a bad epoch parse", time.Unix(1<<34, 0), true},
		// A genuine 7d-only rejection sits under the horizon and must be
		// left exactly alone — the cap bounds corruption, it does not clip
		// real weekly buckets.
		{"an ordinary weekly reset", now.Add(7 * 24 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := claudeAccountPrio("a", 0)
			p := New([]*Account{a}, now)
			p.markWindowRejectedAt(a, "7d-fable", tc.claim, now)

			w := windowNamed(a.QuotaWindows(), "7d-fable")
			want := tc.claim
			if tc.capped {
				want = now.Add(maxExhaustedHorizon)
			}
			if !w.ResetAt.Equal(want) {
				t.Errorf("forged 7d-fable ResetAt = %v, want %v (claim was %v, maxExhaustedHorizon is %v)",
					w.ResetAt, want, tc.claim, maxExhaustedHorizon)
			}
		})
	}
}

// The exclusion — the half with no escape — is clamped harder still: a
// rejection is believed for at most windowRejectionTTL before ordinary
// traffic is allowed to re-test it, the way issue #151 expires a stale
// overage refusal. A deadline already inside the TTL is left alone.
func TestMarkWindowRejectedClampsExclusionToTTL(t *testing.T) {
	now := time.Now().Round(0)
	for _, tc := range []struct {
		name  string
		claim time.Time
		want  time.Time
	}{
		{"a year out", now.Add(365 * 24 * time.Hour), now.Add(windowRejectionTTL)},
		{"a weekly reset", now.Add(7 * 24 * time.Hour), now.Add(windowRejectionTTL)},
		{"an hour out", now.Add(time.Hour), now.Add(windowRejectionTTL)},
		{"ten minutes out", now.Add(10 * time.Minute), now.Add(10 * time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := claudeAccountPrio("a", 0)
			p := New([]*Account{a}, now)
			p.markWindowRejectedAt(a, "7d-fable", tc.claim, now)

			got, ok := a.WindowRejectedUntil("7d-fable")
			if !ok {
				t.Fatalf("no live rejection recorded for a claim of %v", tc.claim)
			}
			if !got.Equal(tc.want) {
				t.Errorf("rejection deadline = %v, want %v (claim %v, windowRejectionTTL %v)",
					got, tc.want, tc.claim, windowRejectionTTL)
			}
		})
	}
}

// The headline. A family rejected with an absurd deadline becomes
// selectable again — re-measurable by ordinary traffic — once the TTL has
// passed, with no daemon restart. The rejection is recorded as having
// happened TTL+1m ago, so this is the real wall clock reading a stored
// deadline, not a test clock reaching into the selector.
func TestRejectedFamilyIsReMeasurableWithinTheTTLWithoutRestart(t *testing.T) {
	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, time.Now())

	rejectedAt := time.Now().Add(-windowRejectionTTL - time.Minute)
	claimed := rejectedAt.Add(365 * 24 * time.Hour)
	p.markWindowRejectedAt(a, "7d-fable", claimed, rejectedAt)

	if a.WindowRejectedFor(fableModel) {
		t.Fatalf("still excluded for fable %v after the rejection, with the claim %v: "+
			"the exclusion outlives windowRejectionTTL (%v), so nothing re-measures this family",
			windowRejectionTTL+time.Minute, claimed, windowRejectionTTL)
	}
	if got := p.SelectFor("s", fableBody(fableModel)); got == nil {
		t.Fatal("SelectFor(fable) = nil — a lapsed rejection must let a fable request reach the account, " +
			"since that request is the only thing that can re-measure the family")
	}

	// ...but it comes back DEPRIORITISED, not healthy. The forged row still
	// says spent until its own (capped) reset, which is what keeps the
	// re-test on the last-resort tier where a refusal costs nothing rather
	// than promoting the account back to tier 1 on a number nobody
	// measured.
	if !a.OverThresholdForWindow("7d-fable", 1) {
		t.Error("the forged 7d-fable row stopped reading spent when the exclusion lapsed — " +
			"the account now looks healthy for fable on a reading nothing re-measured")
	}
}

// Deprioritised, demonstrated: with a healthy alternative in the pool, a
// fable request after the lapse must still prefer the healthy account. A
// fix that cleared the forged row along with the exclusion would send fable
// straight back to the account upstream refused.
func TestLapsedRejectionStillLosesToAHealthyAccount(t *testing.T) {
	rejected := claudeAccountPrio("rejected", 0) // the BETTER priority
	clean := claudeAccountPrio("clean", 1)
	p := New([]*Account{rejected, clean}, time.Now())

	rejectedAt := time.Now().Add(-windowRejectionTTL - time.Minute)
	p.markWindowRejectedAt(rejected, "7d-fable", rejectedAt.Add(7*24*time.Hour), rejectedAt)

	got := p.SelectFor("s", fableBody(fableModel))
	if got == nil || got.Name != "clean" {
		t.Fatalf("SelectFor(fable) = %v, want %q — a lapsed rejection re-admits the account, "+
			"it does not make it the preferred one", gotName(got), "clean")
	}
}

// The forged reading must not claim to be a measurement. Source is the field
// every surface reads for provenance, and "headers" on this row said a
// provider had reported 100% when nothing had.
func TestForgedRejectionRowIsNotSourcedAsMeasured(t *testing.T) {
	now := time.Now().Round(0)
	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, now)

	// A real, measured reading first, so this cannot pass by the row simply
	// never having been stamped.
	a.SetQuotaWindows([]QuotaWindow{
		{Name: "7d-fable", Limit: 1, Used: 0.4, Source: "headers", ResetAt: now.Add(48 * time.Hour), FetchedAt: now},
	})
	if got := windowNamed(a.QuotaWindows(), "7d-fable").Source; got != "headers" {
		t.Fatalf("precondition: measured 7d-fable Source = %q, want %q", got, "headers")
	}

	p.markWindowRejectedAt(a, "7d-fable", now.Add(time.Hour), now)

	w := windowNamed(a.QuotaWindows(), "7d-fable")
	if w.Source == "headers" || w.Source == "poll" {
		t.Errorf("forged 7d-fable Source = %q — a row spillway wrote from a rejection is "+
			"indistinguishable from one a provider measured", w.Source)
	}
	if w.Source != windowSourceRejected {
		t.Errorf("forged 7d-fable Source = %q, want %q", w.Source, windowSourceRejected)
	}
	if w.Used != 1 || w.Limit != 1 {
		t.Errorf("forged 7d-fable = %v/%v, want 1/1 — #54's dashboard row must still read spent", w.Used, w.Limit)
	}

	// A later real measurement of the same window takes the row back, marker
	// and all: the marker is not sticky.
	a.SetQuotaWindows([]QuotaWindow{
		{Name: "7d-fable", Limit: 1, Used: 0.1, Source: "headers", ResetAt: now.Add(72 * time.Hour), FetchedAt: now},
	})
	if got := windowNamed(a.QuotaWindows(), "7d-fable").Source; got != "headers" {
		t.Errorf("after a real reading, 7d-fable Source = %q, want %q — the marker must not outlive the forgery", got, "headers")
	}
}

// #54, at pool level and against the clamps: a fable rejection must leave
// Sonnet serving off the same account. The claim here is a year out, so
// both clamps fire — neither may widen the rejection beyond its family.
// internal/proxy's TestFableRejectionStillLeavesSonnetServing is the
// end-to-end form (#140).
func TestClampedFableRejectionStillLeavesSonnetServing(t *testing.T) {
	now := time.Now().Round(0)
	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, now)
	p.markWindowRejectedAt(a, "7d-fable", now.Add(365*24*time.Hour), now)

	got := p.SelectFor("s", fableBody(sonnetModel))
	if got == nil || got.Name != "a" {
		t.Fatalf("SelectFor(sonnet) = %v, want %q — a fable rejection must not gate Sonnet (#54)", gotName(got), "a")
	}
	if a.State() != StateOK {
		t.Errorf("account state = %v, want StateOK — a family-scoped rejection must not exhaust the account (#54)", a.State())
	}
	if p.SelectFor("s2", fableBody(fableModel)) != nil {
		t.Error("SelectFor(fable) returned an account — the clamps must not weaken the exclusion while it is live")
	}
}
