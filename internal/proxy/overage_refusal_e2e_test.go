package proxy

// Issue #151 end to end: the live 2026-09-04 scenario, whole.
//
// The user enabled a credit limit in the Anthropic console, spent extra
// usage, disabled the limit, then re-enabled it. spillway had cached
// "overage-disabled-reason: member_zero_credit_limit" and served 429s for
// hours with allowOverage correctly on, because the cached refusal
// suppressed the only request that could have replaced it.
//
// Here the same pool meets the same upstream, except the upstream has since
// changed its mind — and the only thing standing between the two is how old
// the cached refusal is.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// refusalRig: one opted-in account, out of quota, carrying a cached
// provider refusal of the given age. The upstream behind it now permits
// extra usage again — exactly the state the console change produced — so
// whether the request ever reaches it is entirely the refusal's age.
func refusalRig(t *testing.T, age time.Duration) (*httptest.Server, *pool.Pool, *reqlog.Log, *pool.Account) {
	t.Helper()
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		// The user re-enabled their credit limit: extra usage is permitted
		// again, and nothing in spillway knows it yet.
		w.Header().Set("Anthropic-Ratelimit-Unified-Overage-Status", "allowed_warning")
		w.Header().Set("Anthropic-Ratelimit-Unified-Overage-In-Use", "true")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","content":[]}`))
	}))
	t.Cleanup(up.Close)

	now := time.Now()
	a := pool.NewAccount("opted-in", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetAllowOverage(boolp151(true)) // the operator's standing "spend on this one"
	a.SetOverageAtForTest(provider.Overage{
		Known: true, Available: false, Reason: "member_zero_credit_limit",
	}, now.Add(-age))

	p := pool.New([]*pool.Account{a}, now)
	p.MarkExhausted(a, now.Add(48*time.Hour)) // the 7d window, days out

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "fail" // the decision, not a wait
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
	return front, p, rl, a
}

func boolp151(b bool) *bool { return &b }

// The fix, whole. A refusal older than the TTL stops suppressing selection,
// the request actually goes out, the fresh header says extra usage is
// permitted, and normal service resumes — no daemon restart.
func TestStaleRefusalLetsTheRequestOutAndAFreshHeaderRestoresService(t *testing.T) {
	front, _, rl, a := refusalRig(t, 90*time.Minute)

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (%s), want 200 — the cached refusal is still blocking "+
			"the request that would lift it (issue #151)", resp.StatusCode, body)
	}

	e := waitForEntry(t, rl)
	if e.Account != a.Name {
		t.Errorf("served by %q, want the opted-in account", e.Account)
	}
	// Still never silent about money: this is a billed request and is logged
	// as one.
	if e.Event != reqlog.EventOverage {
		t.Errorf("event = %q, want %q", e.Event, reqlog.EventOverage)
	}

	// The header the request brought back has replaced the stale refusal.
	ov := a.Overage()
	if !ov.Known || !ov.Available {
		t.Fatalf("overage after the round trip = %+v, want known and available", ov)
	}
	if _, refused := a.OverageRefusal(); refused {
		t.Error("still reporting a refusal after the provider permitted extra usage")
	}
	// And the account is now available on its own merits rather than on an
	// expiry, so it keeps serving.
	resp2 := postMessages(t, front.URL, testBody)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("second request status = %d, want 200 — the recovery did not stick",
			resp2.StatusCode)
	}
}

// The other side of the same rig: a refusal spillway heard minutes ago is
// obeyed, the request never leaves, and the 429 now says why.
func TestFreshRefusalStillFailsFastAndTheBodyNamesTheReason(t *testing.T) {
	front, _, _, _ := refusalRig(t, 2*time.Minute)

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 — a fresh provider refusal must still be "+
			"believed, or a genuinely refusing provider gets hammered", resp.StatusCode)
	}

	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	// The whole answer to "why is my extra usage not working", in the one
	// place the user is definitely looking (issue #151).
	if !strings.Contains(body.Error.Message, "member_zero_credit_limit") {
		t.Errorf("429 message = %q, want it to name the provider's refusal reason",
			body.Error.Message)
	}
	if !strings.Contains(body.Error.Message, "all accounts exhausted") {
		t.Errorf("429 message = %q, want it to keep saying the pool is exhausted",
			body.Error.Message)
	}
}

// With no refusal in play the message is unchanged — the reason is an
// addition for the case that has one, not a new permanent suffix.
func TestExhaustedBodyIsUnchangedWithNoRefusal(t *testing.T) {
	now := time.Now()
	a := pool.NewAccount("plain", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, now)
	p.MarkExhausted(a, now.Add(4*time.Hour))

	cfg := config.Defaults()
	cfg.Upstream = "http://127.0.0.1:1" // must never be reached
	cfg.Pool.ExhaustedMode = "fail"
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if got, want := string(raw),
		`{"type":"error","error":{"type":"rate_limit_error","message":"spillway: all accounts exhausted"}}`; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

// The notification body carries the reason too, and still names no account
// (issue #101: a leaked ntfy topic must not be worth reading).
func TestExhaustedNotificationCarriesTheReasonButNoAccountName(t *testing.T) {
	now := time.Now()
	a := pool.NewAccount("very-identifying-name", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetOverageForTest(provider.Overage{
		Known: true, Available: false, Reason: "member_zero_credit_limit",
	})
	p := pool.New([]*pool.Account{a}, now)
	p.MarkExhausted(a, now.Add(4*time.Hour))

	msg := exhaustedMessage(p)
	if !strings.Contains(msg, "member_zero_credit_limit") {
		t.Errorf("exhaustedMessage = %q, want the refusal reason", msg)
	}
	if strings.Contains(msg, "very-identifying-name") {
		t.Errorf("exhaustedMessage = %q leaked an account name into a notification", msg)
	}
}
