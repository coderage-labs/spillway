package admin

// Issue #135: a spent window past its reset is unknown, not spent — and the
// dashboard's bits must say so the same way the selector does.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

// Same account as TestFableOnlySpendDoesNotMarkAccountOverThreshold, but the
// fable window's reset has passed: FableSpent clears, and the window is still
// listed, flagged expired, rather than vanishing.
func TestExpiredFableWindowIsNotFableSpent(t *testing.T) {
	a := pool.NewAccount("fable-expired", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "7d-fable", Limit: 1, Used: 1, ResetAt: time.Now().Add(-time.Minute), FetchedAt: time.Now().Add(-time.Hour)},
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
	if acct.FableSpent {
		t.Error("FableSpent = true for a fable window whose reset has passed (issue #135)")
	}
	if len(acct.Windows) != 1 || !acct.Windows[0].Expired {
		t.Errorf("windows = %+v, want the 7d-fable row present and flagged expired", acct.Windows)
	}
}

// TestStateSeparatesReserveFromUsable with the spent account's reset in the
// past: it is usable again, because nothing current says it is spent.
func TestStateCountsExpiredReserveAsUsable(t *testing.T) {
	spent := pool.NewAccount("spent", pool.SourceYAML, "tok", "", 0, "")
	spent.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "7d", Limit: 1, Used: 1, ResetAt: time.Now().Add(-time.Minute), FetchedAt: time.Now().Add(-time.Hour)},
	})
	healthy := pool.NewAccount("healthy", pool.SourceYAML, "tok", "", 0, "")
	healthy.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "7d", Limit: 1, Used: 0.08, ResetAt: time.Now().Add(26 * time.Hour), FetchedAt: time.Now()},
	})
	p := pool.New([]*pool.Account{spent, healthy}, time.Now())
	p.Apply(pool.Settings{SwitchThreshold: 0.98})

	st := fetchState(t, New(p, nil, events.New(), testToken))
	if st.Usable != 2 || st.Reserve != 0 {
		t.Errorf("usable %d, reserve %d; want 2 and 0 — an expired spent reading is not a reserve", st.Usable, st.Reserve)
	}
}
