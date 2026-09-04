package accounts

// A spent model family the probe never engages must not be read as "probing
// this account would be billed". Before quota windows were retained across
// readings this was hard to hit; now a spent fable bucket persists until its
// own reset, so an account healthy for everything else would otherwise stop
// being probed for up to a week.

import (
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

func claudeAccount(name string) *pool.Account {
	a := pool.NewAccount(name, pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	return a
}

// billableAccount is a claudeAccount that has opted into extra usage, so a
// probe on it while it is spent really would be charged. Every wouldBill
// case below needs that: since issue #152 a spent account that CANNOT bill
// answers "no" regardless of which windows are spent.
func billableAccount(name string) *pool.Account {
	a := claudeAccount(name)
	yes := true
	a.SetAllowOverage(&yes)
	return a
}

func TestSpentFableBucketDoesNotSuppressProbing(t *testing.T) {
	a := billableAccount("work")
	now := time.Now()
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.02, Source: "headers", ResetAt: now.Add(time.Hour), FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 0.79, Source: "headers", ResetAt: now.Add(48 * time.Hour), FetchedAt: now},
		{Name: "7d-fable", Limit: 1, Used: 1.0, Source: "headers", ResetAt: now.Add(48 * time.Hour), FetchedAt: now},
	})

	if wouldBill(a, false, now) {
		t.Error("a spent fable bucket suppressed probing; the probe asks for a non-fable model, so that family cannot charge it")
	}
}

// The guard the original behaviour exists for must survive: a spent window
// the probe DOES draw on still suppresses it, because that probe would be
// billed to re-learn a reading already on file.
func TestSpentGeneralWindowStillSuppressesProbing(t *testing.T) {
	a := billableAccount("work")
	now := time.Now()
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 1.0, Source: "headers", ResetAt: now.Add(time.Hour), FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 0.5, Source: "headers", ResetAt: now.Add(48 * time.Hour), FetchedAt: now},
	})

	if !wouldBill(a, false, now) {
		t.Error("a spent 5h window no longer suppresses probing; the probe draws on it and would be charged")
	}
}

// A provider with no per-family notion keeps the original all-windows
// behaviour rather than silently probing a spent account.
func TestProviderWithoutFamiliesKeepsWholeAccountRule(t *testing.T) {
	a := pool.NewAccount("kimi", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "kimi-oauth"
	yes := true
	a.SetAllowOverage(&yes)
	now := time.Now()
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "weekly", Limit: 100, Used: 100, Source: "poll", ResetAt: now.Add(48 * time.Hour), FetchedAt: now},
	})

	if !wouldBill(a, false, now) {
		t.Error("a spent window on a provider with no window families must still suppress probing")
	}
}
