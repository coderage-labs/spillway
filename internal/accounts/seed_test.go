package accounts

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

func openTestLog(t *testing.T) *reqlog.Log {
	t.Helper()
	l, err := reqlog.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func allowOverage(v bool) *bool { return &v }

// Reproduces issue #34's actual scenario: an account that is spent, with
// extra usage permitted, whose windows were cleared by a restart (in-memory
// only). Seeding last-known state from quota_samples before the probe loop
// runs must be enough to stop needsProbe treating "no windows" as "unknown".
func TestSeedQuotaSuppressesProbeForASpentAccountWithOverage(t *testing.T) {
	l := openTestLog(t)
	now := time.Now()

	a := pool.NewAccount("spent", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetAllowOverage(allowOverage(true))
	p := pool.New([]*pool.Account{a}, now)

	// Last thing recorded before the (simulated) restart: fully spent, reset
	// still nine hours out.
	if err := l.RecordQuota(reqlog.Sample{
		Ts: now.Add(-10 * time.Minute), Account: "spent", Window: "7d",
		Limit: 1, Used: 1, ResetAt: now.Add(9 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	if len(a.QuotaWindows()) != 0 {
		t.Fatal("precondition: freshly built account should start with no windows, like after a restart")
	}

	SeedQuota(context.Background(), p, l, now, quietLogger())

	if len(a.QuotaWindows()) == 0 {
		t.Fatal("seeding did not install any windows from quota_samples")
	}
	if needsProbe(a, false, 30*time.Minute) {
		t.Error("a spent account seeded with a still-valid reading must not be probed — " +
			"that is the request issue #34 says gets billed")
	}
}

// With nothing at all in quota_samples (a genuinely new account, or one that
// has never reported quota), seeding must be a no-op and the account must
// still be discoverable: needsProbe has to stay true, or the tank never
// fills in.
func TestSeedQuotaLeavesAnAccountWithNoSamplesProbeable(t *testing.T) {
	l := openTestLog(t)
	now := time.Now()

	a := pool.NewAccount("unknown", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, now)

	SeedQuota(context.Background(), p, l, now, quietLogger())

	if len(a.QuotaWindows()) != 0 {
		t.Fatalf("seeding manufactured %d windows out of an empty quota_samples table", len(a.QuotaWindows()))
	}
	if !needsProbe(a, false, 30*time.Minute) {
		t.Error("an account with no samples at all must still be probed — unknown state must stay discoverable")
	}
}

// A sample whose reset has already passed is not current state: the window
// may have refilled. Seeding it anyway — especially as "spent" — would
// suppress a probe that should happen, the opposite failure to the one this
// fix targets. Such a sample must be discarded, leaving the account
// probeable rather than falsely settled.
func TestSeedQuotaDiscardsASampleWhoseResetHasPassed(t *testing.T) {
	l := openTestLog(t)
	now := time.Now()

	a := pool.NewAccount("stale-reset", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetAllowOverage(allowOverage(true))
	p := pool.New([]*pool.Account{a}, now)

	// Spent, and its reset was an hour ago — the window has almost
	// certainly turned over since this was recorded.
	if err := l.RecordQuota(reqlog.Sample{
		Ts: now.Add(-2 * time.Hour), Account: "stale-reset", Window: "5h",
		Limit: 1, Used: 1, ResetAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	SeedQuota(context.Background(), p, l, now, quietLogger())

	if len(a.QuotaWindows()) != 0 {
		t.Fatalf("a sample past its reset should have been discarded, not seeded: %v", a.QuotaWindows())
	}
	if !needsProbe(a, false, 30*time.Minute) {
		t.Error("a stale, past-reset sample must not suppress a probe")
	}
}

// Issue #104: SeedQuota sits on the startup path before the listeners bind,
// so a stuck or pathologically slow quota_samples read must not be able to
// hang the daemon. A context that has already expired must make SeedQuota
// return promptly, having applied nothing — exactly like an account with no
// samples at all, never a hang and never a half-applied seed.
func TestSeedQuotaRespectsAnExpiredContext(t *testing.T) {
	l := openTestLog(t)
	now := time.Now()

	a := pool.NewAccount("spent", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetAllowOverage(allowOverage(true))
	p := pool.New([]*pool.Account{a}, now)

	// Would seed successfully (see
	// TestSeedQuotaSuppressesProbeForASpentAccountWithOverage) if the
	// context did not expire first.
	if err := l.RecordQuota(reqlog.Sample{
		Ts: now.Add(-10 * time.Minute), Account: "spent", Window: "7d",
		Limit: 1, Used: 1, ResetAt: now.Add(9 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	start := time.Now()
	SeedQuota(ctx, p, l, now, quietLogger())
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("SeedQuota took %s against an already-expired context — it must return immediately, not hang", elapsed)
	}
	if len(a.QuotaWindows()) != 0 {
		t.Fatalf("an expired context must leave accounts unseeded (same as no samples), got %d windows", len(a.QuotaWindows()))
	}
	if !needsProbe(a, false, 30*time.Minute) {
		t.Error("an account that timed out during seeding must still be probeable, like a genuinely unknown one")
	}
}

// Same guarantee under an actual timeout rather than a context that is
// already dead on arrival: given a query slow enough to blow a very short
// deadline, SeedQuota must still bound its own wall-clock time to roughly
// that deadline, not to however long the query would otherwise take.
func TestSeedQuotaBoundedByTimeoutOnASlowQuery(t *testing.T) {
	l := openTestLog(t)
	now := time.Now()

	a := pool.NewAccount("acct", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, now)

	if err := l.RecordQuota(reqlog.Sample{
		Ts: now.Add(-time.Minute), Account: "acct", Window: "5h",
		Limit: 1, Used: 0.5, ResetAt: now.Add(4 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	// Let the deadline actually elapse before calling in, rather than racing it.
	time.Sleep(time.Millisecond)

	start := time.Now()
	SeedQuota(ctx, p, l, now, quietLogger())
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("SeedQuota took %s past a nanosecond timeout — a slow seed must not delay startup", elapsed)
	}
}

// Issue #138, the reported live scenario: a window's real header arrived
// long ago and nothing has re-measured it since, but the sampler keeps
// stamping a fresh Ts on the row every tick anyway. Across a restart,
// SeedQuota must install the window's real measurement age (FetchedAt), not
// the sampler's write time — otherwise the window looks as fresh as the
// restart, indefinitely, which is exactly what let it survive five restarts
// live (work's 7d-fable, header from 2026-08-31 20:04, still reading
// fetchedAt ~08:46:58 after each one).
//
// Simulated across two restart cycles, per the task's evidence bar: seed
// once, simulate more sampler ticks (still no re-measurement, Ts keeps
// advancing), seed again — the installed FetchedAt must stay pinned to the
// original measurement both times, never drifting toward "now".
func TestSeedQuotaInstallsTheWindowsRealAgeNotTheSamplerTick(t *testing.T) {
	l := openTestLog(t)
	measuredAt := time.Now().Add(-40 * time.Hour).Truncate(time.Millisecond) // the real header's own timestamp
	farFutureReset := measuredAt.Add(45 * time.Hour)

	a := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, measuredAt)

	// Restart cycle 1: the sampler has been ticking every 30s since
	// measuredAt without the window ever being re-measured. Each tick
	// writes a fresh Ts but the true FetchedAt never moves.
	sampleAt1 := measuredAt.Add(time.Hour)
	if err := l.RecordQuota(reqlog.Sample{
		Ts: sampleAt1, Account: "work", Window: "7d-fable",
		Limit: 1, Used: 1, ResetAt: farFutureReset, FetchedAt: measuredAt,
	}); err != nil {
		t.Fatal(err)
	}
	restart1 := measuredAt.Add(2 * time.Hour)
	SeedQuota(context.Background(), p, l, restart1, quietLogger())

	got := windowsByName(a.QuotaWindows())["7d-fable"]
	if got == nil {
		t.Fatal("7d-fable was not seeded")
	}
	if !got.FetchedAt.Equal(measuredAt) {
		t.Errorf("restart 1: FetchedAt = %v, want the original measurement time %v, not the sampler tick %v",
			got.FetchedAt, measuredAt, sampleAt1)
	}

	// Restart cycle 2: more ticks have passed (Ts advances further still),
	// still no re-measurement.
	sampleAt2 := measuredAt.Add(30 * time.Hour)
	if err := l.RecordQuota(reqlog.Sample{
		Ts: sampleAt2, Account: "work", Window: "7d-fable",
		Limit: 1, Used: 1, ResetAt: farFutureReset, FetchedAt: measuredAt,
	}); err != nil {
		t.Fatal(err)
	}
	a2 := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	a2.Type = "claude-oauth"
	p2 := pool.New([]*pool.Account{a2}, measuredAt)
	restart2 := measuredAt.Add(31 * time.Hour)
	SeedQuota(context.Background(), p2, l, restart2, quietLogger())

	got2 := windowsByName(a2.QuotaWindows())["7d-fable"]
	if got2 == nil {
		t.Fatal("7d-fable was not seeded on the second restart")
	}
	if !got2.FetchedAt.Equal(measuredAt) {
		t.Errorf("restart 2: FetchedAt = %v, want the original measurement time %v still, not sampler tick %v or restart time %v",
			got2.FetchedAt, measuredAt, sampleAt2, restart2)
	}
}

func windowsByName(ws []pool.QuotaWindow) map[string]*pool.QuotaWindow {
	out := make(map[string]*pool.QuotaWindow, len(ws))
	for i := range ws {
		out[ws[i].Name] = &ws[i]
	}
	return out
}
