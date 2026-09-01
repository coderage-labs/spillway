package reqlog

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCacheStatHitRate(t *testing.T) {
	if got := (CacheStat{}).HitRate(); got != nil {
		t.Errorf("HitRate with no volume = %v, want nil", got)
	}
	c := CacheStat{CacheCreationInputTokens: 4165, CacheReadInputTokens: 1816}
	got := c.HitRate()
	if got == nil {
		t.Fatal("HitRate = nil, want a value")
	}
	want := 1816.0 / (1816.0 + 4165.0)
	if *got != want {
		t.Errorf("HitRate = %v, want %v", *got, want)
	}
}

func TestCacheStatsAggregatesPerAccount(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	rows := []Entry{
		{Account: "work", Event: EventServed, InputTokens: 10, OutputTokens: 20,
			CacheCreationInputTokens: 100, CacheReadInputTokens: 50},
		{Account: "work", Event: EventServed, InputTokens: 5, OutputTokens: 15,
			CacheCreationInputTokens: 0, CacheReadInputTokens: 200},
		{Account: "kimi", Event: EventServed, InputTokens: 1, OutputTokens: 1,
			CacheCreationInputTokens: 40, CacheReadInputTokens: 0},
	}
	for _, e := range rows {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := l.CacheStats()
	if err != nil {
		t.Fatal(err)
	}
	byAcct := map[string]CacheStat{}
	for _, s := range stats {
		byAcct[s.Account] = s
	}

	work, ok := byAcct["work"]
	if !ok {
		t.Fatal("no aggregate for account 'work'")
	}
	if work.CacheCreationInputTokens != 100 || work.CacheReadInputTokens != 250 {
		t.Errorf("work aggregate = %+v, want create=100 read=250", work)
	}
	if work.InputTokens != 15 || work.OutputTokens != 35 {
		t.Errorf("work aggregate = %+v, want input=15 output=35", work)
	}

	kimi, ok := byAcct["kimi"]
	if !ok {
		t.Fatal("no aggregate for account 'kimi'")
	}
	if kimi.CacheCreationInputTokens != 40 || kimi.CacheReadInputTokens != 0 {
		t.Errorf("kimi aggregate = %+v, want create=40 read=0", kimi)
	}
}

// TestRotationCostAttributesCorrectly is issue #110's second question,
// proven directly: within one session, a cache-creation cost that follows a
// same-account request is "stable"; one that follows a DIFFERENT account
// serving the previous request in that session is "rotated". This is the
// schema answering "can a rotation's cost be attributed to rotation, not to
// anything else" — see RotationCost's doc.
func TestRotationCostAttributesCorrectly(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Session "s1": three requests, in order, first on "work" (no
	// predecessor — never a rotation cost), then "work" again (same
	// account — stable), then rotates to "kimi" (rotation cost).
	base := int64(1_000_000)
	for i, e := range []Entry{
		{Account: "work", Event: EventServed, SessionHash: "s1", CacheCreationInputTokens: 4165},
		{Account: "work", Event: EventServed, SessionHash: "s1", CacheCreationInputTokens: 30},
		{Account: "kimi", Event: EventRotatedQuota, SessionHash: "s1", CacheCreationInputTokens: 4200},
		// Session "s2": a single request with no predecessor at all.
		{Account: "work", Event: EventServed, SessionHash: "s2", CacheCreationInputTokens: 999},
		// No session hash at all (identity/passthrough path): excluded
		// entirely, on either side.
		{Account: "work", Event: EventPassthrough, CacheCreationInputTokens: 777},
	} {
		e.Ts = time.UnixMilli(base + int64(i)*1000)
		if err := l.Record(e); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}

	rc, err := l.RotationCost()
	if err != nil {
		t.Fatal(err)
	}
	// Rotated: only the "kimi" request in s1 (4200).
	if rc.RotatedCacheCreationTokens != 4200 {
		t.Errorf("RotatedCacheCreationTokens = %d, want 4200", rc.RotatedCacheCreationTokens)
	}
	// Stable: s1's first request (4165, no predecessor) + s1's second
	// (30, same account) + s2's only request (999, no predecessor) = 5194.
	// The session-less row (777) is excluded from both.
	if rc.StableCacheCreationTokens != 5194 {
		t.Errorf("StableCacheCreationTokens = %d, want 5194", rc.StableCacheCreationTokens)
	}
}
