package admin

// Issue #151: /api/accounts is where `spillway status` gets the refusal it
// prints, so the endpoint has to distinguish "the provider said no" (kept on
// file as the reason, forever useful) from "spillway is currently acting on
// that no" (the bit that expires).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
)

func accountsFrom(t *testing.T, p *pool.Pool) []accountJSON {
	t.Helper()
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
	return got
}

func TestAccountsReportsALiveOverageRefusalWithItsAge(t *testing.T) {
	read := time.Now().Add(-10 * time.Minute)
	a := pool.NewAccount("refused", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetOverageAtForTest(provider.Overage{
		Known: true, Available: false, Reason: "member_zero_credit_limit",
	}, read)

	got := accountsFrom(t, pool.New([]*pool.Account{a}, time.Now()))
	if len(got) != 1 {
		t.Fatalf("got %d accounts, want 1", len(got))
	}
	if !got[0].OverageRefused {
		t.Error("overageRefused is false for a refusal ten minutes old")
	}
	if got[0].OverageReason != "member_zero_credit_limit" {
		t.Errorf("overageReason = %q, want the provider's reason", got[0].OverageReason)
	}
	if got[0].OverageCheckedAt == nil || !got[0].OverageCheckedAt.Equal(read) {
		t.Errorf("overageCheckedAt = %v, want %v — without it no surface can tell a "+
			"current refusal from one about to be re-tested", got[0].OverageCheckedAt, read)
	}
}

// The two fields part company once the refusal expires: the reason survives
// as diagnosis, the refusal stops being asserted.
func TestAccountsStopsAssertingAnExpiredRefusalButKeepsTheReason(t *testing.T) {
	a := pool.NewAccount("stale", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetOverageAtForTest(provider.Overage{
		Known: true, Available: false, Reason: "member_zero_credit_limit",
	}, time.Now().Add(-25*time.Hour))

	got := accountsFrom(t, pool.New([]*pool.Account{a}, time.Now()))
	if got[0].OverageRefused {
		t.Error("overageRefused is still true for a refusal spillway has stopped " +
			"believing — the dashboard and the selector would disagree")
	}
	if got[0].OverageReason != "member_zero_credit_limit" {
		t.Errorf("overageReason = %q, want the reason kept: it is the whole answer "+
			"to \"why is my extra usage not working\"", got[0].OverageReason)
	}
}

// An account nothing has heard from asserts nothing either way.
func TestAccountsAssertsNoRefusalForAnUnknownReading(t *testing.T) {
	a := pool.NewAccount("never-used", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"

	got := accountsFrom(t, pool.New([]*pool.Account{a}, time.Now()))
	if got[0].OverageRefused {
		t.Error("an account with no overage reading was reported as refused")
	}
	if got[0].OverageCheckedAt != nil {
		t.Errorf("overageCheckedAt = %v for a reading that never happened",
			got[0].OverageCheckedAt)
	}
}
