package pool

// Issue #229's third fix: a probe (or the dashboard's "check now", #192)
// that measures a rejected window's real headroom must clear the exclusion
// and wake anything parked on it — nothing did either, before this, so a
// manual free reset (#228) or an out-of-band provider reset (#135) sat
// invisible behind windowRejected until its original (now much longer,
// #229's first fix) deadline arrived on its own.
//
// These pin ClearRecoveredWindowRejections directly, at the pool level:
// accounts/probe_now_test.go covers the same fix exercised end-to-end
// through ProbeNow, the path the dashboard's "check now" button actually
// calls.

import (
	"testing"
	"time"
)

// The headline: a fresh measurement that overwrites the forged row with a
// reading under its limit clears the rejection and signals capacity, same
// as ClearExhausted does for account-wide exhaustion (capacity_test.go's
// TestClearExhaustedSignalsCapacity).
func TestClearRecoveredWindowRejectionsClearsOnHeadroomAndSignals(t *testing.T) {
	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, time.Now())
	p.MarkWindowRejected(a, "7d-fable", time.Now().Add(time.Hour))

	if !a.WindowRejectedFor(fableModel) {
		t.Fatal("precondition: the rejection must be live, or clearing it proves nothing")
	}

	// A real measurement lands — as though this account's probe response
	// had, unusually, carried a fresh 7d-fable reading — showing headroom.
	a.SetQuotaWindows([]QuotaWindow{
		{Name: "7d-fable", Limit: 1, Used: 0.2, Source: "headers",
			ResetAt: time.Now().Add(48 * time.Hour), FetchedAt: time.Now()},
	})

	wake := p.CapacitySignal()
	select {
	case <-wake.Ch():
		t.Fatal("capacity signal fired before ClearRecoveredWindowRejections was ever called")
	default:
	}

	p.ClearRecoveredWindowRejections(a)

	if a.WindowRejectedFor(fableModel) {
		t.Error("still excluded for fable after a fresh under-limit reading — the recovery was not observed")
	}
	select {
	case <-wake.Ch():
	case <-time.After(time.Second):
		t.Fatal("ClearRecoveredWindowRejections did not signal capacity — a parked hold would sleep out " +
			"the full (now much longer, issue #229) exclusion instead of waking promptly")
	}
}

// The guard: a fresh measurement that STILL reads spent must change
// nothing. Judged from the reading, not from whatever HTTP status carried
// it — this pins the "not from status < 400" half of the fix directly,
// without needing an HTTP round trip.
func TestClearRecoveredWindowRejectionsLeavesStillSpentRejectionAlone(t *testing.T) {
	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, time.Now())
	p.MarkWindowRejected(a, "7d-fable", time.Now().Add(time.Hour))

	// A real measurement lands, but it still reads spent — e.g. the probe's
	// own model succeeded (200) while the account's fable bucket, read
	// separately, has not actually refilled.
	a.SetQuotaWindows([]QuotaWindow{
		{Name: "7d-fable", Limit: 1, Used: 1.0, Source: "headers",
			ResetAt: time.Now().Add(48 * time.Hour), FetchedAt: time.Now()},
	})

	wake := p.CapacitySignal()
	p.ClearRecoveredWindowRejections(a)

	if !a.WindowRejectedFor(fableModel) {
		t.Error("rejection cleared on a still-spent reading — a probe that reports no headroom must not " +
			"re-admit the family")
	}
	select {
	case <-wake.Ch():
		t.Error("capacity signalled for a rejection that was never actually cleared")
	default:
	}
}

// The common case: no fresh measurement at all (the forged row is
// untouched, still Source == windowSourceRejected). Must be a complete
// no-op — this is what almost every real probe looks like, since
// probeModel never governs the fable family.
func TestClearRecoveredWindowRejectionsNoOpWithoutAFreshMeasurement(t *testing.T) {
	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, time.Now())
	p.MarkWindowRejected(a, "7d-fable", time.Now().Add(time.Hour))

	wake := p.CapacitySignal()
	p.ClearRecoveredWindowRejections(a)

	if !a.WindowRejectedFor(fableModel) {
		t.Error("rejection cleared with no new measurement at all")
	}
	select {
	case <-wake.Ch():
		t.Error("capacity signalled with nothing having changed")
	default:
	}
}

// An already-expired rejection is tidied (deleted) but never reported as a
// "recovery" — it wasn't excluding anything, so there's nothing to signal.
func TestClearRecoveredWindowRejectionsTidiesExpiredEntryWithoutSignalling(t *testing.T) {
	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, time.Now())
	p.markWindowRejectedAt(a, "7d-fable", time.Now().Add(-time.Minute), time.Now().Add(-time.Hour))

	wake := p.CapacitySignal()
	p.ClearRecoveredWindowRejections(a)

	if _, ok := a.WindowRejectedUntil("7d-fable"); ok {
		t.Error("an already-expired rejection is still reported as live")
	}
	select {
	case <-wake.Ch():
		t.Error("capacity signalled for an entry that was already expired, not recovered")
	default:
	}
}
