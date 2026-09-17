package reqlog

// Issue #187, the half of it that was a fixture rather than a bug:
// internal/admin's end-to-end test wrote its 30,000-row fixture with 30,000
// Record calls, which is 30,000 transactions and so 30,000 fsyncs. On macOS
// that is 2.7s and invisible; on the Windows runner the test spent the
// package's whole ten-minute budget inside FlushFileBuffers and was killed
// before reaching an assertion, reported as a timeout naming the test and
// nothing else.
//
// What follows asserts the MECHANISM, never a wall clock — a duration
// threshold here is a flake (issues #98, #134), and it would in any case be
// measuring the wrong machine. What is asserted is that the cost of seeding
// does not grow with the number of rows, counted through the same statement
// counter runningtotal_test.go already uses, plus that this faster path
// leaves the log in exactly the state the per-row path would have.

import (
	"fmt"
	"testing"
	"time"
)

// seedFixture builds n rows with predictable, non-zero counters.
func seedFixture(n int, start time.Time) []Entry {
	out := make([]Entry, 0, n)
	for i := range n {
		out = append(out, Entry{
			Ts:      start.Add(time.Duration(i) * time.Second),
			Account: fmt.Sprintf("acct-%d", i%2), Path: "/v1/messages", Status: 200,
			Event: EventServed, DurationMs: 100, Bytes: 2048,
			InputTokens: 1, OutputTokens: 2,
			CacheCreationInputTokens: 3, CacheReadInputTokens: 7,
		})
	}
	return out
}

// The cost of seeding must not scale with the row count. Ten times the rows
// for the same number of prepared statements is what "one transaction"
// means here; one statement per row is what "one commit and one fsync per
// row" looks like from the outside, and is the defect.
//
// Record's own count is measured alongside as the premise: it MUST scale, or
// the counter is not measuring what this test claims it is.
func TestSeedingCostDoesNotScaleWithRows(t *testing.T) {
	useCountingDriver(t)
	l := openTest(t)
	countingDriverActuallyCounts(t, l)

	const small, large = 200, 2000
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Millisecond)

	before := statementsPrepared.Load()
	if err := l.SeedForTest(seedFixture(small, base)); err != nil {
		t.Fatal(err)
	}
	smallCost := statementsPrepared.Load() - before

	before = statementsPrepared.Load()
	if err := l.SeedForTest(seedFixture(large, base.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	largeCost := statementsPrepared.Load() - before

	before = statementsPrepared.Load()
	for _, e := range seedFixture(small, base.Add(2*time.Hour)) {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	perRowCost := statementsPrepared.Load() - before

	if perRowCost < small {
		t.Fatalf("premise broken: %d Record calls prepared only %d statements — "+
			"the counter is not seeing the per-row path, so nothing below means anything",
			small, perRowCost)
	}
	if smallCost != largeCost {
		t.Errorf("seeding %d rows prepared %d statements and %d rows prepared %d: "+
			"the cost grows with the row count, so the fixture is being written one "+
			"transaction — and one fsync — per row again (#187)",
			small, smallCost, large, largeCost)
	}
	if smallCost >= perRowCost {
		t.Errorf("seeding %d rows cost %d statements, no better than the %d the "+
			"per-row path cost for the same rows", small, smallCost, perRowCost)
	}
}

// Faster is worthless if it is not the same. A seeded fixture and the same
// rows written through Record must leave an identical table and identical
// running lifetime totals — the figures /api/accounts serves from (#165).
func TestSeedForTestLeavesWhatRecordWouldHave(t *testing.T) {
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Millisecond)
	rows := seedFixture(500, base)

	seeded := openTest(t)
	if err := seeded.SeedForTest(rows); err != nil {
		t.Fatal(err)
	}

	recorded := openTest(t)
	for _, e := range rows {
		if err := recorded.Record(e); err != nil {
			t.Fatal(err)
		}
	}

	if a, b := rowCount(t, seeded), rowCount(t, recorded); a != b {
		t.Fatalf("seeded %d rows, recorded %d", a, b)
	}
	// The running total, which is what the dashboard actually reads.
	gotStats, err := seeded.CacheStats()
	if err != nil {
		t.Fatal(err)
	}
	wantStats, err := recorded.CacheStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotStats) != len(wantStats) {
		t.Fatalf("seeded log reports %d accounts, recorded log %d", len(gotStats), len(wantStats))
	}
	for i := range wantStats {
		if gotStats[i] != wantStats[i] {
			t.Errorf("account %d: seeded totals %+v, recorded totals %+v — "+
				"the fixture path has drifted from the production one",
				i, gotStats[i], wantStats[i])
		}
	}
	// And the rows themselves, in the order the dashboard reads them.
	gotRows, err := seeded.Recent(100)
	if err != nil {
		t.Fatal(err)
	}
	wantRows, err := recorded.Recent(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotRows) != len(wantRows) {
		t.Fatalf("Recent: seeded %d rows, recorded %d", len(gotRows), len(wantRows))
	}
	for i := range wantRows {
		if gotRows[i] != wantRows[i] {
			t.Errorf("Recent row %d differs: seeded %+v, recorded %+v", i, gotRows[i], wantRows[i])
		}
	}
}
