package pool

import (
	"errors"

	"github.com/coderage-labs/spillway/internal/provider"
	"testing"
	"time"
)

func pinPool(t *testing.T) (*Pool, *Account, *Account) {
	t.Helper()
	a := NewAccount("first", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	b := NewAccount("second", SourceYAML, "t", "", 0, "")
	b.Type = "claude-oauth"
	return New([]*Account{a, b}, time.Now()), a, b
}

// A pin has to beat the choice already made, or the switch appears to do
// nothing until the session happens to rotate on its own.
func TestPinBeatsStickiness(t *testing.T) {
	p, _, _ := pinPool(t)
	first := p.Select("s")
	p.Done(first)
	other := "second"
	if first.Name == "second" {
		other = "first"
	}
	if err := p.Pin(other, false); err != nil {
		t.Fatal(err)
	}
	got := p.Select("s")
	if got.Name != other {
		t.Fatalf("selected %q, want the pinned %q", got.Name, other)
	}
	if p.Pinned() != other {
		t.Errorf("Pinned() = %q", p.Pinned())
	}
}

// Naming an account is a statement that you want it, and the rotate-away
// threshold exists to make an automatic choice rather than overrule an
// explicit one.
func TestPinIgnoresTheRotateAwayThreshold(t *testing.T) {
	p, a, _ := pinPool(t)
	a.SetQuotaWindows([]QuotaWindow{{Name: "5h", Limit: 1, Used: 0.999, Source: "headers"}})
	if !a.OverThreshold(p.Threshold()) {
		t.Fatal("test setup: account is not over threshold")
	}
	if err := p.Pin("first", false); err != nil {
		t.Fatal(err)
	}
	if got := p.Select("s"); got.Name != "first" {
		t.Errorf("selected %q; a pin should survive the threshold", got.Name)
	}
}

// But not exhaustion. Holding every request while a healthy account sits idle
// turns a convenience into a way to take yourself offline by accident.
func TestPinFallsBackWhenTheAccountIsSpent(t *testing.T) {
	p, a, _ := pinPool(t)
	if err := p.Pin("first", false); err != nil {
		t.Fatal(err)
	}
	p.MarkExhausted(a, time.Now().Add(time.Hour))

	got := p.Select("s")
	if got == nil {
		t.Fatal("held with a usable account available")
	}
	if got.Name != "second" {
		t.Errorf("selected %q, want the fallback", got.Name)
	}
	// The pin is not forgotten — it takes effect again when the account is
	// back, which is what a user who pinned it would expect.
	if p.Pinned() != "first" {
		t.Errorf("pin was dropped on fallback: %q", p.Pinned())
	}
}

// Pinning at an account whose only path left is paid extra usage must not be
// the cheap way around the tier that exists to ask permission.
func TestPinRefusesToSpendMoney(t *testing.T) {
	p, a, _ := pinPool(t)
	p.AllowOverage = true
	a.SetOverageForTest(provider.Overage{Known: true, Available: true})
	p.MarkExhausted(a, time.Now().Add(time.Hour))
	if !p.WouldBill(a) {
		t.Fatal("test setup: pinning this account would not bill")
	}

	err := p.Pin("first", false)
	if !errors.Is(err, ErrPinWouldBill) {
		t.Fatalf("err = %v, want ErrPinWouldBill", err)
	}
	if p.Pinned() != "" {
		t.Error("refused and pinned anyway")
	}
	if err := p.Pin("first", true); err != nil {
		t.Fatalf("force should permit it: %v", err)
	}
}

// Moving a live session to another provider hands the client a model it never
// negotiated (§6.18), so it needs asking for twice.
func TestPinRefusesToCrossProvider(t *testing.T) {
	claude := NewAccount("claude", SourceYAML, "t", "", 0, "")
	claude.Type = "claude-oauth"
	kimi := NewAccount("kimi", SourceYAML, "t", "", 0, "")
	kimi.Type = "kimi-oauth"
	p := New([]*Account{claude, kimi}, time.Now())

	p.Done(p.Select("s")) // session is now on claude

	err := p.Pin("kimi", false)
	if !errors.Is(err, ErrPinCrossProvider) {
		t.Fatalf("err = %v, want ErrPinCrossProvider", err)
	}

	if err := p.Pin("kimi", true); err != nil {
		t.Fatalf("force should permit it: %v", err)
	}
	// And it must actually take effect: the session-provider pin would
	// otherwise skip every kimi account and leave the pin doing nothing.
	if got := p.Select("s"); got.Name != "kimi" {
		t.Errorf("selected %q; a forced cross-provider pin did nothing", got.Name)
	}
}

func TestPinRejectsUnusableTargets(t *testing.T) {
	p, _, b := pinPool(t)
	if err := p.Pin("nobody", false); err == nil {
		t.Error("pinned an account that does not exist")
	}
	b.Park()
	if err := p.Pin("second", false); err == nil {
		t.Error("pinned a parked account")
	}
	if err := p.Pin("second", true); err == nil {
		t.Error("force pinned a parked account; force is for money and providers, not for parked")
	}
}

func TestUnpinRestoresOrdinarySelection(t *testing.T) {
	p, _, _ := pinPool(t)
	if err := p.Pin("second", false); err != nil {
		t.Fatal(err)
	}
	p.Done(p.Select("s"))
	p.Unpin()
	if p.Pinned() != "" {
		t.Fatal("still pinned")
	}
	// Sticky keeps it on second, which is correct — unpinning is not a
	// second switch. What matters is that the pin no longer overrides.
	p.MarkExhausted(p.find("second"), time.Now().Add(time.Hour))
	if got := p.Select("s"); got == nil || got.Name != "first" {
		t.Errorf("selected %v; ordinary selection should have resumed", got)
	}
}
