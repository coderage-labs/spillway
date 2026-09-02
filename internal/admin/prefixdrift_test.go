package admin

// Issue #111 phase 1's report has to be reachable the way every other
// diagnostic here is: a read-only GET on the loopback admin listener,
// returning JSON. A measurement nobody can query is not a measurement.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

func TestPrefixDriftEndpoint(t *testing.T) {
	rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	base := time.Now().Add(-time.Hour)
	rows := []reqlog.Entry{
		{Ts: base, Account: "work", Path: "/v1/messages", Status: 200, Event: reqlog.EventServed,
			SessionHash: "s1", InputTokens: 10,
			ToolsOrderHash: "1111111111111111", ToolsSortedHash: "2222222222222222",
			ToolsRawHash: "3333333333333333", SystemHash: "4444444444444444",
			FirstMsgShapeHash: "5555555555555555"},
		{Ts: base.Add(time.Second), Account: "work", Path: "/v1/messages", Status: 200, Event: reqlog.EventServed,
			SessionHash: "s1", InputTokens: 10, CacheCreationInputTokens: 777,
			// Reordered: ordered hash moved, sorted held still.
			ToolsOrderHash: "6666666666666666", ToolsSortedHash: "2222222222222222",
			ToolsRawHash: "7777777777777777", SystemHash: "4444444444444444",
			FirstMsgShapeHash: "5555555555555555"},
	}
	for i, e := range rows {
		if err := rl.Record(e); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}

	a := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	s := New(pool.New([]*pool.Account{a}, time.Now()), rl, events.New(), testToken)
	front := httptest.NewServer(s)
	defer front.Close()

	req, _ := authed(front.URL + "/api/prefix-drift")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got []reqlog.PrefixChange
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var jitter reqlog.PrefixChange
	found := false
	for _, c := range got {
		if c.Kind == reqlog.KindToolsOrderOnly && !c.AccountChanged {
			jitter, found = c, true
		}
	}
	if !found {
		t.Fatalf("no tools-order-only cell in %v", got)
	}
	if jitter.Pairs != 1 || jitter.CacheCreationInputTokens != 777 {
		t.Errorf("tools-order-only = %+v, want 1 pair / 777 tokens — the endpoint is not serving the real query", jitter)
	}
}

// With no request log wired up the endpoint must answer with an empty
// report rather than a 500 or a null, same as every other diagnostic here.
func TestPrefixDriftEndpointWithoutLog(t *testing.T) {
	s, _ := newTestServer(t)
	front := httptest.NewServer(s)
	defer front.Close()

	req, _ := authed(front.URL + "/api/prefix-drift")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got []reqlog.PrefixChange
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("report = %v, want empty with no request log", got)
	}
}
