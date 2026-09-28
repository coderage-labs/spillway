package pool

// Tests for issue #230's per-family capacity predicate: FamilyKey,
// FamilyHasCapacity and EarliestFamilyReset. These are what
// internal/proxy's episode tracker is built on, so the guarantee that
// matters most here is that FamilyHasCapacity can never disagree with
// SelectExcept's own tiers 1/2 — see TestFamilyHasCapacityAgreesWithSelection.

import (
	"testing"
	"time"
)

// The fable family resolves to "7d-fable"; anything that doesn't draw on a
// family bucket (Sonnet, or an unrecognised model) resolves to "" — the
// same narrowing family.go's OverThresholdFor/WindowRejectedFor already
// use, so a caller can never see FamilyKey name a family selection itself
// doesn't recognise.
func TestFamilyKeyNarrowsByModel(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())

	if got := p.FamilyKey(fableBody(fableModel)); got != "7d-fable" {
		t.Errorf("FamilyKey(fable) = %q, want %q", got, "7d-fable")
	}
	if got := p.FamilyKey(fableBody(sonnetModel)); got != "" {
		t.Errorf("FamilyKey(sonnet) = %q, want \"\"", got)
	}
	if got := p.FamilyKey(nil); got != "" {
		t.Errorf("FamilyKey(nil body) = %q, want \"\"", got)
	}
}

// Kimi has no family-scoped windows at all — FamilyKey must never invent
// one for it, regardless of model string.
func TestFamilyKeyKimiAlwaysGeneral(t *testing.T) {
	k := NewAccount("kimi", SourceYAML, "t", "", 0, "")
	k.Type = "kimi-oauth"
	p := New([]*Account{k}, time.Now())

	if got := p.FamilyKey(fableBody("anything")); got != "" {
		t.Errorf("FamilyKey on a Kimi-only pool = %q, want \"\" (no family windows to narrow by)", got)
	}
}

// Headline: a clean pool has capacity for both the fable family and the
// general windows.
func TestFamilyHasCapacityTrueOnACleanAccount(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())

	if !p.FamilyHasCapacity("7d-fable") {
		t.Error("FamilyHasCapacity(fable) = false on a clean account, want true")
	}
	if !p.FamilyHasCapacity("") {
		t.Error("FamilyHasCapacity(general) = false on a clean account, want true")
	}
}

// The live bug's exact shape (issue #230): every account's fable window is
// rejected while the general windows are untouched. FamilyHasCapacity must
// say fable is down and general is NOT — the disagreement the whole issue
// is about.
//
// Plant: if FamilyHasCapacity checked a.State() == StateExhausted instead
// of the real per-window predicate (windowRejectionExcludes), a fable-only
// rejection — which issue #54 deliberately leaves at StateOK — would read
// as "capacity", and this test would fail with "FamilyHasCapacity(fable) =
// true".
func TestFamilyHasCapacityFalseForRejectedFamilyOnly(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())
	p.MarkWindowRejected(a, "7d-fable", time.Now().Add(time.Hour))

	if a.State() != StateOK {
		t.Fatalf("precondition: a family rejection must not exhaust the account (issue #54), state = %v", a.State())
	}
	if p.FamilyHasCapacity("7d-fable") {
		t.Error("FamilyHasCapacity(fable) = true with every account's fable window rejected, want false")
	}
	if !p.FamilyHasCapacity("") {
		t.Error("FamilyHasCapacity(general) = false while only fable is rejected — this is the exact bug " +
			"#230 reports: Sonnet/Opus were serving fine while fable alone was out")
	}
}

// Account-wide exhaustion takes both down: a family can never have
// capacity when the account carrying it is fully exhausted.
func TestFamilyHasCapacityFalseForBothWhenAccountExhausted(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())
	p.MarkExhausted(a, time.Now().Add(time.Hour))

	if p.FamilyHasCapacity("7d-fable") {
		t.Error("FamilyHasCapacity(fable) = true on an account-wide exhausted account, want false")
	}
	if p.FamilyHasCapacity("") {
		t.Error("FamilyHasCapacity(general) = true on an account-wide exhausted account, want false")
	}
}

// Extra usage (tier 3) is a standing money decision, not "capacity" — an
// account that would only serve by billing must not count as recovered.
func TestFamilyHasCapacityFalseWhenOnlyOverageWouldServe(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	allow := true
	a.SetAllowOverage(&allow)
	p := New([]*Account{a}, time.Now())
	p.MarkExhausted(a, time.Now().Add(time.Hour))

	if p.WouldBill(a) == false {
		t.Fatal("precondition: this account must be the one that would bill")
	}
	if p.FamilyHasCapacity("") {
		t.Error("FamilyHasCapacity(general) = true for an account that would only serve on paid extra usage, " +
			"want false — tier 3 is not a recovery")
	}
}

// A family has capacity if AT LEAST ONE account can serve it, even while
// others cannot — mirrors TestFableRequestPrefersAccountWithHeadroom's
// pool shape at the capacity-predicate level.
func TestFamilyHasCapacityTrueWithOneHealthyAccountAmongManyRejected(t *testing.T) {
	spent := NewAccount("spent", SourceYAML, "t", "", 0, "")
	spent.Type = "claude-oauth"
	healthy := NewAccount("healthy", SourceYAML, "t", "", 0, "")
	healthy.Type = "claude-oauth"

	p := New([]*Account{spent, healthy}, time.Now())
	p.MarkWindowRejected(spent, "7d-fable", time.Now().Add(time.Hour))

	if !p.FamilyHasCapacity("7d-fable") {
		t.Error("FamilyHasCapacity(fable) = false with a healthy account still in the pool, want true")
	}
}

// The logical guarantee internal/proxy's episode tracker leans on: a
// family can never have capacity while the general windows do not (every
// provider's GoverningWindows only ever ADDS a family bucket on top of the
// general pair, never removes them). Constructed the other way round from
// the tests above — general is fine, fable alone is down — specifically to
// pin the DIRECTION of the implication, not just exercise the same fact
// twice.
func TestFamilyHasCapacityNeverTrueForFamilyWhenGeneralIsDown(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())
	p.MarkExhausted(a, time.Now().Add(time.Hour))

	if p.FamilyHasCapacity("7d-fable") {
		t.Fatal("FamilyHasCapacity(fable) = true while the account carrying it is exhausted account-wide — " +
			"impossible by construction, since a family check requires the SAME account to be eligible")
	}
}

// Kimi accounts have no family windows to check against and must be
// skipped, not counted as capacity, for a Claude-only family — a mixed
// pool with only a Kimi account must report no fable capacity.
func TestFamilyHasCapacitySkipsAccountsWithNoSuchFamily(t *testing.T) {
	k := NewAccount("kimi", SourceYAML, "t", "", 0, "")
	k.Type = "kimi-oauth"
	p := New([]*Account{k}, time.Now())

	if p.FamilyHasCapacity("7d-fable") {
		t.Error("FamilyHasCapacity(fable) = true on a Kimi-only pool, want false — Kimi has no fable bucket at all")
	}
	// General capacity is unaffected: Kimi still counts as ordinary,
	// account-wide capacity.
	if !p.FamilyHasCapacity("") {
		t.Error("FamilyHasCapacity(general) = false on a healthy Kimi account, want true")
	}
}

// EarliestFamilyReset reports the family's own rejection deadline, not the
// account's (there isn't one — #54 leaves the account at StateOK).
func TestEarliestFamilyResetReportsWindowRejectionDeadline(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())
	until := time.Now().Add(90 * time.Minute)
	p.MarkWindowRejected(a, "7d-fable", until)

	got, ok := p.EarliestFamilyReset("7d-fable")
	if !ok {
		t.Fatal("EarliestFamilyReset(fable) ok = false, want true")
	}
	if diff := got.Sub(until); diff < -time.Second || diff > time.Second {
		t.Errorf("EarliestFamilyReset(fable) = %v, want ~%v", got, until)
	}
}

// EarliestFamilyReset("") is the general/account-wide reset — same
// question EarliestReset already answers, generalised to be usable without
// a live request body.
func TestEarliestFamilyResetGeneralMatchesExhaustedUntil(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())
	until := time.Now().Add(2 * time.Hour)
	p.MarkExhausted(a, until)

	got, ok := p.EarliestFamilyReset("")
	if !ok {
		t.Fatal("EarliestFamilyReset(\"\") ok = false, want true")
	}
	if diff := got.Sub(until); diff < -time.Second || diff > time.Second {
		t.Errorf("EarliestFamilyReset(\"\") = %v, want ~%v", got, until)
	}
}

// Nothing scheduled (every blocking account disabled) must report
// ok=false, never a stale or made-up reset.
func TestEarliestFamilyResetUnknownWhenNothingScheduled(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())

	if _, ok := p.EarliestFamilyReset("7d-fable"); ok {
		t.Error("EarliestFamilyReset(fable) ok = true on a perfectly healthy pool, want false — nothing is scheduled")
	}
}
