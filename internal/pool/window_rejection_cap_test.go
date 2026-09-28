package pool

// Issue #194: windowRejected was an UNCAPPED hard exclusion that nothing
// could re-measure. Issue #229 then found #194's own escape — letting the
// exclusion itself lapse after windowRejectionTTL so ordinary traffic could
// re-test it — was spending a live held request on a 429 that was certain,
// because the real reset was still days out (live 2026-09-28).
//
// So today: the exclusion is capped (maxExhaustedHorizon, same as
// MarkExhausted) but otherwise lasts the REAL reset, never a shorter TTL;
// the visible QuotaWindow is FORGED at Used/Limit = 1.0 and stamped
// windowSourceRejected rather than "headers", so it can never be mistaken
// for a measurement; and windowRejectionTTL now bounds how long a rejection
// may go un-probed before Account.WindowRejectionNeedsProbeAt forces
// accounts.needsProbe to re-test it — never a live request. These pin the
// cap on the row, the source marker, and the exclusion now tracking the
// same capped value as the row. Clocks are injected throughout
// (markWindowRejectedAt) so every assertion is an exact computed time —
// nothing here waits, and nothing asserts an elapsed duration (issues #98,
// #134).

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

// The exclusion deadline is the SAME capped value as the forged row's
// ResetAt (issue #229) — no separate, shorter windowRejectionTTL clamp. A
// deadline already inside the horizon is left alone, same as the row.
func TestMarkWindowRejectedExclusionUsesRealCappedReset(t *testing.T) {
	now := time.Now().Round(0)
	for _, tc := range []struct {
		name  string
		claim time.Time
		want  time.Time
	}{
		{"a year out", now.Add(365 * 24 * time.Hour), now.Add(maxExhaustedHorizon)},
		{"a weekly reset", now.Add(7 * 24 * time.Hour), now.Add(7 * 24 * time.Hour)},
		{"an hour out", now.Add(time.Hour), now.Add(time.Hour)},
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
				t.Errorf("rejection deadline = %v, want %v (claim %v, maxExhaustedHorizon %v)",
					got, tc.want, tc.claim, maxExhaustedHorizon)
			}
			// Must be the identical value the forged row carries — the two
			// used to diverge (row: capped `until`; exclusion: capped THEN
			// TTL'd), which is exactly the bug: EarliestWindowReset read the
			// row's value while WindowRejectedFor read the shorter one.
			if row := windowNamed(a.QuotaWindows(), "7d-fable"); !row.ResetAt.Equal(got) {
				t.Errorf("exclusion deadline %v != forged row ResetAt %v — they must never diverge again", got, row.ResetAt)
			}
		})
	}
}

// The headline, replacing what #194 believed and what #229's first two
// fixes got only half right.
//
// WITHIN windowRejectionTTL of being recorded, a family rejected with a
// real, far-off reset stays HARD-EXCLUDED — no ordinary request re-admitted
// to test it — because a live rejection this fresh is unambiguously
// justified. WindowRejectedFor (the raw "is a rejection on file and still
// live" fact) and SelectFor (which also asks whether anything has
// measured the window recently enough — pool.windowRejectionExcludes)
// agree while the rejection is fresh; they can diverge once TTL passes
// with nothing having measured it (see window_billing_fallback_test.go),
// which is the corrected half of this fix. This test pins the UNCHANGED
// half: still within TTL, both keep saying no.
func TestRejectedFamilyStaysHardExcludedWithinTTL(t *testing.T) {
	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, time.Now())

	realReset := time.Now().Add(9 * 24 * time.Hour) // days out — the live #229 shape
	p.MarkWindowRejected(a, "7d-fable", realReset)

	if !a.WindowRejectedFor(fableModel) {
		t.Fatal("no longer excluded moments after being recorded — the rejection itself expired far too soon")
	}
	if got := p.SelectFor("s", fableBody(fableModel)); got != nil {
		t.Fatalf("SelectFor(fable) = %q, want nil — a rejection recorded moments ago "+
			"must not be reachable by live traffic yet", got.Name)
	}
	// The other half of the fix: an outstanding rejection always counts as
	// SPENT (issue #229's coordinator follow-up — the family probe triggers
	// on state, not on "has this gone un-probed long enough"), so
	// accounts.probeRejectedFamilies keeps being told to test this family
	// on every tick ProbeIdle visits the account, not just once past some
	// internal age gate.
	if !a.HasSpentOrExpiredFamilyAt(p.Threshold(), time.Now()) {
		t.Error("HasSpentOrExpiredFamilyAt = false for an account with a live rejection — nothing will " +
			"ever re-measure this family: it is excluded from selection AND invisible to the prober")
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
