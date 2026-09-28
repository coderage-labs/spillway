package pool

// Issue #229's coordinator follow-up, fourth round: with probeEvery and
// windowRejectionTTL both defaulting to 30 minutes, the scheduled probe
// ticker's phase is independent of when any rejection happens — so a
// "probe every tick from the moment of rejection" scheme (the previous
// round) could still lose the race to a tick landing a hair after the
// exclusion's own deadline, once every cycle. FamilyProbeDue's half-mark
// plus effectiveWindowRejectionTTLLocked's widening (2×probeEvery when
// that exceeds the raw TTL) is what guarantees a margin: the "start
// probing" mark and the "give up and re-admit" deadline bracket an
// interval at least as wide as one tick, so at least one tick must land
// inside it, however the ticker's phase lines up (a pigeonhole argument —
// see both functions' comments in family.go and pool.go).
//
// This test simulates several hours of that ticker, entirely with an
// injected clock (issues #98, #134) — no sleeping, no wall-clock
// assertions — driving windowRejectionExcludes and FamilyProbeDue
// directly, the same two functions usable() and accounts.ProbeIdle call
// in production. It plants the exact regression the coordinator named:
// reverting FamilyProbeDue's trigger from the half-mark back to the full
// effective TTL reopens the gap and this test catches it.

import (
	"testing"
	"time"
)

// A fast ticker (well under the raw TTL) needs no widening at all — the
// raw constant already gives it plenty of margin, and there is no reason
// to hold an account excluded any longer than necessary.
func TestEffectiveWindowRejectionTTLUnchangedForFastTicker(t *testing.T) {
	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, time.Now())
	p.SetProbeEvery(1 * time.Minute)

	if got := p.EffectiveWindowRejectionTTL(); got != windowRejectionTTL {
		t.Errorf("EffectiveWindowRejectionTTL = %v, want the unwidened raw %v", got, windowRejectionTTL)
	}
}

// A slower ticker widens the effective TTL to 2×probeEvery.
func TestEffectiveWindowRejectionTTLWidensForSlowTicker(t *testing.T) {
	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, time.Now())
	p.SetProbeEvery(time.Hour)

	if got, want := p.EffectiveWindowRejectionTTL(), 2*time.Hour; got != want {
		t.Errorf("EffectiveWindowRejectionTTL = %v, want %v (2×probeEvery)", got, want)
	}
}

// Required by the coordinator explicitly: probeInterval: 0 (startup-only
// probing) must NOT widen the fallback — nothing will ever revisit the
// account on a schedule, so there is no tick to guarantee a margin for.
func TestEffectiveWindowRejectionTTLUnwidenedWhenProbingDisabled(t *testing.T) {
	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, time.Now())
	p.SetProbeEvery(0)

	if got := p.EffectiveWindowRejectionTTL(); got != windowRejectionTTL {
		t.Errorf("EffectiveWindowRejectionTTL = %v, want the raw %v when probing is disabled", got, windowRejectionTTL)
	}
}

// TestScheduledProbeAlwaysMeasuresBeforeFallbackReadmits is the required
// test: probeEvery == windowRejectionTTL == 30m (the exact default shape
// that carries zero margin without this fix), a fable window rejected and
// kept SPENT by every simulated probe tick, over several hours of
// simulated time. The account must never be re-admitted (which is what
// would let a held request reach it and 429 for real).
func TestScheduledProbeAlwaysMeasuresBeforeFallbackReadmits(t *testing.T) {
	const probeEvery = 30 * time.Minute
	t0 := time.Now()

	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, t0)
	p.Apply(Settings{SwitchThreshold: 0.98})
	p.SetProbeEvery(probeEvery)

	realReset := t0.Add(9 * 24 * time.Hour) // days out — nothing here relies on it arriving
	p.markWindowRejectedAt(a, "7d-fable", realReset, t0)

	// Precondition: this is exactly the shape with no built-in margin —
	// probeEvery equals the raw windowRejectionTTL. If the effective TTL
	// were not widened, the "start probing" and "give up" marks would
	// coincide with the tick itself, not bracket it.
	if effTTL := p.EffectiveWindowRejectionTTL(); effTTL != 2*probeEvery {
		t.Fatalf("precondition broken: effective TTL = %v, want %v (2×probeEvery) — the widening "+
			"this test depends on did not engage", effTTL, 2*probeEvery)
	}

	simulatedTicks := 0
	measuredTicks := 0
	tick := t0
	end := t0.Add(6 * time.Hour)
	for tick.Before(end) {
		simulatedTicks++
		// The exact check usable() (inside SelectExcept) makes for every
		// real or held request arriving at this simulated instant. False
		// here means the account was re-admitted — a held request would
		// reach it and 429 for real, which is the bug this test guards.
		if !p.windowRejectionExcludes(a, fableModel, tick) {
			t.Fatalf("account re-admitted at simulated %v (%v into the run) — a held request would "+
				"have been sent upstream and 429'd for real, exactly the live bug this closes",
				tick, tick.Sub(t0))
		}
		// The exact check accounts.ProbeIdle makes to decide whether to
		// visit this tick. The scheduled sweep only ever measures the
		// family when this is true — simulating "the family probe
		// measuring still spent each tick" means every tick where this
		// fires, the row gets refreshed, exactly as a real 200 with a
		// still-spent 7d_oi header would leave it.
		if p.FamilyProbeDue(a, tick) {
			measuredTicks++
			a.SetQuotaWindows([]QuotaWindow{
				{Name: "7d-fable", Limit: 1, Used: 1.0, Source: "headers", ResetAt: realReset, FetchedAt: tick},
			})
		}
		tick = tick.Add(probeEvery)
	}
	// Final check at the end of the simulated run, same as inside the loop.
	if !p.windowRejectionExcludes(a, fableModel, end) {
		t.Fatalf("account re-admitted at the end of the simulated run (%v)", end)
	}

	if measuredTicks == 0 {
		t.Fatal("FamilyProbeDue never fired across the whole simulated run — the exclusion only held " +
			"because nothing tested it, which proves nothing")
	}
	t.Logf("simulated %d ticks over %v, FamilyProbeDue fired on %d of them", simulatedTicks, end.Sub(t0), measuredTicks)
}
