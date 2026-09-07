package admin

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// Issue #162 end to end, through the two endpoints the dashboard actually
// polls, against a real on-disk request log rather than a nil one:
// /api/requests must return the newest 100 rows newest-first, and
// /api/accounts must report per-account cache figures that do not move when
// retention deletes the rows they were computed from.
//
// The reqlog package pins the query plans; this pins the HTTP surface, which
// is where the 102.8s was actually observed.
func TestRequestAndAccountEndpointsOverARealLog(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-thousand-row fixture in -short mode")
	}
	logPath := filepath.Join(t.TempDir(), "requests.db")
	rl, err := reqlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	now := time.Now().Truncate(time.Millisecond)
	// Enough rows that an unindexed ORDER BY would have to sort them all,
	// spread over 20 days so a 14-day prune has something to remove.
	const rows = 30000
	span := 20 * 24 * time.Hour
	for i := 0; i < rows; i++ {
		account := "work"
		if i%2 == 0 {
			account = "labs"
		}
		if err := rl.Record(reqlog.Entry{
			Ts:      now.Add(-span + time.Duration(i)*(span/rows)),
			Account: account, Path: "/v1/messages", Status: 200,
			Event: reqlog.EventServed, DurationMs: 100, Bytes: 2048,
			InputTokens: 1, OutputTokens: 2,
			CacheCreationInputTokens: 3, CacheReadInputTokens: 7,
		}); err != nil {
			t.Fatal(err)
		}
	}

	work := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	work.Type = "claude-oauth"
	labs := pool.NewAccount("labs", pool.SourceYAML, "tok", "", 0, "")
	labs.Type = "claude-oauth"
	p := pool.New([]*pool.Account{work, labs}, now)
	front := httptest.NewServer(New(p, rl, events.New(), testToken))
	defer front.Close()

	getJSON := func(path string, into any) {
		t.Helper()
		req, err := authed(front.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := front.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}

	var entries []reqlog.Entry
	getJSON("/api/requests", &entries)
	if len(entries) != 100 {
		t.Fatalf("/api/requests returned %d entries, want the newest 100", len(entries))
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Ts.After(entries[i-1].Ts) {
			t.Fatalf("/api/requests entry %d is newer than %d — not newest-first", i, i-1)
		}
	}
	// The newest row really is the newest written, not whatever a sort
	// happened to surface.
	if got, want := entries[0].Ts.UnixMilli(),
		now.Add(-span+time.Duration(rows-1)*(span/rows)).UnixMilli(); got != want {
		t.Errorf("newest entry ts = %d, want %d", got, want)
	}

	var beforeAccounts []struct {
		Name              string   `json:"name"`
		CacheHitRate      *float64 `json:"cacheHitRate"`
		CacheCreateTokens int64    `json:"cacheCreateTokens"`
		CacheReadTokens   int64    `json:"cacheReadTokens"`
	}
	getJSON("/api/accounts", &beforeAccounts)
	if len(beforeAccounts) != 2 {
		t.Fatalf("/api/accounts returned %d accounts, want 2", len(beforeAccounts))
	}
	for _, a := range beforeAccounts {
		if a.CacheCreateTokens != 3*rows/2 || a.CacheReadTokens != 7*rows/2 {
			t.Fatalf("%s: create/read = %d/%d, want %d/%d",
				a.Name, a.CacheCreateTokens, a.CacheReadTokens, 3*rows/2, 7*rows/2)
		}
		if a.CacheHitRate == nil {
			t.Fatalf("%s: cacheHitRate is null with cache volume present", a.Name)
		}
	}

	// Now retention runs. Rows go; the displayed lifetime figures must not.
	//
	// Twice, at widening cutoffs, deliberately: the first prune INSERTs a
	// rollup row for each account and the second one CONFLICTs with it and
	// takes the accumulate branch. A single prune leaves that branch
	// completely unexercised, which is exactly how a rollup that replaces
	// instead of adding would ship looking fine.
	n, err := rl.PruneRequests(now.Add(-18 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("first prune removed nothing — the fixture has no rows past 18 days")
	}
	second, err := rl.PruneRequests(now.Add(-reqlog.RequestRetention))
	if err != nil {
		t.Fatal(err)
	}
	if second == 0 {
		t.Fatal("second prune removed nothing — the upsert's accumulate branch is untested")
	}
	n += second

	var afterAccounts []struct {
		Name              string   `json:"name"`
		CacheHitRate      *float64 `json:"cacheHitRate"`
		CacheCreateTokens int64    `json:"cacheCreateTokens"`
		CacheReadTokens   int64    `json:"cacheReadTokens"`
	}
	getJSON("/api/accounts", &afterAccounts)
	if len(afterAccounts) != len(beforeAccounts) {
		t.Fatalf("/api/accounts returned %d accounts after the prune, want %d",
			len(afterAccounts), len(beforeAccounts))
	}
	for i := range beforeAccounts {
		b, a := beforeAccounts[i], afterAccounts[i]
		if b.Name != a.Name || b.CacheCreateTokens != a.CacheCreateTokens ||
			b.CacheReadTokens != a.CacheReadTokens {
			t.Errorf("%s's dashboard cache figures moved across a %d-row prune: %+v -> %+v",
				b.Name, n, b, a)
		}
		if a.CacheHitRate == nil || *a.CacheHitRate != *b.CacheHitRate {
			t.Errorf("%s's hit rate changed across the prune", b.Name)
		}
	}

	// And the request list still works, now reading only surviving rows.
	var afterEntries []reqlog.Entry
	getJSON("/api/requests", &afterEntries)
	if len(afterEntries) != 100 {
		t.Fatalf("/api/requests returned %d entries after the prune, want 100", len(afterEntries))
	}
	for _, e := range afterEntries {
		if e.Ts.Before(now.Add(-reqlog.RequestRetention)) {
			t.Fatalf("a pruned-era row survived: %v", e.Ts)
		}
	}
}
