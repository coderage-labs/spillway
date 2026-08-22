package proxy

// End-to-end for the one tier that costs money.
//
// The pool tests prove selection. These prove the whole path: a request
// arrives, every free account is spent, and the request is served by the
// billable account AND recorded as such. The recording is the point — the
// defence against silent spending is that it is never silent.

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// overageProxyRig: a spent account that can bill, and a dead-spent one that
// cannot, behind a real handler and a real upstream.
func overageProxyRig(t *testing.T, allow bool) (*httptest.Server, *pool.Pool, *reqlog.Log, *pool.Account) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Overage-Status", "allowed")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","content":[]}`))
	}))
	t.Cleanup(up.Close)

	now := time.Now()
	billable := pool.NewAccount("billable", pool.SourceYAML, "tok", "", 0, "")
	billable.Type = "claude-oauth"
	billable.SetOverageForTest(provider.Overage{Known: true, Available: true})
	p := pool.New([]*pool.Account{billable}, now)
	p.Apply(pool.Settings{AllowOverage: allow})
	p.MarkExhausted(billable, now.Add(4*time.Hour))

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "fail" // never hold: we want the decision, not a wait
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rl.Close() })
	h.SetHooks(Hooks{Log: rl, Events: events.New()})

	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	return front, p, rl, billable
}

// Enabled: the request is served, and the log says it was billed.
func TestOverageRequestIsServedAndRecordedAsBilled(t *testing.T) {
	front, _, rl, billable := overageProxyRig(t, true)

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the billable account should have served it",
			resp.StatusCode)
	}

	e := waitForEntry(t, rl)
	if e.Account != billable.Name {
		t.Errorf("served by %q, want the billable account", e.Account)
	}
	// The whole defence against silent spending: this is never logged as an
	// ordinary served request.
	if e.Event != reqlog.EventOverage {
		t.Errorf("event = %q, want %q — a billed request logged as a normal one "+
			"is a charge the user has no way to find", e.Event, reqlog.EventOverage)
	}
}

// Disabled (the default): the same request must fail rather than quietly buy
// its way through.
func TestOverageDisabledFailsInsteadOfSpending(t *testing.T) {
	front, _, rl, _ := overageProxyRig(t, false)

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 — with overage off, an exhausted pool must "+
			"refuse rather than bill", resp.StatusCode)
	}
	e := waitForEntry(t, rl)
	if e.Event == reqlog.EventOverage {
		t.Error("a request was billed with overage disabled")
	}
	if e.Event != reqlog.EventExhausted {
		t.Errorf("event = %q, want %q", e.Event, reqlog.EventExhausted)
	}
}

// The upstream's own headers must update what we believe about the account,
// so a revoked credit limit takes effect on the next request rather than
// after a restart.
func TestOverageStateIsRefreshedFromTheResponse(t *testing.T) {
	front, _, _, billable := overageProxyRig(t, true)
	resp := postMessages(t, front.URL, testBody)
	resp.Body.Close()

	if ov := billable.Overage(); !ov.Known || !ov.Available {
		t.Errorf("overage = %+v after a response saying allowed", ov)
	}
}

// The commonest billed request is NOT from a 429'd account.
//
// An account spillway has been rotating away from correctly has never been
// rejected: its quota headers say it is finished, its state is still OK. The
// first version of this detection tested StateExhausted and so logged exactly
// that case as ordinary served traffic — a charge with nothing in the record
// to find it by.
func TestOverThresholdAccountIsRecordedAsBilledWithoutA429(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Overage-Status", "allowed")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","content":[]}`))
	}))
	defer up.Close()

	now := time.Now()
	// Quota gone by the headers, but nothing has rejected it.
	billable := pool.NewAccount("spent-not-rejected", pool.SourceYAML, "tok", "", 0, "")
	billable.Type = "claude-oauth"
	billable.SetAllowOverage(func(b bool) *bool { return &b }(true))
	billable.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "7d", Limit: 1, Used: 1, ResetAt: now.Add(9 * time.Hour), FetchedAt: now},
	})
	p := pool.New([]*pool.Account{billable}, now)
	p.Apply(pool.Settings{SwitchThreshold: 0.98, AllowOverage: true})

	if billable.State() != pool.StateOK {
		t.Fatalf("precondition: state is %v, want StateOK — the point is that "+
			"nothing has 429'd it", billable.State())
	}
	if !p.WouldBill(billable) {
		t.Fatal("precondition: the pool does not think this would bill")
	}

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "fail"
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()
	h.SetHooks(Hooks{Log: rl, Events: events.New()})
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if e := waitForEntry(t, rl); e.Event != reqlog.EventOverage {
		t.Errorf("event = %q, want %q — a billed request logged as ordinary "+
			"traffic is a charge with no record to find it by", e.Event, reqlog.EventOverage)
	}
}

// The provider says a request was billed after spillway judged it free.
//
// This is not hypothetical: it is what the first live test did. The status
// header carried a value the parser did not recognise, so selection saw a
// free account, and a billed request was recorded as ordinary traffic. The
// response's own Overage-In-Use must override the prediction, because a
// prediction can be wrong and this cannot.
func TestInUseHeaderOverridesAFreePrediction(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Overage-Status at all — the account looks entirely free, which
		// is precisely the case a prediction gets wrong.
		w.Header().Set("Anthropic-Ratelimit-Unified-Overage-In-Use", "true")
		w.Header().Set("Anthropic-Ratelimit-Unified-Overage-Utilization", "0.98")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","content":[]}`))
	}))
	defer up.Close()

	// A completely healthy account: nothing about it predicts billing.
	a := pool.NewAccount("healthy", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, time.Now())
	if p.WouldBill(a) {
		t.Fatal("precondition: the pool already predicts billing, so this proves nothing")
	}

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()
	h.SetHooks(Hooks{Log: rl, Events: events.New()})
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, testBody)
	resp.Body.Close()

	if e := waitForEntry(t, rl); e.Event != reqlog.EventOverage {
		t.Errorf("event = %q, want %q — the provider said this was billed and "+
			"spillway recorded it as ordinary traffic", e.Event, reqlog.EventOverage)
	}
}
