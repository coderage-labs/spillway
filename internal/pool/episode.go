package pool

// Per-family capacity, for exhaustion/recovery notification episodes
// (issue #230).
//
// #230's live bug: a notification said "spillway: pool exhausted" while
// only the fable family was actually out — every account's 7d-fable window
// was spent while Sonnet/Opus kept serving normally elsewhere. The fix
// needs a notion of "does THIS family currently have capacity" that can
// never disagree with what SelectExcept would actually do, so the
// functions below are deliberately built from the exact same primitives
// SelectExcept's tiers 1/2 use (Account.eligible, windowRejectionExcludes,
// Pool.wouldBill) rather than a second, hand-rolled definition of
// "exhausted" that could drift from routing.
//
// family is a window name as FamilyKey below derives it ("7d-fable" today
// — the only family window any provider currently reports) or "" for the
// general, account-wide windows every request draws on regardless of
// family. Note that FamilyHasCapacity(family) true, for any non-general
// family, implies FamilyHasCapacity("") true: the account satisfying the
// family check is eligible and not window-rejected for its OWN governing
// set, which is a superset of the general windows (GoverningWindows only
// ever appends a family's extra bucket to the general pair, never removes
// them — see claudeGoverningWindows), so it clears the general check too.
// There is no reachable state where a family has capacity but the general
// windows do not; internal/proxy's episode tracker relies on exactly this
// to decide when a family episode is subsumed by an "all models" one.

import (
	"sort"
	"strings"
	"time"

	"github.com/coderage-labs/spillway/internal/provider"
)

// FamilyKey identifies which notification episode (issue #230) a request
// belongs to: the quota window name(s) that govern it beyond the general
// set every request draws on. "" means "no extra family" — this request
// only draws on the general windows (any Kimi request, or a Claude request
// for a model with no family bucket of its own).
//
// Derived from GoverningWindows exactly as selection derives eligibility
// (family.go's OverThresholdFor/WindowRejectedFor), so it can never name a
// family selection itself doesn't recognise. Scans every account's
// provider rather than assuming one kind, so a pool mixing providers still
// resolves correctly: a provider whose GoverningWindows returns only the
// general set for this model (nil GoverningWindows, or a model it simply
// doesn't recognise as belonging to any family) contributes nothing here.
func (p *Pool) FamilyKey(body []byte) string {
	model := modelOf(body)
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range p.accounts {
		gw := provider.For(a.Type).GoverningWindows
		if gw == nil {
			continue
		}
		general := map[string]bool{}
		for _, n := range gw("") {
			general[n] = true
		}
		for _, n := range gw(model) {
			if !general[n] {
				seen[n] = true
			}
		}
	}
	if len(seen) == 0 {
		return ""
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, "+")
}

// familyRepresentativeModel returns a model string that, for a's own
// provider, GoverningWindows would recognise as governed by family (a
// window name, e.g. "7d-fable") — the same model shape
// accounts.probeRejectedFamilies uses to actually probe it (issue #229's
// FamilyProbeModel), reused here purely to ask windowRejectionExcludes the
// right question. Nothing is sent; this never performs a probe.
//
// family == "" is the general windows, which every provider recognises
// for the zero-value model string (modelOf's own "unrecognised" answer),
// so that case needs no provider lookup at all.
//
// ok=false means a's provider has nothing to do with family at all (nil
// GoverningWindows — Kimi — or an unrecognised window name), so the caller
// must skip this account for that family rather than guess.
func familyRepresentativeModel(a *Account, family string) (string, bool) {
	if family == "" {
		return "", true
	}
	spec := provider.For(a.Type)
	if spec.FamilyProbeModel == nil {
		return "", false
	}
	return spec.FamilyProbeModel(family, a.EffectiveModelMap())
}

// FamilyHasCapacity reports whether at least one account could serve
// family right now via tier 1 or 2 of SelectExcept — eligible, not
// window-rejected for it, and not about to bill (issue #230). This is
// deliberately the exact predicate selection's first two tiers use, not a
// second definition that could disagree with routing: the recovery
// notification must never fire while spillway itself still refuses the
// family, and the "held" notification must never fire while spillway is
// already routing it.
//
// Tier 3 (paid extra usage) deliberately does not count as "capacity" —
// that tier is a standing money decision, not a recovery, and reporting
// "back" the moment an account starts billing would be the overage
// notification's job (EventOverageCap), not this one's.
//
// family == "" asks about the general windows: an account is skipped only
// for being ineligible or about to bill, matching EarliestReset's own
// account-wide notion of "exhausted" (every provider's GoverningWindows("")
// is exactly the general set, so no account is excluded here for having no
// family windows at all — general capacity is a fact about the whole pool
// regardless of provider).
func (p *Pool) FamilyHasCapacity(family string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, a := range p.accounts {
		if !a.eligible() {
			continue
		}
		model, ok := familyRepresentativeModel(a, family)
		if !ok {
			continue // this account's provider has nothing to do with family
		}
		if p.windowRejectionExcludes(a, model, now) {
			continue
		}
		if p.wouldBill(a) {
			continue
		}
		return true
	}
	return false
}

// EarliestFamilyReset reports the soonest moment ANY account could regain
// capacity for family — the same two facts EarliestReset and
// EarliestWindowReset each report for a live request's own body, unified
// per-family and across every account, for issue #230's recovery
// notifications: those fire from a background watcher with no request body
// to derive a model from, so they need this instead of EarliestWindowReset.
//
// ok=false when nothing gives a scheduled answer — every blocking account
// is disabled, or family isn't recognised by anything in the pool
// (familyRepresentativeModel ok=false for every account) — the same
// "unknown, don't guess" answer EarliestReset/EarliestWindowReset give.
func (p *Pool) EarliestFamilyReset(family string) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var earliest time.Time
	ok := false
	consider := func(t time.Time) {
		if !ok || t.Before(earliest) {
			earliest, ok = t, true
		}
	}
	for _, a := range p.accounts {
		if a.State() == StateDisabled {
			continue
		}
		if a.State() == StateExhausted {
			consider(a.ExhaustedUntil())
		}
		model, mok := familyRepresentativeModel(a, family)
		if !mok {
			continue
		}
		if u, wok := a.earliestWindowRejectionFor(model, now); wok {
			consider(u)
		}
	}
	return earliest, ok
}
