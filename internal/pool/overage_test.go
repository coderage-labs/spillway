package pool

// Extra usage (pay-as-you-go past the subscription quota).
//
// Every test here is about not spending money by accident. The default must
// be no, an unknown state must be no, and the tier must be unreachable while
// any free account can serve — because the failure mode is a bill the user
// finds out about weeks later.

import (
	"net/http"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/provider"
)

func boolp(b bool) *bool { return &b }

// overageRig: one exhausted account that CAN bill, one healthy account.
func overageRig(t *testing.T) (p *Pool, billable, healthy *Account) {
	t.Helper()
	now := time.Now()
	billable = NewAccount("billable", SourceYAML, "tok", "", 0, "")
	billable.Type = "claude-oauth"
	billable.setOverage(provider.Overage{Known: true, Available: true}, time.Now())
	healthy = NewAccount("healthy", SourceYAML, "tok", "", 0, "")
	healthy.Type = "claude-oauth"
	p = New([]*Account{billable, healthy}, now)
	p.MarkExhausted(billable, now.Add(4*time.Hour))
	return p, billable, healthy
}

// The default. Nothing opted in, so the tier does not exist.
func TestOverageIsOffByDefault(t *testing.T) {
	p, billable, _ := overageRig(t)
	if p.allowOverage {
		t.Fatal("overage defaulted to on — it must never spend without being asked")
	}
	if billable.CanOverage(p.allowOverage) {
		t.Error("an account reports itself billable while the pool forbids it")
	}
	// Exhaust the healthy one too: with overage off, this must go dry rather
	// than quietly reach for the paid account.
	p.MarkExhausted(p.accounts[1], time.Now().Add(4*time.Hour))
	if got := p.SelectFor("s", nil); got != nil {
		t.Errorf("selected %s with overage disabled — spillway spent money uninvited", got.Name)
	}
}

// Enabled, but never while anything free can serve.
func TestOverageIsNeverReachedWhileAFreeAccountWorks(t *testing.T) {
	p, _, healthy := overageRig(t)
	p.allowOverage = true
	for i := 0; i < 5; i++ {
		got := p.SelectFor("s", nil)
		if got != healthy {
			t.Fatalf("selected %v, want the free account — the paid tier must be last", got)
		}
		p.Done(got)
	}
}

// Enabled, and everything free is gone: serving beats holding for hours.
func TestOverageServesWhenEverythingFreeIsSpent(t *testing.T) {
	p, billable, healthy := overageRig(t)
	p.allowOverage = true
	p.MarkExhausted(healthy, time.Now().Add(4*time.Hour))

	got := p.SelectFor("s", nil)
	if got != billable {
		t.Fatalf("selected %v, want the billable account", got)
	}
	p.Done(got)
}

// A per-account no overrides a pool-wide yes: the account may be billed to
// someone else entirely.
func TestPerAccountOptOutBeatsThePoolDefault(t *testing.T) {
	p, billable, healthy := overageRig(t)
	p.allowOverage = true
	billable.allowOverage = boolp(false)
	p.MarkExhausted(healthy, time.Now().Add(4*time.Hour))

	if got := p.SelectFor("s", nil); got != nil {
		t.Errorf("selected %s despite its per-account opt-out", got.Name)
	}
}

// ...and a per-account yes works with the pool default off, for the case
// where exactly one account is yours to spend on.
func TestPerAccountOptInWorksWithThePoolOff(t *testing.T) {
	p, billable, healthy := overageRig(t)
	billable.allowOverage = boolp(true)
	p.MarkExhausted(healthy, time.Now().Add(4*time.Hour))

	got := p.SelectFor("s", nil)
	if got != billable {
		t.Fatalf("selected %v, want the opted-in account", got)
	}
	p.Done(got)
}

// Never billable on a guess. An account we have never heard from, or one
// whose provider said something we do not recognise, is not spent on.
func TestUnknownOverageStateIsNeverBilled(t *testing.T) {
	p, _, healthy := overageRig(t)
	p.allowOverage = true
	silent := NewAccount("never-used", SourceYAML, "tok", "", 0, "")
	silent.Type = "claude-oauth"
	if silent.CanOverage(true) {
		t.Error("an account that has never responded was judged billable")
	}
	rejected := NewAccount("rejected", SourceYAML, "tok", "", 0, "")
	rejected.setOverage(provider.Overage{Known: true, Available: false, Reason: "member_zero_credit_limit"}, time.Now())
	if rejected.CanOverage(true) {
		t.Error("an account the provider refused overage for was judged billable")
	}
	// A value we have not seen before must fail closed.
	odd := provider.For("claude-oauth").OverageFromHeaders(http.Header{
		"Anthropic-Ratelimit-Unified-Overage-Status": []string{"pending_review"},
	})
	if odd.Available {
		t.Errorf("an unrecognised overage status %q was treated as permission to bill", "pending_review")
	}
	_ = healthy
}

// A dead credential or an operator park still means no, even with overage on:
// the tier admits exhausted accounts, not broken ones.
func TestOverageDoesNotReviveParkedOrDisabledAccounts(t *testing.T) {
	p, billable, healthy := overageRig(t)
	p.allowOverage = true
	p.MarkExhausted(healthy, time.Now().Add(4*time.Hour))

	billable.Park()
	if got := p.SelectFor("s1", nil); got != nil {
		t.Errorf("selected parked account %s", got.Name)
	}
	billable.Unpark()
	billable.Disable()
	if got := p.SelectFor("s2", nil); got != nil {
		t.Errorf("selected disabled account %s", got.Name)
	}
}

// The headers Anthropic actually sends, captured live on 2026-08-22.
func TestAnthropicOverageHeadersAreReadAsObserved(t *testing.T) {
	spec := provider.For("claude-oauth")
	if spec.OverageFromHeaders == nil {
		t.Fatal("the anthropic provider does not read overage headers")
	}
	live := http.Header{
		"Anthropic-Ratelimit-Unified-Overage-Status":          []string{"rejected"},
		"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": []string{"member_zero_credit_limit"},
	}
	ov := spec.OverageFromHeaders(live)
	if !ov.Known {
		t.Error("the header was present but the state came back unknown")
	}
	if ov.Available {
		t.Error("\"rejected\" was read as permission to bill")
	}
	if ov.Reason != "member_zero_credit_limit" {
		t.Errorf("reason = %q, want the provider's explanation", ov.Reason)
	}

	allowed := http.Header{"Anthropic-Ratelimit-Unified-Overage-Status": []string{"allowed"}}
	if got := spec.OverageFromHeaders(allowed); !got.Known || !got.Available {
		t.Errorf("\"allowed\" read as %+v, want available", got)
	}

	// Absent entirely: no opinion, not a denial. The distinction matters for
	// providers that never send it.
	if got := spec.OverageFromHeaders(http.Header{}); got.Known {
		t.Errorf("a missing header produced a definite answer: %+v", got)
	}
}

// The state is recorded from live traffic, not only from probes.
func TestRecordQuotaCapturesOverage(t *testing.T) {
	a := NewAccount("a", SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())
	// Deliberately carries no window headers: overage must still be picked
	// up, since it is reported on every response.
	p.RecordQuota(a, http.Header{
		"Anthropic-Ratelimit-Unified-Overage-Status": []string{"allowed"},
	}, time.Now())

	if ov := a.Overage(); !ov.Known || !ov.Available {
		t.Errorf("overage = %+v after a response that allowed it", ov)
	}
}

// The deadlock this rule exists to break.
//
// Spillway will not probe an account whose quota is spent, because that probe
// would be billed. So on the very account extra usage is for, no confirming
// header ever arrives. If a confirmed header were required, the feature would
// be permanently unreachable exactly where it is needed.
func TestExplicitOptInWorksWithoutAConfirmingHeader(t *testing.T) {
	now := time.Now()
	unseen := NewAccount("never-heard-from", SourceYAML, "tok", "", 0, "")
	unseen.Type = "claude-oauth"
	unseen.allowOverage = boolp(true)
	healthy := NewAccount("healthy", SourceYAML, "tok", "", 0, "")
	healthy.Type = "claude-oauth"
	p := New([]*Account{unseen, healthy}, now)
	p.MarkExhausted(unseen, now.Add(4*time.Hour))
	p.MarkExhausted(healthy, now.Add(4*time.Hour))

	if ov := unseen.Overage(); ov.Known {
		t.Fatal("precondition: this account is supposed to have told us nothing")
	}
	got := p.SelectFor("s", nil)
	if got != unseen {
		t.Fatalf("selected %v — an explicitly opted-in account was unreachable because "+
			"no header ever confirmed it, which is the deadlock the rule exists to break", got)
	}
	p.Done(got)
}

// The provider's "no" still wins over an opt-in: the request would be
// rejected anyway, and arguing costs a round trip.
func TestProviderRefusalBeatsAnOptIn(t *testing.T) {
	now := time.Now()
	a := NewAccount("refused", SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.allowOverage = boolp(true)
	a.setOverage(provider.Overage{Known: true, Available: false, Reason: "member_zero_credit_limit"}, time.Now())
	p := New([]*Account{a}, now)
	p.MarkExhausted(a, now.Add(4*time.Hour))

	if got := p.SelectFor("s", nil); got != nil {
		t.Errorf("selected %s after the provider said overage is rejected", got.Name)
	}
}

// A pool-wide yes is weaker: the user singled nothing out, so an unconfirmed
// account is not spent on.
func TestPoolWideYesStillNeedsConfirmation(t *testing.T) {
	now := time.Now()
	a := NewAccount("unconfirmed", SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, now)
	p.allowOverage = true
	p.MarkExhausted(a, now.Add(4*time.Hour))

	if got := p.SelectFor("s", nil); got != nil {
		t.Errorf("selected %s on a pool-wide default with nothing confirming "+
			"the account can bill at all", got.Name)
	}
}

// Every free account's quota is spent before any money is.
//
// Once no account is under threshold, tier 2 picks among the rest — and
// picking by load alone would reach for the paid account while a free one
// has not even been asked. A free account being over threshold costs nothing
// to try: the provider may still say yes, and if it says no the 429 is free.
func TestFreeQuotaIsFullyDrainedBeforeAnythingIsBilled(t *testing.T) {
	now := time.Now()
	// Paid account listed FIRST and with zero load, so load-order would pick
	// it. Both accounts are over threshold; neither has been rejected yet.
	paid := NewAccount("paid", SourceYAML, "tok", "", 0, "")
	paid.Type = "claude-oauth"
	paid.allowOverage = boolp(true)
	paid.SetQuotaWindows([]QuotaWindow{
		{Name: "7d", Limit: 1, Used: 1, ResetAt: now.Add(9 * time.Hour), FetchedAt: now},
	})
	free := NewAccount("free", SourceYAML, "tok", "", 0, "")
	free.Type = "claude-oauth"
	free.SetQuotaWindows([]QuotaWindow{
		{Name: "7d", Limit: 1, Used: 0.995, ResetAt: now.Add(9 * time.Hour), FetchedAt: now},
	})
	p := New([]*Account{paid, free}, now)
	p.allowOverage = true

	got := p.SelectFor("s", nil)
	if got != free {
		t.Fatalf("selected %v, want the free account — its quota has not actually "+
			"been refused yet, and trying it costs nothing", got)
	}
	p.Done(got)

	// Only once the free account is genuinely rejected does the paid one
	// come into play.
	p.MarkExhausted(free, now.Add(9*time.Hour))
	got = p.SelectFor("s2", nil)
	if got != paid {
		t.Fatalf("selected %v, want the paid account once every free one is spent", got)
	}
	p.Done(got)
}

// The overage tier admits exhausted accounts, so nothing else stops a refused
// account being handed straight back. Without the skip set the caller spins.
func TestARefusedOverageAccountIsNotHandedBackImmediately(t *testing.T) {
	p, billable, healthy := overageRig(t)
	p.allowOverage = true
	p.MarkExhausted(healthy, time.Now().Add(4*time.Hour))

	got := p.SelectFor("s", nil)
	if got != billable {
		t.Fatalf("precondition: expected the billable account, got %v", got)
	}
	p.Done(got)

	// It just refused. Asking again within the same request must not return
	// it — quota state alone cannot rule it out, because being exhausted is
	// the tier's entry condition.
	again := p.SelectExcept("s", nil, map[string]bool{billable.Name: true})
	if again != nil {
		t.Errorf("re-selected %s after it refused — this is a tight retry loop "+
			"against the provider", again.Name)
	}
}
