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
// The deadline it applies is MarkWindowRejected's clamped one — capped by
// maxExhaustedHorizon, the same as MarkExhausted, but no longer bounded to
// windowRejectionTTL (issue #229's correction to #194): #194 believed the
// exclusion for at most that TTL and let selection fall back to mere
// deprioritisation past it, which sounded free but wasn't — tier 2 is real
// traffic, and it 429'd on a genuinely-rejected family for certain, every
// TTL, spending a live held request each time (2026-09-28). The exclusion
// now lasts as long as the rejection genuinely does; only
// ClearRecoveredWindowRejections (a probe's own fresh reading) or the real
// reset arriving can end it early.
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

// earliestWindowRejectionFor reports the soonest deadline among the windows
// governing model that upstream has confirmed rejected and that has not yet
// passed (issue #140). It is WindowRejectedFor's question — "is this
// account excluded for this model right now?" — asked for *when* rather
// than *whether*, and it applies the identical deadline test
// (until.After(now)) so the two can never disagree about which rejections
// are live.
//
// A deadline already in the past is skipped rather than returned: an
// expired rejection no longer excludes anything, so reporting it would hand
// the hold a wake time it has already passed, and the hold treats that as
// "re-select now" — which, if the account is still unusable for some other
// reason, is a busy loop, not a wait.
//
// nil GoverningWindows (Kimi) has nothing to check, exactly as in
// WindowRejectedFor: that provider's refusals go through account-wide
// StateExhausted, which EarliestReset already covers.
func (a *Account) earliestWindowRejectionFor(model string, now time.Time) (time.Time, bool) {
	gw := provider.For(a.Type).GoverningWindows
	if gw == nil {
		return time.Time{}, false
	}
	governing := gw(model)
	a.mu.Lock()
	defer a.mu.Unlock()
	var earliest time.Time
	ok := false
	for _, name := range governing {
		until, has := a.windowRejected[name]
		if !has || !until.After(now) {
			continue
		}
		if !ok || until.Before(earliest) {
			earliest, ok = until, true
		}
	}
	return earliest, ok
}

// windowRejectionAgeAt reports how long it has been since window name was
// last measured or re-marked at all — whichever happened most recently —
// for windowRejectionExcludes' bounded fallback below: a rejection reverts
// to #194's original, softer exclusion shape once this age passes
// windowRejectionTTL, rather than staying hard-excluded for the full real
// reset with nothing ever able to clear it. This is deliberately blind to
// WHY the age grew past the TTL — a billable account whose probe was never
// sent, a probe that 400'd on an unrecognised model, a network error, a
// 200 with no header for this window — every one of those leaves the row
// untouched, and every one of them must eventually re-admit the account
// rather than exclude it forever (issue #229's coordinator follow-up).
// ok=false when nothing has ever been recorded for that name.
//
// Note this is a DIFFERENT question from accounts.probeRejectedFamilies'
// own trigger (Account.SpentOrExpiredFamiliesAt), which is state-based
// (spent or expired) rather than age-based, and runs on every probe tick
// regardless of how long a rejection has stood — the two mechanisms serve
// different callers with different needs, and must not be confused for
// one another.
//
// Deliberately not restricted to the forged row specifically (Source ==
// windowSourceRejected): a family probe that measures real, fresh headers
// showing the window STILL spent overwrites the row with Source "headers"
// but must not go quiet forever after — it still needs to be re-tried
// again after another windowRejectionTTL, the same as before that
// measurement arrived.
func (a *Account) windowRejectionAgeAt(name string, now time.Time) (age time.Duration, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.windows {
		if a.windows[i].Name == name {
			return now.Sub(a.windows[i].FetchedAt), true
		}
	}
	return 0, false
}

// familyWindowNames lists this account's provider's FAMILY-scoped window
// names — every window name this account has ever recorded a reading for
// that is NOT one of GoverningWindows("")'s general, account-wide windows
// ("5h"/"7d" for Claude). "7d-fable" is the only one today; derived from
// the account's own recorded window names rather than hard-coded, so a
// future family needs no edit here, only in the provider package.
//
// nil GoverningWindows (Kimi: no family-scoped provider) has nothing to
// narrow by, so this is empty — that provider's quota is all
// account-wide, exactly as WindowRejectedFor already treats it.
func (a *Account) familyWindowNames() []string {
	gw := provider.For(a.Type).GoverningWindows
	if gw == nil {
		return nil
	}
	general := make(map[string]bool)
	for _, n := range gw("") {
		general[n] = true
	}
	var out []string
	for _, w := range a.QuotaWindows() {
		if !general[w.Name] {
			out = append(out, w.Name)
		}
	}
	return out
}

// familySpentOrExpiredAt reports whether family-scoped window name is
// currently SPENT or EXPIRED, per the coordinator's follow-up to issue
// #229 (a production-live review of the first family-probe design, which
// keyed the probe on "a windowRejected entry exists" — too narrow, since a
// family can go stale without ever having been formally 429-rejected):
//
//   - SPENT: an outstanding window rejection for it (WindowRejectedUntil),
//     OR its latest reading is at/above frac (OverThresholdForWindow) —
//     the same "spent" the dashboard's FableSpent already reports.
//   - EXPIRED: its recorded reset has passed with no fresh reading since
//     (QuotaWindow.Expired, issue #135's currentAt) — spillway genuinely
//     does not know whether it refilled.
//
// Neither (HEALTHY) must return false: probing a healthy fable window with
// a fable-governed model would spend real fable quota to learn a fact
// already on file, which is the "a probe must never be a purchase in
// spirit" mistake even where it can't literally bill.
func (a *Account) familySpentOrExpiredAt(name string, frac float64, now time.Time) bool {
	if _, ok := a.WindowRejectedUntil(name); ok {
		return true
	}
	if a.overThresholdForWindowAt(name, frac, now) {
		return true
	}
	for _, w := range a.QuotaWindows() {
		if w.Name == name {
			return w.Expired
		}
	}
	return false
}

// SpentOrExpiredFamiliesAt lists this account's family-scoped windows
// currently spent or expired (issue #229's coordinator follow-up) — the
// ones accounts.probeRejectedFamilies must probe (subject to its own
// billing guard) and the ones that force accounts.ProbeIdle/needsProbe to
// visit this account at all, regardless of how fresh its OTHER windows
// look through ordinary traffic (a busy account still serving Sonnet/Opus
// would otherwise never be asked to check a family nothing about its
// general staleness reflects).
func (a *Account) SpentOrExpiredFamiliesAt(frac float64, now time.Time) []string {
	var out []string
	for _, name := range a.familyWindowNames() {
		if a.familySpentOrExpiredAt(name, frac, now) {
			out = append(out, name)
		}
	}
	return out
}

// HasSpentOrExpiredFamilyAt is SpentOrExpiredFamiliesAt narrowed to "is
// this list non-empty", for accounts.ProbeIdle's forcing check.
func (a *Account) HasSpentOrExpiredFamilyAt(frac float64, now time.Time) bool {
	return len(a.SpentOrExpiredFamiliesAt(frac, now)) > 0
}

// windowRejectionExcludes is usable()'s per-window exclusion test — the
// same question WindowRejectedFor answers, MINUS one bounded fallback a
// production-live review of issue #229 required (and a second review,
// after that first fallback, found half-broken — see below).
//
// accounts.probeRejectedFamilies is what is meant to eventually lift a
// rejection for good, by measuring the family with a model that actually
// governs it. But that measurement can fail to arrive for any number of
// reasons: the account has extra usage enabled and the window still reads
// spent, so sending the probe would risk billing it (§6.21) and
// probeRejectedFamilies correctly refuses to send it at all; the model it
// chose 400s or 404s as unrecognised; the request errors on the network;
// or the response simply carries no header for this window. Every one of
// those is "could not measure", and NONE of them may extend a hard
// exclusion indefinitely — an account whose family probe never succeeds
// would otherwise stay excluded for the FULL real reset (days,
// potentially), the exact stale-negative shape issue #194 was written to
// escape, just laundered through a different cause each time.
//
// So the rule is not "exclude unless the probe is known to be unsafe" (an
// earlier version of this function, which asked a StaticQuestion —
// familyProbeUnsafe, since removed — about billing and model availability
// only, and so kept excluding right through a probe that was SENT but
// silently failed to measure anything). It is simpler and catches every
// cause at once: has ANYTHING refreshed this window's own row —
// windowRejectionAgeAt, which moves on every write to it, forged or
// measured, spent or healthy — within the EFFECTIVE TTL
// (effectiveWindowRejectionTTLLocked, widened past the raw constant when
// the scheduled probe ticker is slower — a third review found the raw
// 30-minute constant left zero margin against a 30-minute default ticker
// phase, so a probe meant to land before this deadline could lose that
// race by the width of one tick)? If yes, the exclusion holds; either the
// rejection is fresh, or a probe genuinely re-confirmed the family is
// still spent (issue #229's coordinator follow-up, required test (c)). If
// no — nothing has touched this row in the full effective TTL, for
// whatever reason — this reverts to #194's original,
// softer shape: the exclusion itself lapses, leaving only the forged row's
// deprioritisation (OverThresholdFor) to protect the family, so a real
// request can go and find out. That is the SAME bounded risk #151/#194
// already accepted for an account whose owner explicitly opted into
// paying — now extended, deliberately, to every other way a measurement
// can fail to arrive.
//
// Takes now explicitly and assumes p.mu is already held (its only caller,
// usable() inside SelectExcept, both holds the lock and has a clock to
// hand down) — issues #98/#134's injected-clock convention, and the only
// way effectiveWindowRejectionTTLLocked's own mu precondition can be
// satisfied here.
func (p *Pool) windowRejectionExcludes(a *Account, model string, now time.Time) bool {
	gw := provider.For(a.Type).GoverningWindows
	if gw == nil {
		return false
	}
	ttl := p.effectiveWindowRejectionTTLLocked()
	for _, name := range gw(model) {
		// The specific window's own rejection state — not WindowRejectedFor,
		// which takes a MODEL and re-derives its own governing set; passing
		// a window NAME through that would wrongly pull in its siblings
		// (claudeGoverningWindows("7d-fable") also returns "5h" and "7d").
		if _, ok := a.WindowRejectedUntil(name); !ok {
			continue
		}
		if age, ok := a.windowRejectionAgeAt(name, now); ok && age >= ttl {
			continue // bounded fallback: nothing has measured this window in the effective TTL, for any reason
		}
		return true
	}
	return false
}

// FamilyProbeDue reports whether any of a's family-scoped windows is
// spent or expired (Account.familySpentOrExpiredAt) AND has gone at least
// HALF the effective windowRejectionTTL without a fresh measurement
// (issue #229's coordinator follow-up on timing). accounts.ProbeIdle ORs
// this into its own needsProbe check to decide whether to visit an
// account this tick, regardless of how fresh its other windows look.
//
// Half, not the full effective TTL, is what leaves effectiveWindowRejectionTTLLocked's
// margin actually usable: see that function's comment for the pigeonhole
// argument — triggering any later than the halfway point can lose the
// race to the ticker's own phase, which is exactly the bug this closes.
//
// A manual "check now" (accounts.ProbeNow) does NOT go through this check
// at all — it calls probeOne directly, bypassing needsProbe and every
// scheduling heuristic on purpose (see probe_now.go), so a user-initiated
// probe is never held back by this gate either.
func (p *Pool) FamilyProbeDue(a *Account, now time.Time) bool {
	threshold := p.Threshold()
	half := p.EffectiveWindowRejectionTTL() / 2
	for _, name := range a.familyWindowNames() {
		if !a.familySpentOrExpiredAt(name, threshold, now) {
			continue
		}
		age, ok := a.windowRejectionAgeAt(name, now)
		if !ok || age >= half {
			return true
		}
	}
	return false
}
