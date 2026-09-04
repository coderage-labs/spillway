package accounts

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func probeUpstream(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.12")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1800000000")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.06")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", "1800600000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_x"}`))
	}))
}

func TestProbeFillsQuotaForIdleAccount(t *testing.T) {
	var hits int32
	srv := probeUpstream(t, &hits)
	defer srv.Close()

	a := pool.NewAccount("idle", pool.SourceYAML, "tok", "", 0, srv.URL)
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, time.Now())

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, 0, quietLogger())

	if hits != 1 {
		t.Fatalf("expected exactly one probe request, got %d", hits)
	}
	if len(a.QuotaWindows()) == 0 {
		t.Fatal("probe did not populate quota windows")
	}
}

// The probe exists to fill a gap, not to originate traffic for its own sake:
// an account that already reported quota must not be probed.
func TestProbeSkipsAccountWithQuota(t *testing.T) {
	var hits int32
	srv := probeUpstream(t, &hits)
	defer srv.Close()

	a := pool.NewAccount("known", pool.SourceYAML, "tok", "", 0, srv.URL)
	a.Type = "claude-oauth"
	a.SetQuotaWindows([]pool.QuotaWindow{{Name: "5h", Limit: 1, Used: 0.2, FetchedAt: time.Now()}})
	p := pool.New([]*pool.Account{a}, time.Now())

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, 0, quietLogger())

	if hits != 0 {
		t.Fatalf("account with quota data must not be probed, got %d requests", hits)
	}
}

func TestProbeSkipsDisabledAccount(t *testing.T) {
	var hits int32
	srv := probeUpstream(t, &hits)
	defer srv.Close()

	a := pool.NewAccount("dead", pool.SourceYAML, "tok", "", 0, srv.URL)
	a.Type = "claude-oauth"
	a.Disable()
	p := pool.New([]*pool.Account{a}, time.Now())

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, 0, quietLogger())

	if hits != 0 {
		t.Fatalf("disabled account must not be probed, got %d requests", hits)
	}
}

// A failing probe says nothing about whether the account can serve real
// traffic, so it must never disable or otherwise penalise the account.
func TestProbeFailureLeavesAccountUsable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := pool.NewAccount("flaky", pool.SourceYAML, "tok", "", 0, srv.URL)
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, time.Now())

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, 0, quietLogger())

	if a.State() != pool.StateOK {
		t.Fatalf("probe failure changed account state to %v", a.State())
	}
}

func TestProbeModelPerProvider(t *testing.T) {
	claude := pool.NewAccount("c", pool.SourceYAML, "t", "", 0, "")
	claude.Type = "claude-oauth"
	if got := probeModel(claude); got != "claude-haiku-4-5-20251001" {
		t.Errorf("claude probe model: %q", got)
	}

	kimi := pool.NewAccount("k", pool.SourceYAML, "t", "", 0, "")
	kimi.Type = "kimi-oauth"
	kimi.ModelMap = map[string]string{"claude-haiku-4-5-20251001": "kimi-for-coding"}
	if got := probeModel(kimi); got != "kimi-for-coding" {
		t.Errorf("kimi probe model should follow modelMap, got %q", got)
	}

	bare := pool.NewAccount("k2", pool.SourceYAML, "t", "", 0, "")
	bare.Type = "kimi-oauth"
	if got := probeModel(bare); got != "k3" {
		t.Errorf("kimi fallback model: %q", got)
	}
}

// Staleness drives re-probing: a reading from hours ago is as useless as none
// on a standby account, which is what made a tank sit blank overnight.
func TestNeedsProbe(t *testing.T) {
	fresh := pool.NewAccount("fresh", pool.SourceYAML, "t", "", 0, "")
	fresh.SetQuotaWindows([]pool.QuotaWindow{{Name: "5h", Limit: 1, Used: 0.2, FetchedAt: time.Now()}})
	stale := pool.NewAccount("stale", pool.SourceYAML, "t", "", 0, "")
	stale.SetQuotaWindows([]pool.QuotaWindow{{Name: "5h", Limit: 1, Used: 0.2,
		FetchedAt: time.Now().Add(-2 * time.Hour)}})
	blank := pool.NewAccount("blank", pool.SourceYAML, "t", "", 0, "")

	if needsProbe(fresh, false, 30*time.Minute) {
		t.Error("a fresh reading should not be re-probed")
	}
	if !needsProbe(stale, false, 30*time.Minute) {
		t.Error("a stale reading should be re-probed")
	}
	if !needsProbe(blank, false, 30*time.Minute) {
		t.Error("no reading at all should always be probed")
	}
	// Interval 0 = startup-only: never re-probe something we already have.
	if needsProbe(stale, false, 0) {
		t.Error("interval 0 should disable staleness re-probing")
	}
	if !needsProbe(blank, false, 0) {
		t.Error("interval 0 must still fill an account with no reading")
	}
}

// A 401 means the stored token was superseded, not that the account is dead:
// recover and retry rather than leaving the tank blank.
func TestProbeRetriesAfterUnauthorized(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.25")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := pool.NewAccount("stale-token", pool.SourceYAML, "tok", "refresh", 0, srv.URL)
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, time.Now())
	// Recovery is what makes the retry worth attempting; without a manager
	// wired the probe correctly gives up, so the test needs one.
	recovered := 0
	p.SetTokenManager(fakeTokens{onRecover: func() { recovered++ }})

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, 0, quietLogger())

	if recovered != 1 {
		t.Errorf("expected exactly one recovery attempt, got %d", recovered)
	}

	if hits < 2 {
		t.Fatalf("a 401 should trigger recovery and one retry, got %d requests", hits)
	}
	if len(a.QuotaWindows()) == 0 {
		t.Error("retry after recovery should have populated quota")
	}
}

// fakeTokens stands in for accounts.Manager so the probe's recovery path can
// be exercised without real credentials.
type fakeTokens struct{ onRecover func() }

func (f fakeTokens) EnsureFresh(context.Context, *pool.Account) error { return nil }
func (f fakeTokens) Recover(context.Context, *pool.Account) error {
	if f.onRecover != nil {
		f.onRecover()
	}
	return nil
}

// A probe must never be a purchase.
//
// Live case (2026-08-22): an account with its 7-day window fully consumed and
// extra usage enabled. Anthropic answers the probe with 200 and a charge, so
// `probeOnStart` plus a 30m `probeInterval` bought a request every half hour
// to re-read a number already on file.
func TestProbeSkipsAnAccountWhereItWouldBeBilled(t *testing.T) {
	now := time.Now()
	a := pool.NewAccount("spent", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	// Extra usage permitted: this is what makes the probe a purchase rather
	// than a free refusal (issue #152). Without it the provider answers 429
	// and the probe should go ahead — see
	// TestSpentAccountIsProbedWhenTheProbeCannotBeBilled.
	yes := true
	a.SetAllowOverage(&yes)
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0, ResetAt: now.Add(2 * time.Hour), FetchedAt: now.Add(-6 * time.Hour)},
		{Name: "7d", Limit: 1, Used: 1, ResetAt: now.Add(9 * time.Hour), FetchedAt: now.Add(-6 * time.Hour)},
	})
	// Stale enough that it would otherwise be probed on this sweep.
	if !needsProbeIgnoringCost(a, 30*time.Minute) {
		t.Fatal("precondition: this account is not stale, so the test proves nothing")
	}
	if needsProbe(a, false, 30*time.Minute) {
		t.Error("probed an account whose quota is gone — on extra usage that is a charge, " +
			"and it re-reads a reset time already on file")
	}
}

// Once the window has actually reset, probing is free again and is the only
// way to learn the account has refilled.
func TestProbeResumesAfterTheWindowResets(t *testing.T) {
	now := time.Now()
	a := pool.NewAccount("refilled", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "7d", Limit: 1, Used: 1, ResetAt: now.Add(-time.Minute), FetchedAt: now.Add(-6 * time.Hour)},
	})
	if !needsProbe(a, false, 30*time.Minute) {
		t.Error("never probed again after the reset passed — the account would show " +
			"as spent forever")
	}
}

// A healthy account is unaffected: this must not become a reason to stop
// refreshing tanks.
func TestProbeStillRunsForAHealthyStaleAccount(t *testing.T) {
	now := time.Now()
	a := pool.NewAccount("healthy", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "7d", Limit: 1, Used: 0.4, ResetAt: now.Add(9 * time.Hour), FetchedAt: now.Add(-6 * time.Hour)},
	})
	if !needsProbe(a, false, 30*time.Minute) {
		t.Error("stopped probing a healthy account")
	}
}

// needsProbeIgnoringCost is the staleness half of needsProbe, used only to
// assert the precondition above.
func needsProbeIgnoringCost(a *pool.Account, staleAfter time.Duration) bool {
	newest := time.Time{}
	for _, w := range a.QuotaWindows() {
		if w.FetchedAt.After(newest) {
			newest = w.FetchedAt
		}
	}
	return time.Since(newest) > staleAfter
}
