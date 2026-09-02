package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/accounts"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

func openSampleLog(t *testing.T) *reqlog.Log {
	t.Helper()
	l, err := reqlog.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func windowNamed(ws []pool.QuotaWindow, name string) *pool.QuotaWindow {
	for i := range ws {
		if ws[i].Name == name {
			return &ws[i]
		}
	}
	return nil
}

// sampleQuota's basic contract: it writes the window's own FetchedAt into
// the sample, not the sampling time, and it now writes an Expired window
// too (see sampleQuota's doc for why the old skip was removed).
func TestSampleQuotaRecordsTheWindowsOwnFetchedAt(t *testing.T) {
	rl := openSampleLog(t)
	now := time.Now()
	measuredAt := now.Add(-40 * time.Hour)

	a := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "7d-fable", Limit: 1, Used: 1, ResetAt: now.Add(5 * time.Hour), FetchedAt: measuredAt},
	})
	p := pool.New([]*pool.Account{a}, now)

	sampleQuota(p, rl, now, discardLogger())

	got, err := rl.LatestQuotaSamples(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 sample, got %d", len(got))
	}
	if !got[0].FetchedAt.Truncate(time.Millisecond).Equal(measuredAt.Truncate(time.Millisecond)) {
		t.Errorf("recorded FetchedAt = %v, want the window's own %v", got[0].FetchedAt, measuredAt)
	}
	if !got[0].Ts.Truncate(time.Millisecond).Equal(now.Truncate(time.Millisecond)) {
		t.Errorf("recorded Ts = %v, want the sampling time %v passed in, not the window's FetchedAt", got[0].Ts, now)
	}
}

// A window already past its reset (Expired) must still be sampled — this is
// the crux of the #135 interaction (see
// TestSeedQuotaSurvivesASiblingTurnoverAcrossARestart below): the OLD
// sampler skipped Expired windows, which meant a turnover retirement's
// corrected (past) ResetAt could never reach disk.
func TestSampleQuotaRecordsAnExpiredWindow(t *testing.T) {
	rl := openSampleLog(t)
	now := time.Now()

	a := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetQuotaWindows([]pool.QuotaWindow{
		// ResetAt in the past + FetchedAt before it => Expired (currentAt=false).
		{Name: "7d-fable", Limit: 1, Used: 1, ResetAt: now.Add(-time.Minute), FetchedAt: now.Add(-time.Hour)},
	})
	p := pool.New([]*pool.Account{a}, now)
	if !windowNamed(a.QuotaWindows(), "7d-fable").Expired {
		t.Fatal("precondition: window should read Expired")
	}

	sampleQuota(p, rl, now, discardLogger())

	got, err := rl.LatestQuotaSamples(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("an Expired window must still be recorded (issue #138/#135 interaction), got %d samples", len(got))
	}
}

// This is the #135 interaction issue #138 calls out by name: a turnover
// that happens BEFORE a restart must still retire its cycle sibling AFTER
// the restart.
//
// Sequence, entirely through real components (pool, the real SQLite-backed
// reqlog.Log, sampleQuota, accounts.SeedQuota — no mocks):
//
//  1. An account's "7d" and "7d-fable" windows share a reported reset far in
//     the future. "7d-fable" is spent (Used=1, spent for fable) and hasn't
//     been re-measured in 40 hours; "7d" was measured more recently.
//  2. The sampler ticks once, recording that pre-turnover state.
//  3. A real (non-fable) response arrives reporting "7d" has turned over
//     (0.89 -> 0.0, same reported reset): Pool.RecordQuota's real header
//     parsing path retires "7d-fable" in memory (issue #135,
//     retireCycleSiblingsLocked) by moving its ResetAt to the turnover
//     moment — which, from this point on, makes it read as Expired.
//  4. The sampler ticks again. With the OLD sampler (skip Expired windows)
//     this tick would record nothing for "7d-fable", so the retirement's
//     corrected ResetAt would never reach disk.
//  5. The process "restarts": a fresh Pool and Account (no in-memory
//     windows) is built, and accounts.SeedQuota installs state from
//     quota_samples.
//
// The account must NOT come back locked out of fable: "7d-fable" must not
// seed back in as a current, spent window.
func TestSeedQuotaSurvivesASiblingTurnoverAcrossARestart(t *testing.T) {
	rl := openSampleLog(t)
	realNow := time.Now()

	reset := realNow.Add(31 * time.Hour).Truncate(time.Second) // shared, far-future reported reset
	fetchedAt7d := realNow.Add(-4 * time.Hour)
	fetchedAtFable := realNow.Add(-40 * time.Hour)
	turnoverTime := realNow.Add(-2 * time.Hour) // the turnover happened before "now" (i.e. before the coming restart)

	a := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "7d", Limit: 1, Used: 0.89, ResetAt: reset, FetchedAt: fetchedAt7d},
		{Name: "7d-fable", Limit: 1, Used: 1, ResetAt: reset, FetchedAt: fetchedAtFable},
	})
	p := pool.New([]*pool.Account{a}, realNow)

	if !a.OverThresholdForWindow("7d-fable", 0.98) {
		t.Fatal("precondition: fable must read spent before the turnover")
	}

	// Sampler tick #1: pre-turnover state hits disk. Explicit Ts, well
	// before turnoverTime, so RecordQuota's own sampleInterval throttle
	// (1 minute) does not swallow tick #2 below.
	sampleQuota(p, rl, realNow.Add(-3*time.Hour), discardLogger())

	// A real, non-fable response arrives reporting "7d" turned over. Built
	// as actual anthropic-ratelimit headers so this goes through the real
	// provider parsing + Pool.RecordQuota + setWindowsSourced +
	// retireCycleSiblingsLocked path, not a shortcut.
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.0")
	h.Set("anthropic-ratelimit-unified-7d-reset", fmt.Sprintf("%d", reset.Unix()))
	p.RecordQuota(a, h, turnoverTime)

	fable := windowNamed(a.QuotaWindows(), "7d-fable")
	if fable == nil || !fable.Expired {
		t.Fatalf("precondition: 7d-fable must be retired (Expired) in memory after the turnover, got %+v", fable)
	}
	if a.OverThresholdForWindow("7d-fable", 0.98) {
		t.Fatal("precondition: retirement should already have freed fable in memory, before any restart")
	}

	// Sampler tick #2: post-turnover state — including the now-Expired
	// 7d-fable, whose corrected ResetAt this tick is the only chance to
	// persist — hits disk. (30s after the turnover, like a real tick;
	// still well past sampleInterval since tick #1.)
	sampleQuota(p, rl, turnoverTime.Add(30*time.Second), discardLogger())

	// Simulate a daemon restart: brand new Pool and Account, no in-memory
	// windows at all, seeded purely from quota_samples.
	a2 := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	a2.Type = "claude-oauth"
	p2 := pool.New([]*pool.Account{a2}, realNow)
	accounts.SeedQuota(context.Background(), p2, rl, realNow, discardLogger())

	if a2.OverThresholdForWindow("7d-fable", 0.98) {
		t.Error("issue #138/#135 interaction: 7d-fable seeded back in as current and spent after a restart, " +
			"even though its cycle sibling demonstrably turned over before the restart — the account is " +
			"wrongly locked out of fable again")
	}
	if got := windowNamed(a2.QuotaWindows(), "7d-fable"); got != nil && !got.Expired {
		t.Errorf("7d-fable seeded as non-expired and current: %+v", got)
	}
}
