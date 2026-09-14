package admin

// Issue #165: /api/accounts is polled every 5s by the dashboard AND on every
// status-line render, inside a 350ms HTTP budget. It was costing a
// whole-table aggregate per call — 747ms on a 695,969-row live database —
// so the status line's fetch timed out and it rendered nothing at all.
//
// The endpoint-level assertion of the fix, without a wall clock: take the
// database out from under the handler entirely and require it to answer
// anyway, with the same figures. A handler that still queried `requests`
// would fail with "no such table"; one that serves the running total does
// not notice.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"

	_ "modernc.org/sqlite"
)

// fetchAccountsJSON drives the real /api/accounts route end to end.
func fetchAccountsJSON(t *testing.T, s *Server) []accountJSON {
	t.Helper()
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/accounts = %d, want 200", resp.StatusCode)
	}
	var got []accountJSON
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func cacheFigures(list []accountJSON) map[string][2]int64 {
	out := map[string][2]int64{}
	for _, a := range list {
		out[a.Name] = [2]int64{a.CacheCreateTokens, a.CacheReadTokens}
	}
	return out
}

// Eight accounts — the count the live daemon had when #165 was measured,
// rather than the four the budget was set against — and a request log whose
// tables are then dropped out from under the server. The response must be
// byte-identical, because producing it never touches them.
func TestAccountsServesCacheFiguresWithoutReadingTheRequestLog(t *testing.T) {
	var accts []*pool.Account
	for i := 0; i < 8; i++ {
		a := pool.NewAccount(fmt.Sprintf("acct-%d", i), pool.SourceYAML, "tok", "", 0, "")
		a.Type = "claude-oauth"
		a.SetQuotaWindows([]pool.QuotaWindow{{
			Name: "5h", Limit: 1, Used: 0.42, Source: "headers",
			ResetAt: time.Now().Add(time.Hour), FetchedAt: time.Now(),
		}})
		accts = append(accts, a)
	}
	p := pool.New(accts, time.Now())

	path := filepath.Join(t.TempDir(), "r.db")
	l, err := reqlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 8; i++ {
		for j := 0; j < 25; j++ {
			if err := l.Record(reqlog.Entry{
				Account: fmt.Sprintf("acct-%d", i), Path: "/v1/messages",
				Status: 200, Event: reqlog.EventServed,
				CacheCreationInputTokens: int64(100 * (i + 1)),
				CacheReadInputTokens:     int64(10 * (i + 1)),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	s := New(p, l, events.New(), testToken)
	before := fetchAccountsJSON(t, s)
	if len(before) != 8 {
		t.Fatalf("/api/accounts returned %d accounts, want 8", len(before))
	}
	wantFigures := cacheFigures(before)
	if got := wantFigures["acct-7"]; got != [2]int64{20000, 2000} {
		t.Fatalf("fixture wrong: acct-7 = %v, want create=20000 read=2000", got)
	}
	for _, a := range before {
		if a.CacheHitRate == nil {
			t.Fatalf("%s has no cache hit rate — the fixture logged it no volume", a.Name)
		}
	}

	// Take both tables away, from a separate connection, exactly as an
	// external observer would. Nothing in production does this; it is the
	// sharpest available probe for "did the handler read the database".
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{`DROP TABLE requests`, `DROP TABLE request_totals`} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// Guard the guard: if the drop had not taken effect, the probe below
	// would be vacuous and the whole test would pass while proving nothing.
	if _, err := raw.Query(`SELECT COUNT(*) FROM requests`); err == nil {
		t.Fatal("the requests table is still queryable after DROP — this test would prove nothing")
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	after := fetchAccountsJSON(t, s)
	if len(after) != len(before) {
		t.Fatalf("/api/accounts returned %d accounts with the request log gone, want %d",
			len(after), len(before))
	}
	gotFigures := cacheFigures(after)
	for name, want := range wantFigures {
		if got := gotFigures[name]; got != want {
			t.Errorf("%s's cache figures changed once the request log was unreadable: "+
				"%v -> %v — /api/accounts is still querying the table per request (issue #165)",
				name, want, got)
		}
	}
	for i := range before {
		if before[i].Name != after[i].Name {
			t.Errorf("account %d changed identity with the request log gone: %q -> %q",
				i, before[i].Name, after[i].Name)
			continue
		}
		if after[i].CacheHitRate == nil {
			t.Errorf("%s lost its cache hit rate once the request log was unreadable",
				before[i].Name)
			continue
		}
		if *before[i].CacheHitRate != *after[i].CacheHitRate {
			t.Errorf("%s's cache hit rate changed once the request log was unreadable: %v -> %v",
				before[i].Name, *before[i].CacheHitRate, *after[i].CacheHitRate)
		}
	}
}
