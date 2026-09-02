package reqlog

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *Log {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestRecordQuotaThrottlesWithinInterval(t *testing.T) {
	l := openTest(t)
	base := time.Now()
	for _, offset := range []time.Duration{0, 10 * time.Second, 30 * time.Second} {
		if err := l.RecordQuota(Sample{Ts: base.Add(offset), Account: "a", Window: "5h",
			Limit: 1, Used: 0.1}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.QuotaSince(base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("samples inside the throttle window should collapse to 1, got %d", len(got))
	}
	// Past the interval a new point lands.
	if err := l.RecordQuota(Sample{Ts: base.Add(2 * time.Minute), Account: "a", Window: "5h",
		Limit: 1, Used: 0.3}); err != nil {
		t.Fatal(err)
	}
	got, _ = l.QuotaSince(base.Add(-time.Hour))
	if len(got) != 2 {
		t.Fatalf("expected a second sample past the interval, got %d", len(got))
	}
}

func TestRecordQuotaSeparatesWindows(t *testing.T) {
	l := openTest(t)
	now := time.Now()
	for _, w := range []string{"5h", "7d"} {
		if err := l.RecordQuota(Sample{Ts: now, Account: "a", Window: w, Limit: 1, Used: 0.2}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := l.QuotaSince(now.Add(-time.Hour))
	if len(got) != 2 {
		t.Fatalf("windows are throttled independently; want 2 samples, got %d", len(got))
	}
}

func TestQuotaSinceOrdersOldestFirstAndKeepsReset(t *testing.T) {
	l := openTest(t)
	now := time.Now().Truncate(time.Millisecond)
	reset := now.Add(3 * time.Hour).Truncate(time.Millisecond)
	for i, used := range []float64{0.1, 0.4, 0.8} {
		if err := l.RecordQuota(Sample{Ts: now.Add(time.Duration(i*5) * time.Minute),
			Account: "a", Window: "5h", Limit: 1, Used: used, ResetAt: reset}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := l.QuotaSince(now.Add(-time.Hour))
	if len(got) != 3 {
		t.Fatalf("want 3 samples, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Ts.Before(got[i-1].Ts) {
			t.Fatal("samples must come back oldest first")
		}
	}
	if !got[0].ResetAt.Equal(reset) {
		t.Errorf("resetAt round-trip: got %v want %v", got[0].ResetAt, reset)
	}
	if got[2].Used != 0.8 {
		t.Errorf("used round-trip: got %v", got[2].Used)
	}
}

func TestQuotaSinceExcludesOlderThanCutoff(t *testing.T) {
	l := openTest(t)
	now := time.Now()
	_ = l.RecordQuota(Sample{Ts: now.Add(-3 * time.Hour), Account: "a", Window: "5h", Limit: 1, Used: 0.1})
	_ = l.RecordQuota(Sample{Ts: now, Account: "a", Window: "5h", Limit: 1, Used: 0.5})
	got, _ := l.QuotaSince(now.Add(-time.Hour))
	if len(got) != 1 || got[0].Used != 0.5 {
		t.Fatalf("cutoff not applied: %+v", got)
	}
}

func TestActivityBucketsIncludeEmptySlots(t *testing.T) {
	l := openTest(t)
	now := time.Now()
	cutoff := now.Add(-10 * time.Minute)
	// Two requests in the same minute, one 5 minutes later, nothing between.
	for _, e := range []Entry{
		{Ts: cutoff.Add(30 * time.Second), Account: "a", Path: "/v1/messages", Status: 200, DurationMs: 100, Event: EventServed},
		{Ts: cutoff.Add(40 * time.Second), Account: "a", Path: "/v1/messages", Status: 200, DurationMs: 300, Event: EventServed},
		{Ts: cutoff.Add(5 * time.Minute), Account: "a", Path: "/v1/messages", Status: 429, DurationMs: 900, Event: EventRotatedQuota},
	} {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	buckets, err := l.Activity(cutoff, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) < 10 {
		t.Fatalf("empty slots must be present so gaps show as gaps; got %d buckets", len(buckets))
	}
	if buckets[0].Count != 2 {
		t.Errorf("first bucket count: got %d want 2", buckets[0].Count)
	}
	if buckets[1].Count != 0 {
		t.Errorf("quiet minute should be an empty bucket, got %d", buckets[1].Count)
	}
	b5 := buckets[5]
	if b5.Count != 1 || b5.Errors != 1 || b5.Rotated != 1 {
		t.Errorf("bucket 5: got count=%d errors=%d rotated=%d, want 1/1/1", b5.Count, b5.Errors, b5.Rotated)
	}
	if buckets[0].P95Ms != 300 {
		t.Errorf("p95 of {100,300}: got %d want 300", buckets[0].P95Ms)
	}
}

func TestP95(t *testing.T) {
	for _, tc := range []struct {
		in   []int64
		want int64
	}{
		{nil, 0},
		{[]int64{5}, 5},
		{[]int64{3, 1, 2}, 3},
		{[]int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}, 100},
	} {
		if got := p95(append([]int64(nil), tc.in...)); got != tc.want {
			t.Errorf("p95(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Samples written under the old "weekly" name must be carried over, or the
// history chart draws one window as two series that hand over on upgrade day.
func TestOpenRenamesLegacyWeeklySamples(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "q.db")

	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Write a sample under the old name, the way an older build would have.
	if _, err := l.db.Exec(
		`INSERT INTO quota_samples (ts, account, window, lim, used) VALUES (?,?,?,?,?)`,
		time.Now().UnixMilli(), "kimi", "weekly", 100.0, 52.0); err != nil {
		t.Fatal(err)
	}
	l.Close()

	// Reopening runs the migration.
	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()

	var name string
	var used float64
	if err := l2.db.QueryRow(`SELECT window, used FROM quota_samples`).Scan(&name, &used); err != nil {
		t.Fatal(err)
	}
	if name != "7d" {
		t.Errorf("window = %q, want the legacy sample renamed to 7d", name)
	}
	if used != 52 {
		t.Errorf("used = %v, want the sample's data left alone", used)
	}
}

// The concurrency cap was never a quota, and its samples are a flat line at
// zero. Dropped on open so the chart is not asked to draw one.
func TestOpenDropsParallelSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for _, w := range []string{"parallel", "5h"} {
		if _, err := l.db.Exec(
			`INSERT INTO quota_samples (ts, account, window, lim, used) VALUES (?,?,?,?,?)`,
			now, "kimi", w, 30.0, 0.0); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()

	rows, err := l2.db.Query(`SELECT window FROM quota_samples`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			t.Fatal(err)
		}
		got = append(got, w)
	}
	if len(got) != 1 || got[0] != "5h" {
		t.Errorf("windows left = %v, want just 5h", got)
	}
}

// LatestQuotaSamples is what startup seeding reads from: one row per
// account/window pair, the newest write, regardless of how many older ones
// are sitting in the table.
func TestLatestQuotaSamplesReturnsNewestPerAccountWindow(t *testing.T) {
	l := openTest(t)
	base := time.Now()
	write := func(account, window string, used float64, offset time.Duration) {
		t.Helper()
		if err := l.RecordQuota(Sample{Ts: base.Add(offset), Account: account, Window: window,
			Limit: 1, Used: used, ResetAt: base.Add(offset + time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	write("a", "5h", 0.1, 0)
	write("a", "5h", 0.5, 2*time.Minute) // past sampleInterval: lands as a second row
	write("a", "7d", 0.2, 0)
	write("b", "5h", 0.9, 0)

	got, err := l.LatestQuotaSamples(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want one row per (account, window) pair (a/5h, a/7d, b/5h), got %d: %+v", len(got), got)
	}
	byKey := map[string]Sample{}
	for _, s := range got {
		byKey[s.Account+"/"+s.Window] = s
	}
	if s, ok := byKey["a/5h"]; !ok || s.Used != 0.5 {
		t.Errorf("a/5h should be the newest write (used=0.5), got %+v", s)
	}
	if s, ok := byKey["a/7d"]; !ok || s.Used != 0.2 {
		t.Errorf("a/7d missing or wrong: %+v", s)
	}
	if s, ok := byKey["b/5h"]; !ok || s.Used != 0.9 {
		t.Errorf("b/5h missing or wrong: %+v", s)
	}
}

// insertRawSample writes directly to quota_samples, bypassing RecordQuota's
// throttle and letting a test place two rows at the exact same ts (a tie) —
// something RecordQuota's own dedup logic would otherwise prevent.
func insertRawSample(t *testing.T, l *Log, ts time.Time, account, window string, lim, used float64, resetAt time.Time) {
	t.Helper()
	var reset any
	if !resetAt.IsZero() {
		reset = resetAt.UnixMilli()
	}
	if _, err := l.db.Exec(`INSERT INTO quota_samples (ts, account, window, lim, used, reset_at)
		VALUES (?, ?, ?, ?, ?, ?)`, ts.UnixMilli(), account, window, lim, used, reset); err != nil {
		t.Fatal(err)
	}
}

// oldLatestQuotaSamplesQuery is issue #104's original, unindexed correlated
// subquery — kept here, verbatim, ONLY so
// TestLatestQuotaSamplesRewriteMatchesOriginalQuery can prove the rewrite
// returns identical rows on identical data. It must never be used outside
// this one test.
const oldLatestQuotaSamplesQuery = `SELECT ts, account, window, lim, used, reset_at, fetched_at
	FROM quota_samples q
	WHERE ts = (SELECT MAX(ts) FROM quota_samples q2
		WHERE q2.account = q.account AND q2.window = q.window)`

func fetchSamples(t *testing.T, l *Log, query string) []Sample {
	t.Helper()
	rows, err := l.db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got, err := scanSamples(rows)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func sampleKey(s Sample) string {
	return fmt.Sprintf("%s|%s|%d|%g|%g|%d", s.Account, s.Window, s.Ts.UnixMilli(), s.Limit, s.Used, s.ResetAt.UnixMilli())
}

func sortedKeys(samples []Sample) []string {
	keys := make([]string, len(samples))
	for i, s := range samples {
		keys[i] = sampleKey(s)
	}
	sort.Strings(keys)
	return keys
}

// The rewrite (a join against a derived per-(account,window) MAX(ts) table)
// must return exactly the same rows as the original correlated subquery on
// identical data — including three edge cases the correlated form handles
// implicitly and a naive `GROUP BY` rewrite would not: an account with a
// single sample, a tie on ts within one (account, window) pair (the
// original's WHERE clause matches every row at the max, not just one), and
// a window that exists for one account but not another.
func TestLatestQuotaSamplesRewriteMatchesOriginalQuery(t *testing.T) {
	l := openTest(t)
	base := time.Now().Truncate(time.Millisecond)
	reset := base.Add(3 * time.Hour)

	// Ordinary history: several samples per pair, oldest to newest.
	insertRawSample(t, l, base, "a", "5h", 1, 0.1, reset)
	insertRawSample(t, l, base.Add(time.Minute), "a", "5h", 1, 0.3, reset)
	insertRawSample(t, l, base.Add(2*time.Minute), "a", "5h", 1, 0.5, reset)

	// Single-sample account: only one row ever recorded.
	insertRawSample(t, l, base, "solo", "7d", 1, 0.9, reset)

	// Tie on ts: two rows for the same (account, window) sharing the exact
	// newest timestamp, with different payloads. The original query's WHERE
	// matches both; a plain GROUP BY would arbitrarily keep only one.
	tieTs := base.Add(5 * time.Minute)
	insertRawSample(t, l, tieTs, "tied", "5h", 1, 0.4, reset)
	insertRawSample(t, l, tieTs, "tied", "5h", 1, 0.7, reset)
	// An older row for the same pair must NOT show up even though it's not tied.
	insertRawSample(t, l, base, "tied", "5h", 1, 0.05, reset)

	// A window that exists for one account but not another: "b" has 7d,
	// "a" (above) only has 5h.
	insertRawSample(t, l, base, "b", "7d", 1, 0.2, reset)

	want := sortedKeys(fetchSamples(t, l, oldLatestQuotaSamplesQuery))
	got, err := l.LatestQuotaSamples(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gotKeys := sortedKeys(got)

	if len(want) == 0 {
		t.Fatal("test fixture produced no rows from the reference query — fixture is broken")
	}
	if !reflect.DeepEqual(want, gotKeys) {
		t.Fatalf("rewrite diverges from the original query:\n old: %v\n new: %v", want, gotKeys)
	}
	// Specifically confirm the tie produced two rows, not one collapsed row.
	tiedCount := 0
	for _, s := range got {
		if s.Account == "tied" && s.Ts.Equal(tieTs) {
			tiedCount++
		}
	}
	if tiedCount != 2 {
		t.Errorf("tie on ts: want both tied rows preserved, got %d", tiedCount)
	}
}

// The rewrite's whole point is to stop doing a per-row correlated subquery.
// EXPLAIN QUERY PLAN on the exact text LatestQuotaSamples runs must not
// reference "q2" — the alias the old correlated subquery used for its inner
// scan. This fails immediately (no large fixture needed) if the query is
// ever reverted to the old form.
func TestLatestQuotaSamplesQueryPlanHasNoCorrelatedSubquery(t *testing.T) {
	l := openTest(t)
	rows, err := l.db.Query("EXPLAIN QUERY PLAN " + latestQuotaSamplesQuery)
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
	if strings.Contains(plan.String(), "q2") {
		t.Fatalf("query plan still references a per-row correlated subquery (alias q2): %s", plan.String())
	}
}

// With enough rows that the old correlated-subquery plan would be visibly
// slow (58,000 across 8 pairs mirrors issue #104's real database almost
// exactly), the rewrite must stay fast. The bound here is deliberately
// generous — this is a regression guard against reverting to the O(n²)-ish
// plan, not a micro-benchmark, and must not make CI flaky on a slow runner.
func TestLatestQuotaSamplesFastOnALargeFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large-fixture timing test in -short mode")
	}
	l := openTest(t)
	base := time.Now().Truncate(time.Millisecond)
	reset := base.Add(3 * time.Hour)
	const pairs = 8
	const rowsPerPair = 7200 // ~57,600 rows total, matching issue #104's live database

	tx, err := l.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO quota_samples (ts, account, window, lim, used, reset_at)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	for p := 0; p < pairs; p++ {
		account := fmt.Sprintf("acct-%d", p%4)
		window := "5h"
		if p%2 == 0 {
			window = "7d"
		}
		account = fmt.Sprintf("%s-%d", account, p) // keep all 8 pairs distinct
		for i := 0; i < rowsPerPair; i++ {
			ts := base.Add(time.Duration(i) * time.Second)
			if _, err := stmt.Exec(ts.UnixMilli(), account, window, 1.0, float64(i%100)/100, reset.UnixMilli()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	got, err := l.LatestQuotaSamples(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != pairs {
		t.Fatalf("want %d rows (one per account/window pair), got %d", pairs, len(got))
	}
	// Measured: the old correlated-subquery plan took 34.3s on this shape of
	// data (issue #104); the rewrite measures ~0.03s. 2s is a wide margin
	// for a slow CI runner while still failing hard if the plan regresses
	// back to the old O(n²)-ish behavior.
	if elapsed > 2*time.Second {
		t.Errorf("LatestQuotaSamples took %s on %d rows — the old correlated-subquery plan would take tens of seconds here; the rewrite should not", elapsed, pairs*rowsPerPair)
	}
}

func indexNames(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'quota_samples'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

// Open runs the (account, window, ts) index migration unconditionally on
// every open — it must be safe to run twice, and safe on a database that
// already carries an equivalent index added by hand under a different name
// (issue #104 notes one was: "applied locally as a stopgap").
func TestQuotaIndexMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")

	l1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening re-runs initQuota's migrations, including
	// `CREATE INDEX IF NOT EXISTS quota_account_window_ts`, against a
	// database that already has that exact index.
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("reopening a database that already has the index must not fail: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}

	names := indexNames(t, path)
	found := 0
	for _, n := range names {
		if n == "quota_account_window_ts" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("want exactly one quota_account_window_ts index after two opens, got %d (indexes: %v)", found, names)
	}
}

// A hand-added index under a name spillway didn't choose (issue #104: "An
// index was added by hand to the user's DB as a stopgap") must not make
// Open fail — CREATE INDEX IF NOT EXISTS only checks its own name, so this
// is a real path: the database ends up with two indexes covering the same
// columns, and that must be harmless, not an error.
func TestQuotaIndexMigrationToleratesHandAddedIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")

	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE quota_samples (
		ts INTEGER NOT NULL, account TEXT NOT NULL, window TEXT NOT NULL,
		lim REAL NOT NULL, used REAL NOT NULL, reset_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	// Simulates the hand-added stopgap index under whatever name an operator
	// happened to pick.
	if _, err := raw.Exec(`CREATE INDEX manual_stopgap_idx ON quota_samples (account, window, ts)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open must tolerate a pre-existing hand-added index: %v", err)
	}
	defer l.Close()

	names := indexNames(t, path)
	if !contains(names, "manual_stopgap_idx") {
		t.Errorf("hand-added index should survive Open, got %v", names)
	}
	if !contains(names, "quota_account_window_ts") {
		t.Errorf("Open should still create its own index alongside the hand-added one, got %v", names)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// PruneQuota is the mechanism issue #104 asks for: nothing was bounding
// quota_samples, which is how it grew to 57,617 rows answering an 8-row
// question. It must delete rows strictly older than cutoff and leave
// everything else — in particular, a recent sample must survive even when
// an old one for a different pair does not.
func TestPruneQuotaDeletesOnlyRowsOlderThanCutoff(t *testing.T) {
	l := openTest(t)
	now := time.Now().Truncate(time.Millisecond)

	insertRawSample(t, l, now.Add(-30*24*time.Hour), "old", "5h", 1, 0.5, now.Add(-29*24*time.Hour))
	insertRawSample(t, l, now.Add(-time.Hour), "recent", "5h", 1, 0.2, now.Add(4*time.Hour))

	cutoff := now.Add(-QuotaRetention)
	n, err := l.PruneQuota(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want exactly 1 row pruned, got %d", n)
	}

	remaining := fetchSamples(t, l, `SELECT ts, account, window, lim, used, reset_at, fetched_at FROM quota_samples`)
	if len(remaining) != 1 || remaining[0].Account != "recent" {
		t.Fatalf("want only the recent sample left, got %+v", remaining)
	}
}

// initQuota prunes on every Open, not only on main's periodic ticker — a
// database that only ever sees short-lived restarts must still shrink. This
// must not take the seed's own most-recent sample with it: a sample well
// within QuotaRetention (and within its own reset) has to survive an Open.
func TestOpenPruneDoesNotDeleteASampleSeedingStillNeeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	now := time.Now().Truncate(time.Millisecond)

	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// 10 days old: within QuotaRetention (14 days) and within its own
	// reset, so SeedQuota would use it if it survives.
	insertRawSample(t, l, now.Add(-10*24*time.Hour), "kept", "7d", 1, 0.3, now.Add(4*24*time.Hour))
	// Far older than QuotaRetention: this one should not survive a reopen.
	insertRawSample(t, l, now.Add(-30*24*time.Hour), "gone", "5h", 1, 0.9, now.Add(-29*24*time.Hour))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l2, err := Open(path) // re-runs initQuota's prune
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()

	got, err := l2.LatestQuotaSamples(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Account != "kept" {
		t.Fatalf("want only the still-recent 'kept' sample after reopen, got %+v", got)
	}
}

// Issue #138: RecordQuota/LatestQuotaSamples must carry FetchedAt separately
// from Ts — a window recorded by a sampler tick long after it was last
// really measured must not have that gap erased by the round trip.
func TestRecordQuotaKeepsFetchedAtSeparateFromTs(t *testing.T) {
	l := openTest(t)
	sampleTime := time.Now().Truncate(time.Millisecond)
	measuredAt := sampleTime.Add(-40 * time.Hour) // the real header's own age

	if err := l.RecordQuota(Sample{
		Ts: sampleTime, Account: "work", Window: "7d-fable",
		Limit: 1, Used: 1, ResetAt: sampleTime.Add(31 * time.Hour), FetchedAt: measuredAt,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := l.LatestQuotaSamples(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 sample, got %d", len(got))
	}
	if !got[0].Ts.Equal(sampleTime) {
		t.Errorf("Ts = %v, want the sampler's own write time %v", got[0].Ts, sampleTime)
	}
	if !got[0].FetchedAt.Equal(measuredAt) {
		t.Errorf("FetchedAt = %v, want the provider's measurement time %v — "+
			"it must not collapse onto Ts (issue #138)", got[0].FetchedAt, measuredAt)
	}
}

// A row written before the fetched_at column existed carries no honest
// measurement time. It must load without error (an existing database must
// keep working) and its FetchedAt must come back as the zero time — never
// something derived from Ts, which is exactly the illusion issue #138 is
// about: a sample's write time is not evidence of when the window was
// really measured.
func TestPreMigrationRowsHaveUnknownNotFreshFetchedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")

	// Simulate a database from before the fetched_at column existed: the
	// exact schema initQuota created prior to issue #138.
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE quota_samples (
		ts INTEGER NOT NULL, account TEXT NOT NULL, window TEXT NOT NULL,
		lim REAL NOT NULL, used REAL NOT NULL, reset_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	oldSampleTs := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	if _, err := raw.Exec(`INSERT INTO quota_samples (ts, account, window, lim, used, reset_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		oldSampleTs.UnixMilli(), "legacy", "7d", 1.0, 0.5,
		oldSampleTs.Add(4*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	l, err := Open(path)
	if err != nil {
		t.Fatalf("opening a pre-#138 database must succeed: %v", err)
	}
	defer l.Close()

	got, err := l.LatestQuotaSamples(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want the pre-existing row to still load, got %d rows", len(got))
	}
	if !got[0].FetchedAt.IsZero() {
		t.Errorf("FetchedAt = %v for a row with no fetched_at column value, want the zero time "+
			"(unknown, never mistaken for fresh) — got a value derived from Ts=%v instead",
			got[0].FetchedAt, oldSampleTs)
	}
}

// The fetched_at migration (ALTER TABLE ADD COLUMN, tolerating "duplicate
// column") must be safe to run twice against the same database — the same
// guarantee TestQuotaIndexMigrationIsIdempotent pins for the index
// migration, here for the column.
func TestFetchedAtColumnMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")

	l1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Close(); err != nil {
		t.Fatal(err)
	}

	l2, err := Open(path) // re-runs the ALTER TABLE ADD COLUMN fetched_at
	if err != nil {
		t.Fatalf("reopening a database that already has fetched_at must not fail: %v", err)
	}
	defer l2.Close()

	rows, err := l2.db.Query(`PRAGMA table_info(quota_samples)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "fetched_at" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("want exactly one fetched_at column after two opens, got %d", count)
	}

	// The column must still work for writes/reads after the second open.
	if err := l2.RecordQuota(Sample{
		Ts: time.Now(), Account: "a", Window: "5h", Limit: 1, Used: 0.1, FetchedAt: time.Now(),
	}); err != nil {
		t.Fatalf("RecordQuota after reopen: %v", err)
	}
}
