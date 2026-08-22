package pool

// Predictive rotation against a real exhausted account.
//
// The numbers here are a live snapshot (2026-08-22): one Claude account with
// its 7-day window fully consumed and its reset hours away, alongside a
// healthy one. Until now rotation had only ever been exercised against a
// mocked 429 — which proves the reactive path but says nothing about the
// predictive one, where the point is to never send the request at all.

import (
	"testing"
	"time"
)

// liveSnapshot builds the two-account pool as it stood, with the spent
// account FIRST. Order matters: leastLoaded breaks ties by list position, so
// if the threshold check were removed the spent account would win. That is
// what makes this a test rather than a coincidence.
func liveSnapshot(t *testing.T) (p *Pool, spent, healthy *Account) {
	t.Helper()
	now := time.Now()
	spent = NewAccount("you@side.example", SourceYAML, "tok", "", 0, "")
	spent.Type = "claude-oauth"
	spent.SetQuotaWindows([]QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0, ResetAt: now.Add(4 * time.Hour), FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 1, ResetAt: now.Add(14 * time.Hour), FetchedAt: now},
	})
	healthy = NewAccount("you@acme.example", SourceYAML, "tok", "", 0, "")
	healthy.Type = "claude-oauth"
	healthy.SetQuotaWindows([]QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.16, ResetAt: now.Add(2 * time.Hour), FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 0.08, ResetAt: now.Add(26 * time.Hour), FetchedAt: now},
	})
	p = New([]*Account{spent, healthy}, now)
	p.SwitchThreshold = 0.98
	return p, spent, healthy
}

func TestLiveSpentAccountIsSkippedBeforeTheRequestIsSent(t *testing.T) {
	p, spent, healthy := liveSnapshot(t)

	// The account is not in StateExhausted — nothing has 429'd it. Only the
	// quota headers say it is finished, which is the whole point: the
	// rotation must happen without spending a request to discover it.
	if spent.State() != StateOK {
		t.Fatalf("precondition: spent account is %v, want StateOK — this test is about "+
			"the predictive path, not the reactive one", spent.State())
	}
	if !spent.OverThreshold(p.threshold()) {
		t.Fatalf("a 7d window at 100%% is not over the 0.98 threshold")
	}
	if healthy.OverThreshold(p.threshold()) {
		t.Fatal("the healthy account was judged over threshold")
	}

	got := p.SelectFor("session-1", nil)
	if got == nil {
		t.Fatal("nothing selected while a healthy account was available")
	}
	if got != healthy {
		t.Errorf("selected %s, want the healthy account %s — the spent account is "+
			"first in the list, so this is list order winning over the threshold",
			got.Name, healthy.Name)
	}
	p.Done(got)
}

// Stickiness must not defeat it: a session already pinned to an account that
// has since filled up has to be moved, or predictive rotation only works for
// brand-new sessions.
func TestLiveStickySessionLeavesAnAccountThatFilledUp(t *testing.T) {
	p, spent, healthy := liveSnapshot(t)
	p.sticky["session-1"] = spent.Name

	got := p.SelectFor("session-1", nil)
	if got != healthy {
		t.Errorf("sticky session stayed on %v, want a move to %s", got, healthy.Name)
	}
	p.Done(got)
}

// The fallthrough: when everything is over threshold, serving beats failing.
// Anthropic may still accept the request, and refusing locally guarantees it
// does not.
func TestLiveEverythingSpentStillServes(t *testing.T) {
	p, spent, healthy := liveSnapshot(t)
	healthy.SetQuotaWindows([]QuotaWindow{
		{Name: "7d", Limit: 1, Used: 0.99, ResetAt: time.Now().Add(26 * time.Hour), FetchedAt: time.Now()},
	})

	got := p.SelectFor("session-2", nil)
	if got == nil {
		t.Fatal("nothing selected — spillway refused locally instead of letting " +
			"the provider decide, which turns a maybe into a certain failure")
	}
	p.Done(got)
	_ = spent
}
