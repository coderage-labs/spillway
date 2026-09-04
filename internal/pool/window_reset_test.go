package pool

// Tests for issue #140: EarliestReset only ever spoke for accounts in
// StateExhausted, and a family-scoped 429 deliberately never puts an
// account there (issue #54). EarliestWindowReset is the missing half —
// when does the per-window refusal blocking THIS request expire.
//
// Fixtures (fableBody, fableModel, sonnetModel) are family_test.go's,
// deliberately: this is the same per-family machinery.

import (
	"testing"
	"time"
)

// The headline, at pool level. Every account is fable-rejected and nothing
// is exhausted, so EarliestReset has nothing to say — which is exactly the
// state in which the hold used to sleep its whole budget. The soonest of
// the two rejection deadlines must come back instead.
func TestEarliestWindowResetReportsFableDeadlineWhenNothingExhausted(t *testing.T) {
	soon := NewAccount("soon", SourceYAML, "t", "", 0, "")
	soon.Type = "claude-oauth"
	late := NewAccount("late", SourceYAML, "t", "", 0, "")
	late.Type = "claude-oauth"

	p := New([]*Account{late, soon}, time.Now())
	soonAt := time.Now().Add(10 * time.Minute).Round(0)
	p.MarkWindowRejected(soon, "7d-fable", soonAt)
	p.MarkWindowRejected(late, "7d-fable", time.Now().Add(4*time.Hour))

	if _, ok := p.EarliestReset(); ok {
		t.Fatal("EarliestReset reported a reset — the premise of #140 is that it cannot see a window rejection")
	}
	if got := p.SelectFor("s", fableBody(fableModel)); got != nil {
		t.Fatalf("SelectFor(fable) = %q, want nil — the request must actually be parked", got.Name)
	}

	got, ok := p.EarliestWindowReset(fableBody(fableModel))
	if !ok {
		t.Fatal("EarliestWindowReset(fable) reported nothing — this is the deadline the hold is waiting on")
	}
	if !got.Equal(soonAt) {
		t.Errorf("EarliestWindowReset(fable) = %v, want %v (the SOONEST of the two, not the 4h one)", got, soonAt)
	}
}

// A fable rejection governs only fable. A Sonnet request is not waiting on
// it — Sonnet is still being served off these accounts (#54's whole point),
// and reporting the fable deadline for it would be a wake time for a
// request that has no reason to be parked at all.
func TestEarliestWindowResetIgnoresWindowsThatDoNotGovernTheModel(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())
	p.MarkWindowRejected(a, "7d-fable", time.Now().Add(10*time.Minute))

	if _, ok := p.EarliestWindowReset(fableBody(sonnetModel)); ok {
		t.Error("EarliestWindowReset(sonnet) reported the 7d-fable deadline — 7d-fable does not govern Sonnet")
	}
	if _, ok := p.EarliestWindowReset(nil); ok {
		t.Error("EarliestWindowReset(no body) reported the 7d-fable deadline — an unrecognised model resolves to the account-wide windows only")
	}
	if got, ok := p.EarliestWindowReset(fableBody(fableModel)); !ok {
		t.Error("EarliestWindowReset(fable) reported nothing — 7d-fable does govern fable")
	} else if time.Until(got) <= 0 {
		t.Errorf("EarliestWindowReset(fable) = %v, already in the past", got)
	}
}

// An account-wide window works through the same path: a rejected "5h"
// governs every model, so any request parked behind it gets its deadline.
// (In production such a rejection also exhausts the account, so
// EarliestReset would cover it — this pins that the narrowing is by
// GoverningWindows and not a hardcoded "fable only".)
func TestEarliestWindowResetCoversAccountWideWindows(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())
	at := time.Now().Add(7 * time.Minute).Round(0)
	p.MarkWindowRejected(a, "5h", at)

	for _, body := range [][]byte{nil, fableBody(sonnetModel), fableBody(fableModel)} {
		got, ok := p.EarliestWindowReset(body)
		if !ok || !got.Equal(at) {
			t.Errorf("EarliestWindowReset(%s) = %v,%v; want %v,true — 5h governs every model",
				string(body), got, ok, at)
		}
	}
}

// The past-deadline filter, at the line that implements it. An expired
// rejection excludes nobody (WindowRejectedFor already reads false for it),
// so it must not come back as a wake time either: the hold turns a
// non-future reset into "re-select immediately", and an account still
// unusable for another reason would then spin instead of wait.
func TestEarliestWindowResetSkipsExpiredDeadline(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())
	p.MarkWindowRejected(a, "7d-fable", time.Now().Add(-time.Minute))

	if a.WindowRejectedFor(fableModel) {
		t.Fatal("premise broken: an expired rejection should not exclude")
	}
	got, ok := p.EarliestWindowReset(fableBody(fableModel))
	if ok {
		t.Errorf("EarliestWindowReset = %v, true — an expired deadline must not be reported (it is a wake time already in the past)", got)
	}
}

// One expired, one live: the live one is the answer. Guards against a
// filter that bails on the first expired entry it sees rather than skipping
// it.
func TestEarliestWindowResetPrefersLiveDeadlineOverExpiredOne(t *testing.T) {
	stale := NewAccount("stale", SourceYAML, "t", "", 0, "")
	stale.Type = "claude-oauth"
	live := NewAccount("live", SourceYAML, "t", "", 0, "")
	live.Type = "claude-oauth"
	p := New([]*Account{stale, live}, time.Now())
	p.MarkWindowRejected(stale, "7d-fable", time.Now().Add(-time.Hour))
	at := time.Now().Add(10 * time.Minute).Round(0)
	p.MarkWindowRejected(live, "7d-fable", at)

	got, ok := p.EarliestWindowReset(fableBody(fableModel))
	if !ok || !got.Equal(at) {
		t.Errorf("EarliestWindowReset = %v,%v; want %v,true (the live deadline, not the hour-ago one)", got, ok, at)
	}
}

// A disabled account's window deadline says nothing about when it can serve
// again — only re-authentication does, and that path signals capacity
// instead. Same reasoning as EarliestReset walking only the exhausted.
func TestEarliestWindowResetSkipsDisabledAccounts(t *testing.T) {
	dead := NewAccount("dead", SourceYAML, "t", "", 0, "")
	dead.Type = "claude-oauth"
	p := New([]*Account{dead}, time.Now())
	p.MarkWindowRejected(dead, "7d-fable", time.Now().Add(10*time.Minute))
	dead.Disable()

	if got, ok := p.EarliestWindowReset(fableBody(fableModel)); ok {
		t.Errorf("EarliestWindowReset = %v, true — a disabled account does not come back on a quota clock", got)
	}
}

// Kimi has no family-scoped windows at all (GoverningWindows nil); its
// refusals are account-wide StateExhausted, which EarliestReset covers.
// Mirrors TestKimiIgnoresWindowRejected.
func TestEarliestWindowResetIgnoresKimi(t *testing.T) {
	k := NewAccount("kimi", SourceYAML, "t", "", 0, "")
	k.Type = "kimi-oauth"
	p := New([]*Account{k}, time.Now())
	p.MarkWindowRejected(k, "7d-fable", time.Now().Add(10*time.Minute))

	if got, ok := p.EarliestWindowReset(fableBody(fableModel)); ok {
		t.Errorf("EarliestWindowReset = %v, true — Kimi has no per-window path", got)
	}
}

// One account, two governing windows rejected on different deadlines: the
// soonest is the one the hold waits on. Separate from the multi-account
// case above — the inner per-window scan and the outer per-account scan are
// two different loops and either can be wrong on its own.
func TestEarliestWindowResetTakesSoonestGoverningWindowOnOneAccount(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())
	soon := time.Now().Add(2 * time.Minute).Round(0)
	p.MarkWindowRejected(a, "7d-fable", soon)
	p.MarkWindowRejected(a, "7d", time.Now().Add(30*time.Minute))

	got, ok := p.EarliestWindowReset(fableBody(fableModel))
	if !ok || !got.Equal(soon) {
		t.Errorf("EarliestWindowReset = %v,%v; want %v,true (the soonest of the two windows on this account)", got, ok, soon)
	}
}

// Nothing rejected at all: no wake time, so the hold keeps whatever
// EarliestReset told it rather than inventing a zero time.
func TestEarliestWindowResetEmptyPool(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())
	if got, ok := p.EarliestWindowReset(fableBody(fableModel)); ok {
		t.Errorf("EarliestWindowReset = %v, true — nothing is rejected", got)
	}
}
