package admin

// Issue #194: the QuotaWindow MarkWindowRejected forges (Used/Limit = 1.0)
// used to be stamped Source "headers", so on /api/accounts — the dashboard's
// only account-level view of quota — the exclusion restating itself was
// byte-for-byte identical to a reading a provider had actually measured.
//
// The dashboard's own rendering of it is asserted in testdata/ui_dom_test.js
// ("forged rejection row not shown as measured"), which SKIPS where node is
// unavailable. This is the same guarantee on the wire, in a test that never
// skips: if the marker does not reach the JSON, no renderer can show it.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

func TestAccountsMarksForgedRejectionWindowAsUnmeasured(t *testing.T) {
	a := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	// A genuinely measured window alongside, so this cannot pass by every
	// row happening to carry the same marker.
	a.SetQuotaWindows([]pool.QuotaWindow{{
		Name: "5h", Limit: 1, Used: 0.42, Source: "headers",
		ResetAt: time.Now().Add(time.Hour), FetchedAt: time.Now(),
	}})
	p := pool.New([]*pool.Account{a}, time.Now())
	p.MarkWindowRejected(a, "7d-fable", time.Now().Add(6*time.Hour))

	front := httptest.NewServer(New(p, nil, events.New(), testToken))
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
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got []accountJSON
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("accounts = %+v", got)
	}

	var measured, forged *pool.QuotaWindow
	for i := range got[0].Windows {
		switch got[0].Windows[i].Name {
		case "5h":
			measured = &got[0].Windows[i]
		case "7d-fable":
			forged = &got[0].Windows[i]
		}
	}
	if measured == nil || forged == nil {
		t.Fatalf("windows = %+v, want both 5h and 7d-fable", got[0].Windows)
	}
	if measured.Source != "headers" {
		t.Errorf("measured 5h source = %q, want %q", measured.Source, "headers")
	}
	if forged.Source == measured.Source {
		t.Errorf("forged 7d-fable source = %q, identical to the measured 5h row — "+
			"the dashboard cannot tell the exclusion from evidence for it", forged.Source)
	}
	if forged.Source != "rejected" {
		t.Errorf("forged 7d-fable source = %q, want %q", forged.Source, "rejected")
	}
	if !strings.Contains(string(raw), `"source":"rejected"`) {
		t.Errorf(`/api/accounts body carries no "source":"rejected": %s`, raw)
	}
	// #54's dashboard bit must still light up: the row is unmeasured, not absent.
	if !got[0].FableSpent {
		t.Error("fableSpent = false — marking the row unmeasured must not stop it reading spent (#54)")
	}
}
