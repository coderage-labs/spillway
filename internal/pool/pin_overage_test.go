package pool

// The provider-side half of the pin money guard (issue #139). The old guard
// asked p.wouldBill, which begins with CanOverage and so answers "would
// spillway choose to bill here?" — false whenever spillway has not been
// given permission to spend. A pin bypasses selection, so once the account
// is named the decision is the provider's, and `overage-status: allowed`
// upstream means the next pinned request is served on extra usage and
// charged with allowOverage still off.
//
// Both directions matter here. Over-refusing a pin is as much a bug as
// under-refusing one: the only remedy a refused pin offers is --force, so a
// guard that fires on accounts nothing would charge for teaches --force as a
// reflex and stops protecting the accounts where money is real.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/provider"
)

// spentCurrentWindow is a current reading of a fully used window: at its limit, and
// with a reset far enough ahead that issue #135's expiry leaves it standing.
func spentCurrentWindow() []QuotaWindow {
	return []QuotaWindow{{
		Name: "5h", Limit: 1, Used: 1, Source: "headers",
		FetchedAt: time.Now(), ResetAt: time.Now().Add(time.Hour),
	}}
}

// The reported bug. Spillway's own overage is off — the default, and the
// state the README's "only if you tell it to" promise describes — but the
// provider says extra usage is allowed, so the pinned request bills.
func TestPinRefusesWhenOnlyTheProviderWouldBill(t *testing.T) {
	p, a, _ := pinPool(t)
	a.SetQuotaWindows(spentCurrentWindow())
	a.SetOverageForTest(provider.Overage{Known: true, Available: true})

	// The precondition that makes this issue #139 and not a duplicate of the
	// existing guard: spillway would not choose to spend here.
	if p.AllowOverage() {
		t.Fatal("test setup: pool overage is on, which is the case the old guard already caught")
	}
	if p.WouldBill(a) {
		t.Fatal("test setup: the old guard already refuses this, so it proves nothing")
	}

	err := p.Pin("first", false)
	if !errors.Is(err, ErrPinWouldBill) {
		t.Fatalf("err = %v, want ErrPinWouldBill", err)
	}
	if p.Pinned() != "" {
		t.Error("refused and pinned anyway")
	}

	// And --force still means it.
	if err := p.Pin("first", true); err != nil {
		t.Fatalf("force should permit it: %v", err)
	}
	if p.Pinned() != "first" {
		t.Errorf("Pinned() = %q after a forced pin", p.Pinned())
	}
}

// A refusal the reader cannot explain is a support ticket: "would spend
// money" on an account whose spillway-side overage is off reads as a bug
// unless the message says whose decision it is. Asserted on the returned
// error, not on log text.
func TestPinRefusalNamesTheProviderSideCause(t *testing.T) {
	p, a, _ := pinPool(t)
	a.SetQuotaWindows(spentCurrentWindow())
	a.SetOverageForTest(provider.Overage{Known: true, Available: true})

	err := p.Pin("first", false)
	if err == nil {
		t.Fatal("pinned a provider-billable account")
	}
	msg := err.Error()
	for _, want := range []string{
		"claude",        // whose decision it is
		"extra usage",   // what the provider has enabled
		"overage",       // spillway's own setting, named so it is not blamed
		"off",           // ... and that having it off is not what saves you
		"no quota left", // why the provider gets to decide at all
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q: %s", want, msg)
		}
	}
}

// Over-refusal, direction one: headroom. The threshold is a rotation
// heuristic and a pin exists to override it, so an account merely past it
// still has free quota and the next request costs nothing. Set low, as a
// user may, to separate "past the threshold" from "spent".
func TestPinAllowsHeadroomEvenWithProviderOverageOn(t *testing.T) {
	p, a, _ := pinPool(t)
	p.Apply(Settings{SwitchThreshold: 0.5})
	a.SetQuotaWindows([]QuotaWindow{{
		Name: "5h", Limit: 1, Used: 0.6, Source: "headers",
		FetchedAt: time.Now(), ResetAt: time.Now().Add(time.Hour),
	}})
	a.SetOverageForTest(provider.Overage{Known: true, Available: true})

	if !a.OverThreshold(p.Threshold()) {
		t.Fatal("test setup: account is not over the threshold, so this proves nothing")
	}
	if err := p.Pin("first", false); err != nil {
		t.Fatalf("refused a pin on an account with 40%% of its quota left: %v", err)
	}
	if got := p.Select("s"); got == nil || got.Name != "first" {
		t.Errorf("selected %v; the pin should have taken effect", got)
	}
}

// Over-refusal, direction two: the provider has said no. Then the request is
// refused rather than billed, so there is no money at stake and nothing to
// refuse — believing the provider is already CanOverage's first rule.
func TestPinAllowsSpentAccountWhenTheProviderRefusesOverage(t *testing.T) {
	p, a, _ := pinPool(t)
	a.SetQuotaWindows(spentCurrentWindow())
	a.SetOverageForTest(provider.Overage{
		Known: true, Available: false, Reason: "member_zero_credit_limit",
	})

	if err := p.Pin("first", false); err != nil {
		t.Fatalf("refused a pin the provider would not bill: %v", err)
	}
}

// Over-refusal, direction three: an unknown reading. Documented choice —
// unknown does not refuse. The refusal would be unactionable (the only way
// to make the reading known is to send a request, and the only way to send
// one on a refused pin is the --force this guard exists to gate), it is the
// rule Account.Overage and CanOverage already state, and `overage` reads
// null on real accounts including the reporter's, so refusing here would
// refuse the common case rather than the dangerous one.
func TestPinAllowsSpentAccountWithAnUnknownOverageReading(t *testing.T) {
	p, a, _ := pinPool(t)
	a.SetQuotaWindows(spentCurrentWindow())

	if a.Overage().Known {
		t.Fatal("test setup: overage reading is known")
	}
	if err := p.Pin("first", false); err != nil {
		t.Fatalf("refused a pin on an account with no overage reading: %v", err)
	}
}

// A confirmed quota rejection is the other way quota can be gone. The pin is
// inert while the account is exhausted (pinnedChoice needs eligible), but the
// exhaustion deadline passes and the pin then serves an account whose quota
// may still be spent — so the refusal belongs at Pin time, not at selection.
func TestPinRefusesAnExhaustedAccountTheProviderWouldBill(t *testing.T) {
	p, a, _ := pinPool(t)
	a.SetOverageForTest(provider.Overage{Known: true, Available: true})
	p.MarkExhausted(a, time.Now().Add(time.Hour))

	if p.WouldBill(a) {
		t.Fatal("test setup: the old guard already refuses this")
	}
	if err := p.Pin("first", false); !errors.Is(err, ErrPinWouldBill) {
		t.Fatalf("err = %v, want ErrPinWouldBill", err)
	}
}

// Issue #135's expiry carries through: a spent reading whose own reset has
// passed says nothing about now, so it must not refuse a pin. Refusing on a
// number nobody has re-measured would be the mirror of the bug #135 fixed —
// and the account this test describes is the ordinary morning-after case, an
// account spent yesterday and refilled since.
func TestPinIgnoresAnExpiredSpentWindow(t *testing.T) {
	p, a, _ := pinPool(t)
	past := time.Now().Add(-2 * time.Hour)
	a.SetQuotaWindows([]QuotaWindow{{
		Name: "5h", Limit: 1, Used: 1, Source: "headers",
		FetchedAt: past.Add(-time.Hour), ResetAt: past,
	}})
	a.SetOverageForTest(provider.Overage{Known: true, Available: true})

	if err := p.Pin("first", false); err != nil {
		t.Fatalf("refused a pin on an expired spent reading: %v", err)
	}
}

// A per-window rejection needs nothing from this guard, and must not be
// turned into a pin refusal: MarkWindowRejected forges a spent row so the
// dashboard shows it, but SelectExcept's usable() already drops the pinned
// account for that model outright, so no fable request reaches it to be
// billed — while Sonnet, governed only by the account-wide windows, still
// serves free from the same pin. Refusing here would take away a pin that
// works.
//
// Documented as the deliberate asymmetry it is: the guard fires on a spent
// HEADER reading for any window (the #135 account: 7d-fable spent, 5h idle,
// no rejection, and a fable request bills), and not on a rejected one.
func TestPinAllowsAnAccountWithOnlyARejectedWindow(t *testing.T) {
	p, a, _ := pinPool(t)
	a.SetOverageForTest(provider.Overage{Known: true, Available: true})
	p.MarkWindowRejected(a, "7d-fable", time.Now().Add(time.Hour))

	if err := p.Pin("first", false); err != nil {
		t.Fatalf("refused a pin whose only spent window upstream already refuses: %v", err)
	}
	body := []byte(`{"model":"claude-sonnet-4-6"}`)
	if got := p.SelectFor("s", body); got == nil || got.Name != "first" {
		t.Errorf("selected %v; sonnet is not governed by the rejected window", got)
	}
}
