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
	if needsProbe(a, 30*time.Minute) {
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
	if !needsProbe(a, 30*time.Minute) {
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
	if !needsProbe(a, 30*time.Minute) {
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
	if !needsProbe(a, 30*time.Minute) {
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
