package accounts

// Issue #152: a spent window was never re-probed before the reset its own
// headers reported, so an out-of-band refill went unnoticed for days.
//
// Live case (2026-09-04): Anthropic ran an ad-hoc reset for every user,
// outside the reset times its headers had given. Three accounts sat at 7d =
// 1.00 with reported resets of Sep 5, Sep 6 and Sep 9. The quota had really
// refilled — confirmed in the Claude app — and nothing in spillway could
// find out: the probe skipped them, no proxied traffic reached them (a
// fourth account had headroom and took everything), #137's expiry could not
// fire with each ResetAt still ahead, and a restart changed nothing because
// probeOnStart goes through the same needsProbe.
//
// Two defects, tested separately below:
//
//  1. wouldBill never asked whether billing was possible. With allowOverage
//     off — the default — the provider answers a probe on a spent account
//     with a free 429, so the guard was declining a free request to avoid a
//     charge that could not occur.
//  2. "the stored reading already says what a probe would" is false for an
//     out-of-band reset (and for #135's reset lag). Where overage IS on, the
//     caution stays but is bounded by billedProbeAge.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
)

// spentAccount is the shape the live incident left behind: the weekly window
// fully consumed, its reported reset still a day away, and the reading old
// enough that an ordinary sweep would otherwise re-measure it.
func spentAccount(t *testing.T, name, upstream string) *pool.Account {
	t.Helper()
	now := time.Now()
	a := pool.NewAccount(name, pool.SourceYAML, "tok", "", 0, upstream)
	a.Type = "claude-oauth"
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.1, Source: "headers",
			ResetAt: now.Add(2 * time.Hour), FetchedAt: now.Add(-6 * time.Hour)},
		{Name: "7d", Limit: 1, Used: 1.0, Source: "headers",
			ResetAt: now.Add(24 * time.Hour), FetchedAt: now.Add(-6 * time.Hour)},
	})
	return a
}

func boolPtr(v bool) *bool { return &v }

// refilledUpstream answers the way the provider did after the ad-hoc reset:
// the weekly window is no longer spent.
func refilledUpstream(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.04")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1800000000")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.06")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", "1800600000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_x"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The headline. Extra usage off — the default, and the only setting
// documented as spending money — so the provider cannot charge for this
// probe: it refuses with a 429 and hands back current quota headers. The
// probe must go ahead.
func TestSpentAccountIsProbedWhenTheProbeCannotBeBilled(t *testing.T) {
	var hits atomic.Int32
	srv := refilledUpstream(t, &hits)

	a := spentAccount(t, "spent-free", srv.URL)
	p := pool.New([]*pool.Account{a}, time.Now())
	if p.AllowOverage() {
		t.Fatal("precondition: pool.allowOverage must be off (the default)")
	}
	if a.CanOverage(p.AllowOverage()) {
		t.Fatal("precondition: this account must not be able to bill, or the test proves nothing")
	}
	if !readsSpent(a, time.Now()) {
		t.Fatal("precondition: the stored reading must say the probe's own window is spent")
	}

	if wouldBill(a, p.AllowOverage(), time.Now()) {
		t.Error("wouldBill = true for an account that cannot spend: the provider answers 429, free")
	}
	if !needsProbe(a, p.AllowOverage(), 30*time.Minute) {
		t.Fatal("a spent account whose probe cannot be charged was skipped — this is the whole of issue #152")
	}

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, 30*time.Minute, quietLogger())
	if got := hits.Load(); got != 1 {
		t.Fatalf("probe requests = %d, want exactly 1", got)
	}
}

// The rule this guard exists for. Same account, extra usage explicitly
// enabled, so the provider answers 200 and charges. Before its reset, and
// inside the age cap, it must not be probed.
func TestSpentAccountIsNotProbedWhenTheProbeWouldBeBilled(t *testing.T) {
	var hits atomic.Int32
	srv := refilledUpstream(t, &hits)

	a := spentAccount(t, "spent-billable", srv.URL)
	a.SetAllowOverage(boolPtr(true))
	p := pool.New([]*pool.Account{a}, time.Now())

	if !wouldBill(a, p.AllowOverage(), time.Now()) {
		t.Fatal("wouldBill = false for a spent account with extra usage enabled: that probe is a purchase")
	}
	if needsProbe(a, p.AllowOverage(), 30*time.Minute) {
		t.Error("probed an account that would be charged for it, before its reset and inside the age cap")
	}

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, 30*time.Minute, quietLogger())
	if got := hits.Load(); got != 0 {
		t.Fatalf("probe requests = %d, want 0: never spend uninvited", got)
	}
}

// The pool-wide default is the other way in to a charge, and must suppress
// the probe exactly as a per-account opt-in does. CanOverage only follows
// the pool default once the provider has said extra usage is actually
// available, so the test arranges both halves.
func TestSpentAccountIsNotProbedWhenThePoolDefaultPaysForIt(t *testing.T) {
	var hits atomic.Int32
	srv := refilledUpstream(t, &hits)

	a := spentAccount(t, "spent-pool-pays", srv.URL)
	a.SetOverageForTest(provider.Overage{Known: true, Available: true, Utilization: 0.1})
	p := pool.New([]*pool.Account{a}, time.Now())
	p.Apply(pool.Settings{AllowOverage: true})

	if !a.CanOverage(p.AllowOverage()) {
		t.Fatal("precondition: pool.allowOverage plus available extra usage should permit spending")
	}
	if needsProbe(a, p.AllowOverage(), 30*time.Minute) {
		t.Error("pool.allowOverage did not suppress a probe that would be charged")
	}

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, 30*time.Minute, quietLogger())
	if got := hits.Load(); got != 0 {
		t.Fatalf("probe requests = %d, want 0", got)
	}
}

// End to end, the live scenario: a stored 7d reading of 1.00 with a reset a
// day away, an ad-hoc reset the headers never predicted, and one probe sweep
// that has to find it. Afterwards the account must be back in ordinary
// rotation rather than deprioritised for another day.
func TestAdHocResetIsFoundByTheProbeAndRestoresRotation(t *testing.T) {
	var hits atomic.Int32
	srv := refilledUpstream(t, &hits)

	a := spentAccount(t, "refilled", srv.URL)
	p := pool.New([]*pool.Account{a}, time.Now())

	if !a.OverThreshold(p.Threshold()) {
		t.Fatal("precondition: a 1.00 weekly window should be over the rotation threshold")
	}

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, 30*time.Minute, quietLogger())

	if got := hits.Load(); got != 1 {
		t.Fatalf("probe requests = %d, want 1: the sweep never re-measured the spent account", got)
	}
	var weekly float64 = -1
	for _, w := range a.QuotaWindows() {
		if w.Name == "7d" {
			weekly = w.Used / w.Limit
		}
	}
	if weekly < 0 {
		t.Fatal("no 7d window after the probe")
	}
	if weekly > 0.5 {
		t.Errorf("7d still reads %.2f spent: the probe response did not replace the stale reading", weekly)
	}
	if a.OverThreshold(p.Threshold()) {
		t.Error("account still over the rotation threshold after a probe showing it refilled")
	}
	if got := p.Select("s1"); got != a {
		t.Errorf("Select returned %v, want the refilled account back in normal rotation", got)
	}
}

// probeOnStart is the escape hatch users reach for, and it did nothing: the
// startup sweep passes staleAfter 0, and SeedQuota has already installed
// last-known state, so every account looked settled. A seeded reading that
// says "spent" is precisely the one a restart should re-measure — nothing
// else can correct it, and here it costs nothing.
func TestProbeOnStartRemeasuresASpentSeededAccount(t *testing.T) {
	var hits atomic.Int32
	srv := refilledUpstream(t, &hits)

	a := spentAccount(t, "seeded-spent", srv.URL)
	p := pool.New([]*pool.Account{a}, time.Now())

	// staleAfter 0 is exactly what cmd/spillway passes for probeOnStart.
	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, 0, quietLogger())

	if got := hits.Load(); got != 1 {
		t.Fatalf("startup probe requests = %d, want 1: restarting the daemon must re-measure a spent reading", got)
	}
}

// ...and must not turn the startup sweep into "probe everything". A seeded
// account with headroom is still settled: its reading is believable, and one
// request per account per restart is traffic nobody asked for.
func TestProbeOnStartStillSkipsAHealthySeededAccount(t *testing.T) {
	var hits atomic.Int32
	srv := refilledUpstream(t, &hits)

	now := time.Now()
	a := pool.NewAccount("seeded-healthy", pool.SourceYAML, "tok", "", 0, srv.URL)
	a.Type = "claude-oauth"
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "7d", Limit: 1, Used: 0.4, Source: "headers",
			ResetAt: now.Add(24 * time.Hour), FetchedAt: now.Add(-6 * time.Hour)},
	})
	p := pool.New([]*pool.Account{a}, now)

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, 0, quietLogger())

	if got := hits.Load(); got != 0 {
		t.Fatalf("startup probe requests = %d, want 0 for an account with a believable reading", got)
	}
}

// Issue #34, which must not come back: one charge per daemon restart. The
// Homebrew cask restarts the service on every upgrade, so a startup sweep
// that buys a request is a recurring bill.
func TestStartupProbeStillNeverBillsASpentSeededAccount(t *testing.T) {
	var hits atomic.Int32
	srv := refilledUpstream(t, &hits)

	a := spentAccount(t, "seeded-billable", srv.URL)
	a.SetAllowOverage(boolPtr(true))
	p := pool.New([]*pool.Account{a}, time.Now())

	ProbeIdle(context.Background(), p, srv.Client(), srv.URL, 0, quietLogger())

	if got := hits.Load(); got != 0 {
		t.Fatalf("startup probe requests = %d, want 0: issue #34's charge-per-restart is back", got)
	}
}

// A seed from a row written before #138 added fetched_at carries the zero
// time. That is an UNKNOWN measurement time, not an infinitely old one — and
// spending against it would buy a request on every restart, which is issue
// #34 verbatim. The age cap must refuse to fire on it.
func TestAgeCapNeverSpendsAgainstAnUnknownFetchedAt(t *testing.T) {
	now := time.Now()
	a := pool.NewAccount("no-fetched-at", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetAllowOverage(boolPtr(true))
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "7d", Limit: 1, Used: 1.0, Source: "headers", ResetAt: now.Add(24 * time.Hour)},
	})

	if needsProbe(a, false, 30*time.Minute) {
		t.Error("the age cap fired on a window with no measurement time at all — that is a charge per restart")
	}
}

// Where the probe IS billed the caution stays, but the reported reset is not
// treated as the only way a window can refill. After billedProbeAge on one
// unchanged reading, one probe is bought: a single request costs far less
// than days of wrongly-deprioritised capacity.
func TestBilledProbeIsBoughtOnceTheReadingIsOldEnough(t *testing.T) {
	now := time.Now()
	mk := func(age time.Duration) *pool.Account {
		a := pool.NewAccount("aged", pool.SourceYAML, "tok", "", 0, "")
		a.Type = "claude-oauth"
		a.SetAllowOverage(boolPtr(true))
		a.SetQuotaWindows([]pool.QuotaWindow{
			// Reset five days out, as the Sep 9 account's was: waiting for
			// it is exactly the failure being fixed.
			{Name: "7d", Limit: 1, Used: 1.0, Source: "headers",
				ResetAt: now.Add(5 * 24 * time.Hour), FetchedAt: now.Add(-age)},
		})
		return a
	}

	ageCap := billedProbeAge(30 * time.Minute)
	if needsProbe(mk(ageCap-time.Hour), false, 30*time.Minute) {
		t.Error("bought a probe before the age cap: inside it, the stored reading is what we go on")
	}
	if !needsProbe(mk(ageCap+time.Hour), false, 30*time.Minute) {
		t.Error("never bought a probe past the age cap: the account stays wrongly spent until its reported reset")
	}
}

// The cap scales with the configured probe cadence but is clamped at both
// ends, so no interval turns "about once a day" into "every few minutes" or
// "not this week".
func TestBilledProbeAgeIsClamped(t *testing.T) {
	cases := []struct {
		staleAfter time.Duration
		want       time.Duration
	}{
		{0, 24 * time.Hour},                // startup-only: no cadence to scale from
		{-time.Minute, 24 * time.Hour},     // nonsense input takes the ceiling too
		{time.Minute, 6 * time.Hour},       // 48m would be far too eager: floored
		{30 * time.Minute, 24 * time.Hour}, // the default: one billed probe a day
		{20 * time.Minute, 16 * time.Hour}, // in between, scaled
		{6 * time.Hour, 24 * time.Hour},    // 12 days would be worse than the reset: ceilinged
		{9999 * time.Hour, 24 * time.Hour}, // and further past it
		// Absurd but arithmetically dangerous: 48 x this overflows int64 and
		// comes back negative, which reads as "far too eager" and floors to
		// 6h — a shorter wait from a longer setting, on the money path.
		{100000 * time.Hour, 24 * time.Hour},
	}
	for _, c := range cases {
		if got := billedProbeAge(c.staleAfter); got != c.want {
			t.Errorf("billedProbeAge(%v) = %v, want %v", c.staleAfter, got, c.want)
		}
	}
}

// The canary asks one question — did the credential authenticate — and a 429
// answers it as well as a 200 does. Skipping a spent account that cannot
// bill left the exact accounts most in need of a health check unchecked.
func TestCanaryChecksASpentAccountThatCannotBill(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "1.0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	a := spentAccount(t, "spent-canary", srv.URL)
	p := pool.New([]*pool.Account{a}, time.Now())
	c := &Canary{Pool: p, Client: srv.Client(), Upstream: srv.URL, Logger: quietLogger()}

	c.Run(context.Background())
	if got := hits.Load(); got != 1 {
		t.Errorf("canary requests = %d, want 1: a free refusal still proves the credential works", got)
	}
}

// ...and still never buys one.
func TestCanaryStillSkipsAnAccountItWouldBeBilledFor(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := spentAccount(t, "billable-canary", srv.URL)
	a.SetAllowOverage(boolPtr(true))
	p := pool.New([]*pool.Account{a}, time.Now())
	c := &Canary{Pool: p, Client: srv.Client(), Upstream: srv.URL, Logger: quietLogger()}

	c.Run(context.Background())
	if got := hits.Load(); got != 0 {
		t.Errorf("canary requests = %d, want 0: a canary must never be a purchase", got)
	}
}
