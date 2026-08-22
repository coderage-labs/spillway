package accounts

import (
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

	SeedQuota(p, l, now, quietLogger())

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

	SeedQuota(p, l, now, quietLogger())

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

	SeedQuota(p, l, now, quietLogger())

	if len(a.QuotaWindows()) != 0 {
		t.Fatalf("a sample past its reset should have been discarded, not seeded: %v", a.QuotaWindows())
	}
	if !needsProbe(a, 30*time.Minute) {
		t.Error("a stale, past-reset sample must not suppress a probe")
	}
}
