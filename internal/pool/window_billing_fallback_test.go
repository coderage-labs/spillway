package pool

// Issue #229's coordinator follow-up, twice refined.
//
// First cut: a family probe must never be sent if it could bill (overage
// enabled for that account and the window still reads spent).
// accounts.probeRejectedFamilies enforces that on the sending side; these
// tests pin the OTHER half, on the selection side — windowRejectionExcludes'
// bounded fallback, which is what keeps such an account from being excluded
// forever once nothing can ever measure it for free.
//
// Second cut, from a production-live review of the first: the fallback was
// gated on "the probe is judged UNSAFE" (billing, or no model available),
// which quietly assumed a SAFE probe always succeeds. It doesn't — a
// probe using a wrong-but-present model id 400s every time, and the first
// version's gate never noticed, so a non-billable account with a bad model
// id was stranded exactly as badly as a billable one, forever, which is
// worse than what this fix was meant to close. windowRejectionExcludes no
// longer asks "is the probe unsafe"; it asks "has anything refreshed this
// window's row within windowRejectionTTL" — true for a fresh rejection AND
// for a probe that genuinely re-confirmed the window is still spent, false
// for every way a measurement can fail to arrive, billable or not.

import (
	"testing"
	"time"
)

func billableClaudeAccount(name string) *Account {
	a := claudeAccountPrio(name, 0)
	yes := true
	a.SetAllowOverage(&yes)
	return a
}

// The headline: CanOverage on, rejection recorded well past windowRejectionTTL
// ago, real reset days out, and nothing has refreshed the row since (the
// family probe was correctly never sent, because it would have billed).
// Selection must fall back to #194's original, softer shape — admitting it
// via the paid tier — rather than excluding it for the full real reset with
// no way back.
func TestBillableAccountFallsBackPastTTLRatherThanStrandedForever(t *testing.T) {
	a := billableClaudeAccount("a")
	p := New([]*Account{a}, time.Now())

	rejectedAt := time.Now().Add(-windowRejectionTTL - time.Minute)
	realReset := time.Now().Add(9 * 24 * time.Hour)
	p.markWindowRejectedAt(a, "7d-fable", realReset, rejectedAt)

	if !a.CanOverage(p.AllowOverage()) {
		t.Fatal("precondition broken: this account must be billable, or the fallback under test never applies")
	}

	got := p.SelectFor("s", fableBody(fableModel))
	if got == nil {
		t.Fatal("SelectFor(fable) = nil — a billable account whose row nothing has refreshed in a full " +
			"TTL is excluded with no way back at all: exactly the stale-negative shape issue #194 was " +
			"written to prevent, reintroduced through a billing account instead of a TTL")
	}
	if got.Name != "a" {
		t.Fatalf("SelectFor(fable) = %q, want %q", got.Name, "a")
	}
}

// Contrast: the SAME billable account, but the rejection was recorded only
// moments ago — still within windowRejectionTTL. The fallback must not
// apply yet; a billable account gets exactly the same short grace period a
// free one does before anything softens.
func TestBillableAccountStillExcludedWithinTTL(t *testing.T) {
	a := billableClaudeAccount("a")
	p := New([]*Account{a}, time.Now())
	p.MarkWindowRejected(a, "7d-fable", time.Now().Add(9*24*time.Hour))

	if got := p.SelectFor("s", fableBody(fableModel)); got != nil {
		t.Fatalf("SelectFor(fable) = %q, want nil — a rejection recorded moments ago must still exclude, "+
			"billable or not", got.Name)
	}
}

// Required test (b), at the pool level: a NON-billable account is not
// exempt from the fallback just because its family probe is free to send
// — free does not mean it succeeds. If nothing has actually refreshed the
// row (the probe never ran, errored, or 400'd on an unrecognised model —
// accounts/probe_family_measurement_test.go exercises the 400 case
// end-to-end), this account falls back exactly like a billable one. This
// is what closes the stranding a wrong-but-present family model id
// caused: the probe was free, plausibly attempted, and still never
// measured anything.
func TestNonBillableAccountAlsoFallsBackWhenNeverMeasured(t *testing.T) {
	a := claudeAccountPrio("a", 0) // CanOverage false: no SetAllowOverage
	p := New([]*Account{a}, time.Now())

	rejectedAt := time.Now().Add(-windowRejectionTTL - time.Minute)
	p.markWindowRejectedAt(a, "7d-fable", time.Now().Add(9*24*time.Hour), rejectedAt)

	if a.CanOverage(p.AllowOverage()) {
		t.Fatal("precondition broken: this account must NOT be billable, or this doesn't isolate the case")
	}
	got := p.SelectFor("s", fableBody(fableModel))
	if got == nil {
		t.Fatal("SelectFor(fable) = nil — a non-billable account whose family probe never actually " +
			"measured anything is stranded exactly as a billable one would be: 'free to send' does not " +
			"mean 'succeeds'")
	}
}

// Required test (c): a family probe that DID measure the window, and
// found it still spent, refreshes the row exactly like any other
// measurement (RecordQuota's ordinary write) — so windowRejectionAgeAt
// reads fresh again and the exclusion correctly holds, on a non-billable
// account, well past when the ORIGINAL rejection was first recorded. This
// is the one case exclusion is actually justified, and the fallback must
// not fire here.
func TestFreshStillSpentMeasurementKeepsAccountExcluded(t *testing.T) {
	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, time.Now())

	rejectedAt := time.Now().Add(-windowRejectionTTL - time.Minute)
	p.markWindowRejectedAt(a, "7d-fable", time.Now().Add(9*24*time.Hour), rejectedAt)

	// A family probe ran moments ago and confirmed the window is STILL
	// spent — a real measurement, not the original forged row, refreshing
	// FetchedAt.
	a.SetQuotaWindows([]QuotaWindow{
		{Name: "7d-fable", Limit: 1, Used: 1.0, Source: "headers",
			ResetAt: time.Now().Add(9 * 24 * time.Hour), FetchedAt: time.Now()},
	})

	if got := p.SelectFor("s", fableBody(fableModel)); got != nil {
		t.Fatalf("SelectFor(fable) = %q, want nil — a fresh measurement confirming the window is "+
			"still spent must keep the account excluded: this is the case exclusion is justified", got.Name)
	}
}
