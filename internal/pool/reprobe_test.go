package pool

// Tests for issue #90: an exhausted account must not be sentenced to an
// arbitrarily far-future deadline, and the background re-probe machinery
// (internal/accounts/probe.go) must be able to both clear a bench that
// turns out to have been wrong and extend one that is confirmed still in
// force — without hammering a genuinely spent account every tick.

import (
	"testing"
	"time"
)

// A single reset value this far out is almost certainly a data problem
// (corrupt header, bad epoch parse, a stale org-level cap) rather than a
// legitimate window — spillway's longest real window is the weekly bucket
// (7d). MarkExhausted must clip it rather than trust it outright.
func TestMarkExhaustedCapsFarFutureUntil(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	p := New([]*Account{a}, time.Now())

	wildlyFar := time.Now().Add(365 * 24 * time.Hour)
	p.MarkExhausted(a, wildlyFar)

	got := a.ExhaustedUntil()
	if got.After(time.Now().Add(maxExhaustedHorizon + time.Minute)) {
		t.Fatalf("ExhaustedUntil = %v, want capped at ~%v out, not the claimed %v",
			got, maxExhaustedHorizon, wildlyFar)
	}
}

// A legitimate exhaustion — a real weekly reset, comfortably inside the
// cap — must pass through untouched.
func TestMarkExhaustedLeavesOrdinaryUntilAlone(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	p := New([]*Account{a}, time.Now())

	weekOut := time.Now().Add(6 * 24 * time.Hour)
	p.MarkExhausted(a, weekOut)

	got := a.ExhaustedUntil()
	if !got.Equal(weekOut) {
		t.Fatalf("ExhaustedUntil = %v, want unchanged %v (well inside the cap)", got, weekOut)
	}
}

// A re-probe that succeeds clears the exhaustion immediately — this is the
// self-heal issue #90 asks for: a daemon restart proved a bench could be
// wrong days before its recorded deadline, and a re-probe should reach the
// same conclusion without needing a restart.
func TestClearExhaustedRevivesAccount(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	p := New([]*Account{a}, time.Now())
	p.MarkExhausted(a, time.Now().Add(3*24*time.Hour))

	if a.State() != StateExhausted {
		t.Fatalf("precondition: account must start exhausted")
	}

	p.ClearExhausted(a)

	if a.State() != StateOK {
		t.Fatalf("State() = %v, want StateOK after ClearExhausted", a.State())
	}
	if !a.ExhaustedUntil().IsZero() {
		t.Errorf("ExhaustedUntil = %v, want zero after clearing", a.ExhaustedUntil())
	}
	if !a.NextProbeAt().IsZero() {
		t.Errorf("NextProbeAt = %v, want zero after clearing", a.NextProbeAt())
	}
}

// ClearExhausted must never revive an account that isn't exhausted in the
// first place — in particular it must not touch StateDisabled (a dead
// credential) or accidentally "clear" an account that is already StateOK.
func TestClearExhaustedNoopWhenNotExhausted(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Disable()
	p := New([]*Account{a}, time.Now())

	p.ClearExhausted(a)

	if a.State() != StateDisabled {
		t.Fatalf("State() = %v, want StateDisabled unchanged", a.State())
	}
}

// The core "no tight loop" guarantee: a re-probe that is rejected again
// must grow the backoff (and thus NextProbeAt), never reset it back down to
// baseInterval, so a genuinely spent account is checked less and less often
// instead of every tick forever.
func TestMarkReprobeRejectedGrowsBackoffWithoutResettingToZero(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	p := New([]*Account{a}, time.Now())
	base := 30 * time.Minute
	const slack = 2 * time.Second // wall-clock tolerance for the test itself running

	before1 := time.Now()
	p.MarkReprobeRejected(a, time.Now().Add(time.Hour), base)
	gap1 := a.NextProbeAt().Sub(before1)
	if gap1 < base-slack {
		t.Fatalf("first rejected re-probe: gap = %v, want >= baseInterval %v", gap1, base)
	}

	before2 := time.Now()
	p.MarkReprobeRejected(a, time.Now().Add(time.Hour), base)
	gap2 := a.NextProbeAt().Sub(before2)
	if gap2 < 2*base-slack {
		t.Fatalf("second rejected re-probe: gap = %v, want >= 2*baseInterval %v "+
			"(backoff must grow, not reset to baseInterval every time)", gap2, 2*base)
	}

	before3 := time.Now()
	p.MarkReprobeRejected(a, time.Now().Add(time.Hour), base)
	gap3 := a.NextProbeAt().Sub(before3)
	if gap3 < 4*base-slack {
		t.Fatalf("third rejected re-probe: gap = %v, want >= 4*baseInterval %v", gap3, 4*base)
	}

	if a.State() != StateExhausted {
		t.Errorf("State() = %v, want StateExhausted after a rejected re-probe", a.State())
	}
}

// The backoff must not grow without bound: maxProbeBackoff caps it so a
// permanently-spent account is still checked eventually.
func TestMarkReprobeRejectedBackoffIsCapped(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	p := New([]*Account{a}, time.Now())
	base := 30 * time.Minute

	for i := 0; i < 20; i++ {
		p.MarkReprobeRejected(a, time.Now().Add(time.Hour), base)
	}

	next := a.NextProbeAt()
	if next.After(time.Now().Add(maxProbeBackoff + time.Minute)) {
		t.Fatalf("NextProbeAt = %v, want capped at ~now+%v", next, maxProbeBackoff)
	}
}
