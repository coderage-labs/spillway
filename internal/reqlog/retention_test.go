package reqlog

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Issue #162 put retention on `requests`, which until then grew forever.
// The point of these tests is the thing retention could easily break and
// nobody would notice: CacheStats is displayed as a LIFETIME per-account
// total, so deleting rows must not move the number.

func statsFor(t *testing.T, l *Log, account string) CacheStat {
	t.Helper()
	stats, err := l.CacheStats()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stats {
		if s.Account == account {
			return s
		}
	}
	t.Fatalf("no CacheStats entry for %q in %+v", account, stats)
	return CacheStat{}
}

// recordAt writes one row at an explicit time with known counters.
func recordAt(t *testing.T, l *Log, ts time.Time, account string, in, out, create, read int64) {
	t.Helper()
	if err := l.Record(Entry{
		Ts: ts, Account: account, Path: "/v1/messages", Status: 200, Event: EventServed,
		InputTokens: in, OutputTokens: out,
		CacheCreationInputTokens: create, CacheReadInputTokens: read,
	}); err != nil {
		t.Fatal(err)
	}
}

// The whole justification for the rollup: pruning must delete the rows and
// leave the displayed lifetime totals exactly where they were.
func TestPruneRequestsPreservesLifetimeCacheStats(t *testing.T) {
	l := openTest(t)
	now := time.Now().Truncate(time.Millisecond)

	// Old rows (pruned) and new rows (kept), for two accounts.
	recordAt(t, l, now.Add(-40*24*time.Hour), "work", 100, 200, 300, 400)
	recordAt(t, l, now.Add(-30*24*time.Hour), "work", 1, 2, 3, 4)
	recordAt(t, l, now.Add(-20*24*time.Hour), "personal", 10, 20, 30, 40)
	recordAt(t, l, now.Add(-time.Hour), "work", 1000, 2000, 3000, 4000)
	recordAt(t, l, now.Add(-time.Minute), "personal", 7, 7, 7, 7)

	before := map[string]CacheStat{
		"work":     statsFor(t, l, "work"),
		"personal": statsFor(t, l, "personal"),
	}
	if before["work"].InputTokens != 1101 {
		t.Fatalf("fixture wrong: work input = %d, want 1101", before["work"].InputTokens)
	}

	n, err := l.PruneRequests(now.Add(-RequestRetention))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("pruned %d rows, want 3 (the three older than 14 days)", n)
	}

	// The rows really are gone.
	var rows int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Errorf("requests holds %d rows after pruning, want 2", rows)
	}

	// And the totals have not moved.
	for _, account := range []string{"work", "personal"} {
		got := statsFor(t, l, account)
		if got != before[account] {
			t.Errorf("%s's lifetime totals changed across the prune:\n before: %+v\n after:  %+v",
				account, before[account], got)
		}
	}
}

// Pruning runs at every open and hourly thereafter, so it must be safe to
// run repeatedly: no row may be summed into the rollup twice, and a prune
// with nothing to delete must not disturb the totals.
func TestPruneRequestsRepeatedlyDoesNotDoubleCount(t *testing.T) {
	l := openTest(t)
	now := time.Now().Truncate(time.Millisecond)

	recordAt(t, l, now.Add(-40*24*time.Hour), "work", 100, 200, 300, 400)
	recordAt(t, l, now.Add(-time.Hour), "work", 1, 1, 1, 1)
	want := statsFor(t, l, "work")

	cutoff := now.Add(-RequestRetention)
	first, err := l.PruneRequests(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Fatalf("first prune removed %d rows, want 1", first)
	}
	for i := 0; i < 4; i++ {
		n, err := l.PruneRequests(cutoff)
		if err != nil {
			t.Fatalf("prune %d: %v", i+2, err)
		}
		if n != 0 {
			t.Fatalf("prune %d removed %d rows, want 0 — the first prune left rows behind", i+2, n)
		}
		if got := statsFor(t, l, "work"); got != want {
			t.Fatalf("totals drifted on prune %d: %+v, want %+v", i+2, got, want)
		}
	}
}

// A second prune at a LATER cutoff folds a second batch into the same
// rollup row: the upsert must add to what is there, not replace it.
func TestPruneRequestsAccumulatesAcrossCutoffs(t *testing.T) {
	l := openTest(t)
	now := time.Now().Truncate(time.Millisecond)

	recordAt(t, l, now.Add(-40*24*time.Hour), "work", 100, 0, 0, 0)
	recordAt(t, l, now.Add(-20*24*time.Hour), "work", 20, 0, 0, 0)
	recordAt(t, l, now.Add(-time.Hour), "work", 3, 0, 0, 0)
	want := statsFor(t, l, "work")
	if want.InputTokens != 123 {
		t.Fatalf("fixture wrong: input = %d, want 123", want.InputTokens)
	}

	if _, err := l.PruneRequests(now.Add(-30 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := statsFor(t, l, "work"); got.InputTokens != 123 {
		t.Fatalf("after first prune input = %d, want 123", got.InputTokens)
	}
	if _, err := l.PruneRequests(now.Add(-10 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := statsFor(t, l, "work"); got.InputTokens != 123 {
		t.Fatalf("after second prune input = %d, want 123 — the rollup was replaced, not accumulated",
			got.InputTokens)
	}
}

// An account that has gone quiet long enough for every one of its rows to
// be pruned still has a lifetime total, and the dashboard must keep showing
// it rather than dropping the account to zero.
func TestCacheStatsKeepsAnAccountWithEveryRowPruned(t *testing.T) {
	l := openTest(t)
	now := time.Now().Truncate(time.Millisecond)

	recordAt(t, l, now.Add(-40*24*time.Hour), "retired", 11, 22, 33, 44)
	recordAt(t, l, now.Add(-time.Hour), "active", 1, 1, 1, 1)

	if _, err := l.PruneRequests(now.Add(-RequestRetention)); err != nil {
		t.Fatal(err)
	}
	got := statsFor(t, l, "retired")
	want := CacheStat{Account: "retired", InputTokens: 11, OutputTokens: 22,
		CacheCreationInputTokens: 33, CacheReadInputTokens: 44}
	if got != want {
		t.Errorf("an account with no surviving rows lost its lifetime total: %+v, want %+v", got, want)
	}
	// HitRate must still be computable from the rollup alone.
	if r := got.HitRate(); r == nil {
		t.Error("HitRate() is nil for an account whose volume is entirely in the rollup")
	} else if *r != 44.0/77.0 {
		t.Errorf("HitRate() = %v, want %v", *r, 44.0/77.0)
	}
}

// Open prunes once, so a daemon that only ever sees short restarts still
// gets pruned (the same reasoning #104 applied to quota_samples), and a
// database that has never been pruned is bounded the first time an upgraded
// binary opens it — without the lifetime totals moving.
func TestOpenPrunesRowsOlderThanRequestRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	now := time.Now().Truncate(time.Millisecond)

	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	recordAt(t, l, now.Add(-40*24*time.Hour), "work", 100, 200, 300, 400)
	recordAt(t, l, now.Add(-15*24*time.Hour), "work", 10, 20, 30, 40)
	recordAt(t, l, now.Add(-time.Hour), "work", 1, 2, 3, 4)
	want := statsFor(t, l, "work")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	up, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()

	entries, err := up.Recent(1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("Recent returned %d rows after an Open-time prune, want 1 (only the recent row)",
			len(entries))
	}
	if got := statsFor(t, up, "work"); got != want {
		t.Errorf("lifetime totals moved across an Open-time prune: %+v, want %+v", got, want)
	}
}

// The rollup table itself must survive re-opening, which is what carries
// the totals across a restart at all.
func TestRequestTotalsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	now := time.Now().Truncate(time.Millisecond)

	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	recordAt(t, l, now.Add(-40*24*time.Hour), "work", 5, 6, 7, 8)
	if _, err := l.PruneRequests(now.Add(-RequestRetention)); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		got := statsFor(t, reopened, "work")
		want := CacheStat{Account: "work", InputTokens: 5, OutputTokens: 6,
			CacheCreationInputTokens: 7, CacheReadInputTokens: 8}
		if got != want {
			t.Errorf("open %d: totals = %+v, want %+v", i, got, want)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// Retention must be at least twice the longest window anything reads the
// table over — the activity histogram's ?hours=168 cap (admin.hoursParam)
// — which is the same margin QuotaRetention keeps. Pinned because
// shortening it below 7 days would silently start truncating a chart the
// dashboard still offers.
func TestRequestRetentionCoversTheLongestReader(t *testing.T) {
	const longestReader = 168 * time.Hour
	if RequestRetention < 2*longestReader {
		t.Errorf("RequestRetention = %v, want at least 2x the 168h activity window (%v)",
			RequestRetention, 2*longestReader)
	}
}

// Pruning a live-sized table — and the exactness of the rollup across a
// ~103k-row delete — is covered by TestRequestQueriesOnALiveSizedFixture in
// index_test.go, which seeds that fixture once for both.

// Sanity: the rollup carries token sums only. Nothing else about a pruned
// request survives, and the redaction surface is unchanged — request_totals
// holds an account name and four integers, nothing more.
func TestRequestTotalsSchemaIsFourCountersAndAnAccount(t *testing.T) {
	l := openTest(t)
	rows, err := l.db.Query(`SELECT name FROM pragma_table_info('request_totals')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"account", "input_tokens", "output_tokens",
		"cache_creation_input_tokens", "cache_read_input_tokens"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("request_totals columns = %v, want exactly %v", got, want)
	}
}
