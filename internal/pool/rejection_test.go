package pool

// Tests for issue #54's correction to #24: a window upstream has already
// confirmed rejected must EXCLUDE the account for the family it governs,
// not merely deprioritise it the way OverThresholdFor does. Mirrors
// family_test.go's style and fixtures (fableBody, fableModel, sonnetModel,
// gotName) deliberately, since this is a direct extension of that issue.

import (
	"testing"
	"time"
)

// The core regression guard this whole fix exists for: a fable-only
// rejection must not make the account ineligible for Sonnet. Priority
// trick borrowed from TestSonnetRequestUnaffectedBySpentFableBucket so a
// tier-2 tie-break can't produce a false pass — rejected must clear tier 1
// on merit.
func TestWindowRejectedLeavesSonnetUnaffected(t *testing.T) {
	rejected := NewAccount("rejected", SourceYAML, "t", "", 0, "")
	rejected.Type = "claude-oauth"
	rejected.SetPriority(0)

	clean := NewAccount("clean", SourceYAML, "t", "", 0, "")
	clean.Type = "claude-oauth"
	clean.SetPriority(1)

	p := New([]*Account{clean, rejected}, time.Now())
	p.MarkWindowRejected(rejected, "7d-fable", time.Now().Add(time.Hour))

	got := p.SelectFor("s", fableBody(sonnetModel))
	if got == nil || got.Name != "rejected" {
		t.Fatalf("SelectFor(sonnet) = %v, want %q (fable rejection doesn't gate Sonnet, and it has the better priority)",
			gotName(got), "rejected")
	}
}

// The other half: a fable request must be EXCLUDED from an account whose
// fable window is confirmed rejected — not merely deprioritised — even
// when it is the ONLY account in the pool. Contrast with
// TestFableSpentAccountStillChosenWhenOnlyOption, which asserts the
// opposite for the soft OverThresholdFor signal: the two must not collapse
// into the same behaviour.
func TestWindowRejectedExcludesFamilyEvenAsSoleAccount(t *testing.T) {
	rejected := NewAccount("rejected", SourceYAML, "t", "", 0, "")
	rejected.Type = "claude-oauth"

	p := New([]*Account{rejected}, time.Now())
	p.MarkWindowRejected(rejected, "7d-fable", time.Now().Add(time.Hour))

	got := p.SelectFor("s", fableBody(fableModel))
	if got != nil {
		t.Fatalf("SelectFor(fable) = %q, want nil: a confirmed rejection must exclude, even as the sole account", got.Name)
	}
}

// With a healthy alternative present, a fable request must prefer it over
// the rejected account (not just tolerate it as a last resort).
func TestWindowRejectedPrefersHealthyAccount(t *testing.T) {
	rejected := NewAccount("rejected", SourceYAML, "t", "", 0, "")
	rejected.Type = "claude-oauth"
	clean := NewAccount("clean", SourceYAML, "t", "", 0, "")
	clean.Type = "claude-oauth"

	p := New([]*Account{rejected, clean}, time.Now())
	p.MarkWindowRejected(rejected, "7d-fable", time.Now().Add(time.Hour))

	got := p.SelectFor("s", fableBody(fableModel))
	if got == nil || got.Name != "clean" {
		t.Fatalf("SelectFor(fable) = %v, want %q (headroom, not the rejected account)", gotName(got), "clean")
	}
}

// Once the recorded deadline passes, the account is eligible for the
// family again — this is a deadline, not a permanent ban.
func TestWindowRejectedExpiresAfterDeadline(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"

	p := New([]*Account{a}, time.Now())
	p.MarkWindowRejected(a, "7d-fable", time.Now().Add(-time.Minute))

	got := p.SelectFor("s", fableBody(fableModel))
	if got == nil || got.Name != "a" {
		t.Fatalf("SelectFor(fable) after deadline = %v, want %q (rejection expired)", gotName(got), "a")
	}
}

// Kimi has no family-scoped provider (GoverningWindows nil): WindowRejectedFor
// must always read false for it, whatever is recorded, since a Kimi
// rejection goes through pool.MarkExhausted's account-wide StateExhausted
// instead — never through this per-window path.
func TestKimiIgnoresWindowRejected(t *testing.T) {
	k := NewAccount("kimi", SourceYAML, "t", "", 0, "")
	k.Type = "kimi-oauth"

	p := New([]*Account{k}, time.Now())
	p.MarkWindowRejected(k, "7d-fable", time.Now().Add(time.Hour))

	if k.WindowRejectedFor("anything") {
		t.Error("WindowRejectedFor should always be false for Kimi: no family-scoped provider")
	}
	got := p.SelectFor("s", fableBody(fableModel))
	if got == nil || got.Name != "kimi" {
		t.Fatalf("SelectFor = %v, want %q (Kimi unaffected by the per-window path)", gotName(got), "kimi")
	}
}
