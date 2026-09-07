package reqlog

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Issue #162: the `requests` table carried no index at all, so Recent's
// `ORDER BY ts DESC, rowid DESC LIMIT 100` sorted every row in the table to
// return a hundred — `SCAN requests` + `USE TEMP B-TREE FOR ORDER BY`,
// which under modernc.org/sqlite is built and spilled in Go. On the live
// daemon that was 103 s per /api/requests call against a 5-second poll.
//
// The tests below assert the PLAN, not a wall clock, for everything that
// must not regress (a timing assertion is a flake — issues #98, #134); the
// one timing test is a deliberately generous regression guard, in #104's
// shape, and is skipped in -short mode.

// queryPlan returns the flattened EXPLAIN QUERY PLAN text for q.
func queryPlan(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			fmt.Fprintf(&plan, "%v ", v)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan.String()
}

// The plan for the exact text Recent runs must read the table through
// requests_ts and must contain no sort step. Both halves matter: an index
// SQLite declines to use is worthless, and a plan that uses the index but
// still sorts (which is what an index on `(ts DESC)` produces — see
// TestDescendingTsIndexWouldStillSort) has not fixed the defect.
func TestRecentQueryPlanUsesTheTsIndex(t *testing.T) {
	l := openTest(t)
	plan := queryPlan(t, l.db, recentQuery, 100)
	if !strings.Contains(plan, "USING INDEX requests_ts") {
		t.Errorf("Recent's plan does not use requests_ts: %s", plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("Recent's plan still sorts (temp B-tree) — this is issue #162's defect: %s", plan)
	}
}

// CacheStats runs on the same 5-second poll. Its `GROUP BY account`
// summing four counters must be answered from requests_account_tokens
// alone — a COVERING index scan, never a visit to the 23-column rows and
// never a temp B-tree for the grouping.
func TestCacheStatsQueryPlanUsesTheCoveringIndex(t *testing.T) {
	l := openTest(t)
	plan := queryPlan(t, l.db, cacheStatsQuery)
	if !strings.Contains(plan, "COVERING INDEX requests_account_tokens") {
		t.Errorf("CacheStats' plan is not a covering-index scan: %s", plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("CacheStats' plan still builds a temp B-tree for the GROUP BY: %s", plan)
	}
}

// Why requests_ts is `(ts)` and not `(ts DESC, rowid DESC)` or `(ts DESC)`,
// pinned so nobody "fixes" the index to match the ORDER BY and makes it
// worse: SQLite rejects rowid in an index on a rowid table outright, and a
// descending index leaves a sort step behind because its implicit rowid
// suffix still ascends. Plain (ts) read backwards satisfies both terms.
func TestDescendingTsIndexWouldStillSort(t *testing.T) {
	l := openTest(t)

	if _, err := l.db.Exec(`CREATE INDEX bad_ts ON requests (ts DESC, rowid DESC)`); err == nil {
		t.Fatal("expected SQLite to reject rowid in an index on a rowid table")
	} else if !strings.Contains(err.Error(), "no such column: rowid") {
		t.Fatalf("unexpected rejection reason: %v", err)
	}

	// A DESC-only index is accepted but strictly worse than the one we
	// ship. Drop ours so SQLite has to choose it.
	if _, err := l.db.Exec(`DROP INDEX requests_ts`); err != nil {
		t.Fatal(err)
	}
	if _, err := l.db.Exec(`CREATE INDEX desc_ts ON requests (ts DESC)`); err != nil {
		t.Fatal(err)
	}
	plan := queryPlan(t, l.db, recentQuery, 100)
	if !strings.Contains(plan, "TEMP B-TREE") {
		t.Skipf("this SQLite build satisfies the ORDER BY from a DESC index (%s) — "+
			"requests_ts is still correct, but the comparison this test pins no longer holds", plan)
	}
}

// requestIndexNames lists the indexes SQLite holds on `requests`, excluding the
// automatic ones it creates for constraints.
func requestIndexNames(t *testing.T, path string) []string {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rows, err := raw.Query(`SELECT name FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'requests' AND name NOT LIKE 'sqlite_autoindex%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The index migration runs unconditionally at every open, so opening an
// already-indexed database twice must be a no-op rather than an error or a
// duplicate.
func TestRequestIndexMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")

	l1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Close(); err != nil {
		t.Fatal(err)
	}
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("reopening an already-indexed database must not fail: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}
	// A third open, to be sure the second did not leave something the
	// third trips over.
	l3, err := Open(path)
	if err != nil {
		t.Fatalf("third open must not fail: %v", err)
	}
	defer l3.Close()

	seen := map[string]int{}
	for _, n := range requestIndexNames(t, path) {
		seen[n]++
	}
	for _, want := range []string{"requests_ts", "requests_account_tokens"} {
		if seen[want] != 1 {
			t.Errorf("index %q appears %d times after three opens, want 1", want, seen[want])
		}
	}
}

// A database written by the current release — every column present, no
// index on `requests` at all, which is what issue #162 measured on the live
// daemon — must gain both indexes in place, keep its rows, and read back
// identically.
func TestExistingUnindexedDatabaseGainsIndexesOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unindexed.db")

	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	for i := 0; i < 50; i++ {
		if err := l.Record(Entry{
			Ts: base.Add(time.Duration(i) * time.Second), Account: fmt.Sprintf("acct-%d", i%3),
			Path: "/v1/messages", Status: 200, Event: EventServed,
			InputTokens: int64(i), CacheReadInputTokens: int64(10 * i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Put the database back into its pre-#162 state.
	for _, name := range []string{"requests_ts", "requests_account_tokens"} {
		if _, err := l.db.Exec(`DROP INDEX ` + name); err != nil {
			t.Fatal(err)
		}
	}
	wantEntries, err := l.Recent(1000)
	if err != nil {
		t.Fatal(err)
	}
	wantStats, err := l.CacheStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(requestIndexNames(t, path)) != 0 {
		t.Fatalf("fixture is not unindexed: %v", requestIndexNames(t, path))
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	up, err := Open(path)
	if err != nil {
		t.Fatalf("Open on an unindexed database must migrate cleanly, got: %v", err)
	}
	defer up.Close()

	seen := map[string]bool{}
	for _, n := range requestIndexNames(t, path) {
		seen[n] = true
	}
	if !seen["requests_ts"] || !seen["requests_account_tokens"] {
		t.Fatalf("Open did not create both indexes: %v", requestIndexNames(t, path))
	}

	gotEntries, err := up.Recent(1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotEntries) != len(wantEntries) {
		t.Fatalf("Recent returned %d rows after indexing, want %d", len(gotEntries), len(wantEntries))
	}
	for i := range wantEntries {
		if gotEntries[i] != wantEntries[i] {
			t.Fatalf("row %d changed across the index migration:\n unindexed: %+v\n indexed:   %+v",
				i, wantEntries[i], gotEntries[i])
		}
	}
	gotStats, err := up.CacheStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotStats) != len(wantStats) {
		t.Fatalf("CacheStats returned %d accounts after indexing, want %d", len(gotStats), len(wantStats))
	}
	for i := range wantStats {
		if gotStats[i] != wantStats[i] {
			t.Errorf("account %d's stats changed across the index migration: %+v vs %+v",
				i, wantStats[i], gotStats[i])
		}
	}
}

// seedRequests writes n rows spread over span, ending now, with ties on ts
// so the `rowid DESC` tie-break is actually exercised. Written in one
// transaction with a prepared statement: 584k individual commits would
// dominate the runtime of the test that uses it.
func seedRequests(t *testing.T, l *Log, n int, span time.Duration, end time.Time) {
	t.Helper()
	accts := []string{"work", "personal", "team-a", "team-b", "kimi-1", "kimi-2", "spare", "backup"}
	step := span / time.Duration(n)
	tx, err := l.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO requests
		(ts, account, path, status, duration_ms, bytes, event, model_asked, model_served, user_agent,
		 session_hash, input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
		 tool_count, tools_order_hash, tools_sorted_hash, tools_raw_hash, system_hash,
		 first_msg_shape_hash, first_msg_blocks, prefix_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		// i/2 rather than i: every timestamp appears twice, so the newest
		// page of results contains ties and an index that got the
		// tie-break wrong would show up as a reordering.
		ts := end.Add(-span + time.Duration(i/2)*2*step).UnixMilli()
		h := fmt.Sprintf("%016x", uint64(i)*2654435761)
		if _, err := stmt.Exec(ts, accts[i%len(accts)], "/v1/messages", 200, 1200+i%900,
			4096+i%9000, EventServed,
			"claude-sonnet-4-5", "claude-sonnet-4-5", "claude-cli/2.1.0 (external, cli)",
			h[:8], int64(30+i%200), int64(400+i%3000), int64(i%20000), int64(i%90000),
			30, h, h, h, h, h, 3, int64(20000+i%40000)); err != nil {
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// An index changes the PLAN; it must not change the ANSWER. Over a fixture
// big enough for the two plans to differ (and with ties on ts, which is
// where a wrong index would reorder rows), Recent must return byte-identical
// entries in identical order with the index present and absent.
func TestRecentReturnsIdenticalRowsWithAndWithoutTheIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large-fixture test in -short mode")
	}
	l := openTest(t)
	seedRequests(t, l, 20000, 24*time.Hour, time.Now().Truncate(time.Millisecond))

	indexed := queryPlan(t, l.db, recentQuery, 100)
	if !strings.Contains(indexed, "USING INDEX requests_ts") {
		t.Fatalf("fixture is not exercising the index: %s", indexed)
	}
	withIndex, err := l.Recent(100)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := l.db.Exec(`DROP INDEX requests_ts`); err != nil {
		t.Fatal(err)
	}
	unindexed := queryPlan(t, l.db, recentQuery, 100)
	if !strings.Contains(unindexed, "TEMP B-TREE") {
		t.Fatalf("fixture without the index does not sort — the comparison is vacuous: %s", unindexed)
	}
	withoutIndex, err := l.Recent(100)
	if err != nil {
		t.Fatal(err)
	}

	if len(withIndex) != 100 || len(withoutIndex) != 100 {
		t.Fatalf("expected 100 rows each, got %d and %d", len(withIndex), len(withoutIndex))
	}
	for i := range withoutIndex {
		if withIndex[i] != withoutIndex[i] {
			t.Fatalf("row %d differs between the indexed and unindexed plan:\n indexed:   %+v\n unindexed: %+v",
				i, withIndex[i], withoutIndex[i])
		}
	}
	// And the order really is newest-first, so "identical" is not two
	// identically-wrong answers.
	for i := 1; i < len(withIndex); i++ {
		if withIndex[i].Ts.After(withIndex[i-1].Ts) {
			t.Fatalf("row %d is newer than row %d — Recent is not newest-first", i, i-1)
		}
	}
}

// One live-sized fixture — liveFixtureRows over 17 days, which under a
// plain `go test` is issue #162's real database exactly — exercising
// everything that only a realistic table can distinguish. A 100-row fixture
// cannot tell a missing index from a present one: SQLite's planner consults
// the table's own statistics, so "the plan uses the index" is only worth
// asserting at scale once the planner has had a real choice.
//
// Deliberately NO wall-clock assertion. The timings are logged, not
// asserted, because a threshold here is not a fact about the fix: this same
// prune measured 1.4s under `go test` and 21.4s under `go test -race`, so
// any bound generous enough for the race detector would sail straight past
// a genuinely unindexed table (issues #98, #134). What is asserted is the
// plan, which is deterministic, and the answer, which must not change.
func TestRequestQueriesOnALiveSizedFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large-fixture test in -short mode")
	}
	l := openTest(t)
	now := time.Now().Truncate(time.Millisecond)
	// 17 days of history, so the prune below deletes the same ~3/17ths of
	// the table the real first prune will.
	seedRequests(t, l, liveFixtureRows, 17*24*time.Hour, now)

	// With six figures of rows of statistics behind it, the planner must
	// still choose both indexes.
	if plan := queryPlan(t, l.db, recentQuery, 100); !strings.Contains(plan, "USING INDEX requests_ts") ||
		strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("Recent's plan on a %s-row table is not a plain index scan: %s", liveFixtureLabel, plan)
	}
	if plan := queryPlan(t, l.db, cacheStatsQuery); !strings.Contains(plan, "COVERING INDEX requests_account_tokens") ||
		strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("CacheStats' plan on a %s-row table is not a covering scan: %s", liveFixtureLabel, plan)
	}

	start := time.Now()
	entries, err := l.Recent(100)
	recentElapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 100 {
		t.Fatalf("Recent returned %d rows, want 100", len(entries))
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Ts.After(entries[i-1].Ts) {
			t.Fatalf("row %d is newer than row %d — Recent is not newest-first", i, i-1)
		}
	}

	start = time.Now()
	before, err := l.CacheStats()
	statsElapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 8 {
		t.Fatalf("CacheStats returned %d accounts, want 8", len(before))
	}

	// Pruning at this size happens on the startup path, before either
	// listener binds — and it must not move the lifetime totals it rolls up.
	start = time.Now()
	n, err := l.PruneRequests(now.Add(-RequestRetention))
	pruneElapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	// 3 of 17 days is ~17% of the table; require at least a tenth, so this
	// cannot pass by pruning a handful of rows.
	if n < liveFixtureRows/10 {
		t.Fatalf("pruned only %d of %s rows — the fixture is not exercising a realistic prune",
			n, liveFixtureLabel)
	}
	after, err := l.CacheStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("account count changed across the prune: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("%s's totals moved across a %d-row prune: %+v -> %+v",
				before[i].Account, n, before[i], after[i])
		}
	}

	t.Logf("%s rows: Recent(100) %v, CacheStats %v, prune of %d rows %v",
		liveFixtureLabel, recentElapsed, statsElapsed, n, pruneElapsed)
}
