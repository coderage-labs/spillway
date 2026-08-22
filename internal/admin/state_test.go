package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

func fetchState(t *testing.T, s *Server) stateJSON {
	t.Helper()
	front := httptest.NewServer(s)
	defer front.Close()
	req, err := authed(front.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/state = %d", resp.StatusCode)
	}
	var out stateJSON
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStateCountsEachKindOfAccount(t *testing.T) {
	ok := pool.NewAccount("ok", pool.SourceYAML, "tok", "", 0, "")
	spent := pool.NewAccount("spent", pool.SourceYAML, "tok", "", 0, "")
	paused := pool.NewAccount("paused", pool.SourceYAML, "tok", "", 0, "")
	dead := pool.NewAccount("dead", pool.SourceYAML, "tok", "", 0, "")
	p := pool.New([]*pool.Account{ok, spent, paused, dead}, time.Now())
	reset := time.Now().Add(90 * time.Minute)
	p.MarkExhausted(spent, reset)
	paused.Park()
	dead.Disable()

	s := New(p, nil, events.New(), testToken)
	st := fetchState(t, s)

	if st.Total != 4 {
		t.Errorf("total = %d, want 4", st.Total)
	}
	if st.Usable != 1 {
		t.Errorf("usable = %d, want 1 (only the healthy account)", st.Usable)
	}
	if st.Exhausted != 1 || st.Parked != 1 || st.Disabled != 1 {
		t.Errorf("breakdown = exhausted %d, parked %d, disabled %d; want 1/1/1",
			st.Exhausted, st.Parked, st.Disabled)
	}
	// A parked account is not disabled and must not be counted as one: one
	// asks the user to un-pause, the other to re-login.
	if st.Holding != nil {
		t.Errorf("holding reported with nothing parked: %+v", st.Holding)
	}
	// Something is still usable, so "when does the pool come back" is not
	// the question — omit it rather than pointing at an irrelevant reset.
	if st.NextReset != nil {
		t.Errorf("nextReset reported while %d accounts are usable", st.Usable)
	}
}

func TestStateReportsHoldAndNextReset(t *testing.T) {
	spent := pool.NewAccount("spent", pool.SourceYAML, "tok", "", 0, "")
	p := pool.New([]*pool.Account{spent}, time.Now())
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	p.MarkExhausted(spent, reset)
	release := p.BeginHold(reset)
	defer release()

	st := fetchState(t, New(p, nil, events.New(), testToken))

	if st.Holding == nil {
		t.Fatal("a parked request is not reported")
	}
	if st.Holding.Count != 1 || !st.Holding.Until.Equal(reset) {
		t.Errorf("holding = %+v, want 1 until %v", st.Holding, reset)
	}
	if st.Usable != 0 {
		t.Errorf("usable = %d, want 0", st.Usable)
	}
	if st.NextReset == nil || !st.NextReset.Equal(reset) {
		t.Errorf("nextReset = %v, want %v — with nothing usable this is the answer", st.NextReset, reset)
	}
}

func TestStateIsReadOnly(t *testing.T) {
	s, _ := newTestServer(t)
	front := httptest.NewServer(s)
	defer front.Close()
	req, err := http.NewRequest(http.MethodPost, front.URL+"/api/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/state = %d, want 405", resp.StatusCode)
	}
}

// The selector rotates away from an account whose quota headers say it is
// finished, without waiting for a 429. Counting that account as usable made
// the dashboard and status line show it as healthy while spillway avoided it.
func TestStateSeparatesReserveFromUsable(t *testing.T) {
	spent := pool.NewAccount("spent", pool.SourceYAML, "tok", "", 0, "")
	spent.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "7d", Limit: 1, Used: 1, ResetAt: time.Now().Add(14 * time.Hour), FetchedAt: time.Now()},
	})
	healthy := pool.NewAccount("healthy", pool.SourceYAML, "tok", "", 0, "")
	healthy.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "7d", Limit: 1, Used: 0.08, ResetAt: time.Now().Add(26 * time.Hour), FetchedAt: time.Now()},
	})
	p := pool.New([]*pool.Account{spent, healthy}, time.Now())
	p.SwitchThreshold = 0.98

	st := fetchState(t, New(p, nil, events.New(), testToken))
	if st.Usable != 1 || st.Reserve != 1 {
		t.Errorf("usable %d, reserve %d; want 1 and 1", st.Usable, st.Reserve)
	}
	// Not exhausted: nothing 429'd it, and conflating the two would hide
	// which accounts have actually been rejected upstream.
	if st.Exhausted != 0 {
		t.Errorf("exhausted = %d, want 0 — no 429 has happened", st.Exhausted)
	}
	// A reserve account still serves, so the pool is not dry.
	if st.NextReset != nil {
		t.Error("nextReset reported while a reserve account can still serve")
	}
}
