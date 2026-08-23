package admin

// Issue #24 decision 3: a model-specific window must not mark the whole
// account spent. OverThreshold, the dashboard badge and the status line must
// agree with the selector's per-family logic, not the old "any window" scan
// — otherwise the UI says an account is done when the selector would still
// happily serve it Sonnet or Opus traffic.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

// An account spent only on its fable bucket must show as OverThreshold=false
// (it is fine for ordinary traffic) and FableSpent=true (naming the family
// that actually is spent, per decision 3), and must count as Usable rather
// than Reserve in the pool-wide state.
func TestFableOnlySpendDoesNotMarkAccountOverThreshold(t *testing.T) {
	a := pool.NewAccount("fable-only", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "7d-fable", Limit: 1, Used: 1, FetchedAt: time.Now()},
	})
	p := pool.New([]*pool.Account{a}, time.Now())
	p.Apply(pool.Settings{SwitchThreshold: 0.98})
	s := New(p, nil, events.New(), testToken)

	front := httptest.NewServer(s)
	defer front.Close()

	req, err := authed(front.URL + "/api/accounts")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got []accountJSON
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("accounts = %+v", got)
	}
	acct := got[0]
	if acct.OverThreshold {
		t.Error("OverThreshold = true for an account spent only on fable — it is fine for Sonnet/Opus")
	}
	if !acct.FableSpent {
		t.Error("FableSpent = false, want true — the 7d-fable window is spent")
	}

	st := fetchState(t, New(p, nil, events.New(), testToken))
	if st.Usable != 1 {
		t.Errorf("usable = %d, want 1 — fable-only spend must not push an account into Reserve", st.Usable)
	}
	if st.Reserve != 0 {
		t.Errorf("reserve = %d, want 0", st.Reserve)
	}
}
