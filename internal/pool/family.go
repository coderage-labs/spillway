package pool

// Per-family quota (issue #24).
//
// Anthropic reports separate quota buckets per model family: an account can
// be fully spent for "fable" while Sonnet and Opus still have headroom.
// OverThreshold used to scan every recorded window regardless of what the
// request actually asked for, so a spent fable bucket read as the whole
// account being done — rotating Sonnet/Opus traffic away from an account
// that had plenty of room for it, and doing nothing to stop a fable request
// landing on an account whose fable bucket really was gone.
//
// The fix is the same shape as capability.go's CanServe: look at the request
// body, ask the provider what governs it, and let selection prefer rather
// than refuse.
//
// A spent reading is only evidence until its own reset (issue #135). The
// scanners here, and OverThreshold, skip a window whose ResetAt has passed —
// QuotaWindow.currentAt, applied lazily at read time exactly as
// WindowRejectedFor applies its deadline. Without that a spent 7d-fable was
// held for the life of the daemon: only a fable response re-measures it, and
// being spent is what kept fable away.

import (
	"encoding/json"
	"time"

	"github.com/coderage-labs/spillway/internal/provider"
)

// modelOf reads the top-level "model" field from a request body, or "" when
// absent, malformed, or there is no body at all (Select's callers that pass
// nil). "" is deliberately treated as "unrecognised" by
// provider.GoverningWindows implementations, never as a signal to skip
// family narrowing — an empty model must resolve to the general windows,
// the same as any other model this package cannot identify.
func modelOf(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return probe.Model
}

// OverThresholdFor reports whether the window(s) that govern model are
// at/above frac, per the account's provider (issue #24). A provider with no
// family-scoped buckets (GoverningWindows nil — Kimi today) falls back to
// OverThreshold's old behaviour of scanning every recorded window, because
// there is nothing to narrow by.
func (a *Account) OverThresholdFor(model string, frac float64) bool {
	return a.overThresholdForAt(model, frac, time.Now())
}

// overThresholdForAt is OverThresholdFor against an explicit clock, for
// tests. It takes a.mu itself (the At suffix marks the injected clock, not a
// held-lock precondition — see overThresholdAt). The Kimi fallback is taken
// before the lock, as it always was: overThresholdAt locks on its own.
func (a *Account) overThresholdForAt(model string, frac float64, now time.Time) bool {
	gw := provider.For(a.Type).GoverningWindows
	if gw == nil {
		return a.overThresholdAt(frac, now)
	}
	governing := gw(model)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, w := range a.windows {
		if !w.currentAt(now) || w.Limit <= 0 || w.Used/w.Limit < frac {
			continue
		}
		for _, name := range governing {
			if w.Name == name {
				return true
			}
		}
	}
	return false
}

// OverThresholdForWindow reports whether one specific window (by name) is
// at/above frac, regardless of which model it governs. Used to surface a
// named family's status in the dashboard/CLI (e.g. "fable") without
// widening it into the general OverThreshold bit (#24 decision 3). A window
// past its reset reads false here too (issue #135), so fableSpent clears on
// its own.
func (a *Account) OverThresholdForWindow(name string, frac float64) bool {
	return a.overThresholdForWindowAt(name, frac, time.Now())
}

// overThresholdForWindowAt is OverThresholdForWindow against an explicit
// clock, for tests. Takes a.mu itself.
func (a *Account) overThresholdForWindowAt(name string, frac float64, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, w := range a.windows {
		if w.Name == name && w.currentAt(now) && w.Limit > 0 && w.Used/w.Limit >= frac {
			return true
		}
	}
	return false
}

// WindowRejectedFor reports whether any window governing model has been
// confirmed rejected by upstream and has not yet reset (issue #54's
// correction to #24).
//
// This is deliberately a different question from OverThresholdFor: that
// one is a PREFERENCE built from proactive utilization headers ("prefer
// another account, but serve from this one if it's all there is" —
// TestFableSpentAccountStillChosenWhenOnlyOption pins that down). A window
// named here means upstream has already returned a 429 for it — not a
// maybe, a confirmed no until the recorded deadline — so this must EXCLUDE
// the account for the family that window governs, even when it is the
// only account in the pool. SelectExcept then returns nil and the request
// takes the existing hold-then-429 path, the same as when every account is
// StateExhausted.
//
// nil GoverningWindows (Kimi: no family-scoped provider) has nothing to
// check — that provider's rejections go through pool.MarkExhausted's
// account-wide StateExhausted instead, which eligible() already covers.
func (a *Account) WindowRejectedFor(model string) bool {
	gw := provider.For(a.Type).GoverningWindows
	if gw == nil {
		return false
	}
	governing := gw(model)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, name := range governing {
		if until, ok := a.windowRejected[name]; ok && until.After(now) {
			return true
		}
	}
	return false
}

// WindowRejectedUntil exposes one window's rejection deadline (for the
// admin/dashboard surface), false when none is recorded or it has already
// passed.
func (a *Account) WindowRejectedUntil(name string) (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	until, ok := a.windowRejected[name]
	if !ok || !until.After(time.Now()) {
		return time.Time{}, false
	}
	return until, true
}
