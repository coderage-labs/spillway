package reqlog

// Issue #165: /api/accounts took 747 ms on the live daemon because
// CacheStats re-ran a whole-table aggregate on every call, which blew the
// status line's 350 ms HTTP budget and made it render nothing at all.
//
// The fix is a running total: one scan at Open, an add per Record, no query
// per read. These tests assert that MECHANISM, never a wall clock — a
// latency threshold here is a flake (issues #98, #134) and #163 removed its
// timing assertions for exactly that reason. What is asserted instead is
//
//   - the number of SQL statements a CacheStats call executes (zero), at two
//     table sizes an order of magnitude apart, so "the cost does not scale
//     with rows" is a counted fact rather than a stopwatch reading, and
//   - that the number this serves still equals the SQL aggregate it replaced,
//     across records, prunes and a restart — because a fast figure that has
//     quietly drifted from the truth is worse than a slow one, and this
//     repository has shipped that mistake before.

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── counting the statements a Log executes ───────────────────────────────

// statementsPrepared counts every statement routed through countingConn.
// Global rather than per-Log because database/sql gives no per-DB hook;
// tests read DELTAS around the call under test and must therefore not run in
// parallel with another test that touches a Log. Nothing in this package
// calls t.Parallel.
var statementsPrepared atomic.Int64

type countingDriver struct{ inner driver.Driver }

func (d countingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return countingConn{c}, nil
}

// countingConn embeds the driver.Conn INTERFACE rather than the concrete
// connection, so its method set is exactly driver.Conn's three methods.
// database/sql's type assertions for QueryerContext, ExecerContext and
// PrepareContext therefore all fail against it, and every query and exec —
// including the ones inside a transaction — is routed through Prepare below
// where it can be counted. Slower than the real path, which is irrelevant:
// this is a counter, not a benchmark.
type countingConn struct{ driver.Conn }

func (c countingConn) Prepare(q string) (driver.Stmt, error) {
	statementsPrepared.Add(1)
	return c.Conn.Prepare(q)
}

var countingOnce sync.Once

// useCountingDriver points reqlog.Open at the counting wrapper for the rest
// of the test.
func useCountingDriver(t *testing.T) {
	t.Helper()
	countingOnce.Do(func() {
		// The wrapper needs the real driver VALUE, and database/sql only
		// exposes it through an open handle.
		probe, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		sql.Register("sqlite-counting", countingDriver{inner: probe.Driver()})
		if err := probe.Close(); err != nil {
			t.Fatal(err)
		}
	})
	prev := sqlDriver
	sqlDriver = "sqlite-counting"
	t.Cleanup(func() { sqlDriver = prev })
}

// countingDriverActuallyCounts guards the guard: if the wrapper silently
// stopped seeing statements, every assertion below would read "0 statements"
// and pass while measuring nothing. Every test that counts calls this first.
func countingDriverActuallyCounts(t *testing.T, l *Log) {
	t.Helper()
	before := statementsPrepared.Load()
	var n int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if got := statementsPrepared.Load() - before; got == 0 {
		t.Fatal("the counting driver counted 0 statements for a query that definitely ran — " +
			"every count in this file would be vacuous")
	}
}

func rowCount(t *testing.T, l *Log) int {
	t.Helper()
	var n int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ── the cost is independent of the row count ─────────────────────────────

// A CacheStats call must execute no SQL at all, and must still execute no
// SQL after the table has grown by an order of magnitude. Zero at both sizes
// is the strongest available form of "the work does not scale with rows":
// there is no work to scale.
//
// The fixture is deliberately large at both ends — tens of thousands of rows
// growing to issue #162's live size — because the defect this replaces was
// invisible on a toy table and only showed up once the scan had six figures
// of rows to walk.
func TestCacheStatsExecutesNoSQLAtAnyTableSize(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large-fixture test in -short mode")
	}
	useCountingDriver(t)
	l := openTest(t)
	countingDriverActuallyCounts(t, l)

	now := time.Now().Truncate(time.Millisecond)
	base := liveFixtureRows / 10
	seedRequests(t, l, base, 17*24*time.Hour, now)
	smallRows := rowCount(t, l)

	before := statementsPrepared.Load()
	start := time.Now()
	small, err := l.CacheStats()
	smallElapsed := time.Since(start)
	smallStmts := statementsPrepared.Load() - before
	if err != nil {
		t.Fatal(err)
	}
	if len(small) != 8 {
		t.Fatalf("CacheStats returned %d accounts, want the fixture's 8", len(small))
	}
	if smallStmts != 0 {
		t.Errorf("CacheStats executed %d SQL statements over %d rows, want 0 — "+
			"it is still reading the table per call (issue #165)", smallStmts, smallRows)
	}

	// Ten times the table, same question.
	seedRequests(t, l, 9*base, 17*24*time.Hour, now)
	bigRows := rowCount(t, l)
	if bigRows < 10*smallRows-10 {
		t.Fatalf("fixture did not grow tenfold: %d rows -> %d", smallRows, bigRows)
	}

	before = statementsPrepared.Load()
	start = time.Now()
	big, err := l.CacheStats()
	bigElapsed := time.Since(start)
	bigStmts := statementsPrepared.Load() - before
	if err != nil {
		t.Fatal(err)
	}
	if bigStmts != 0 {
		t.Errorf("CacheStats executed %d SQL statements over %d rows, want 0", bigStmts, bigRows)
	}
	if bigStmts != smallStmts {
		t.Errorf("CacheStats' statement count scaled with the table: %d statements at %d rows, "+
			"%d at %d rows", smallStmts, smallRows, bigStmts, bigRows)
	}
	if len(big) != len(small) {
		t.Errorf("CacheStats returned %d accounts at %d rows and %d at %d rows",
			len(small), smallRows, len(big), bigRows)
	}
	// And the answer really did change with the rows, so "no statements"
	// cannot be passing because the call is returning a frozen fixture.
	if big[0] == small[0] {
		t.Errorf("%s's totals did not move across a tenfold growth in rows (%+v) — "+
			"the running total is not being maintained", small[0].Account, small[0])
	}
	// Logged, never asserted: a threshold here is a flake, not a fact about
	// the fix (issues #98, #134).
	t.Logf("CacheStats at %d rows %v, at %d rows %v", smallRows, smallElapsed, bigRows, bigElapsed)
}

// ── the number is still the right number ─────────────────────────────────

// sqlAggregate recomputes the lifetime totals the way CacheStats used to,
// straight from the two tables, so the running total can be checked against
// the source of truth it replaced.
func sqlAggregate(t *testing.T, l *Log) map[string]CacheStat {
	t.Helper()
	out, err := l.requestTotals()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := l.db.Query(cacheStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var c CacheStat
		if err := rows.Scan(&c.Account, &c.InputTokens, &c.OutputTokens,
			&c.CacheCreationInputTokens, &c.CacheReadInputTokens); err != nil {
			t.Fatal(err)
		}
		p := out[c.Account]
		c.InputTokens += p.InputTokens
		c.OutputTokens += p.OutputTokens
		c.CacheCreationInputTokens += p.CacheCreationInputTokens
		c.CacheReadInputTokens += p.CacheReadInputTokens
		out[c.Account] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertMatchesSQL(t *testing.T, l *Log, stage string) {
	t.Helper()
	want := sqlAggregate(t, l)
	got, err := l.CacheStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		var missing []string
		for name := range want {
			found := false
			for _, c := range got {
				if c.Account == name {
					found = true
					break
				}
			}
			if !found {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		t.Fatalf("%s: running total has %d accounts, the SQL aggregate has %d; missing %v",
			stage, len(got), len(want), missing)
	}
	for _, c := range got {
		w, ok := want[c.Account]
		if !ok {
			t.Errorf("%s: running total reports account %q that the SQL aggregate does not", stage, c.Account)
			continue
		}
		if c != w {
			t.Errorf("%s: %s's running total %+v does not match the SQL aggregate %+v",
				stage, c.Account, c, w)
		}
	}
}

// The running total is only worth having if it is the same number. Check it
// against the SQL aggregate at every point where it could drift: after a
// bulk seed, after live Records, after a prune has moved counters from
// `requests` into `request_totals`, and after a reopen has re-seeded from
// disk.
func TestCacheStatsMatchesTheSQLAggregate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Millisecond)

	seedRequests(t, l, 20000, 17*24*time.Hour, now)
	assertMatchesSQL(t, l, "after the bulk seed")

	// Live traffic on top, including an account the seed never mentions and
	// a request with no usage counters at all.
	for i := 0; i < 200; i++ {
		if err := l.Record(Entry{
			Ts: now.Add(-time.Duration(i) * time.Minute), Account: fmt.Sprintf("live-%d", i%3),
			Path: "/v1/messages", Status: 200, Event: EventServed,
			InputTokens: int64(i), OutputTokens: int64(2 * i),
			CacheCreationInputTokens: int64(3 * i), CacheReadInputTokens: int64(4 * i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Record(Entry{Ts: now, Account: "silent", Event: EventPassthrough}); err != nil {
		t.Fatal(err)
	}
	assertMatchesSQL(t, l, "after live records")

	// A prune moves counters between the two tables. The combined figure
	// must not move at all — that invariant is what makes the running total
	// exact rather than a cache that needs invalidating.
	beforePrune, err := l.CacheStats()
	if err != nil {
		t.Fatal(err)
	}
	n, err := l.PruneRequests(now.Add(-RequestRetention))
	if err != nil {
		t.Fatal(err)
	}
	if n < 1000 {
		t.Fatalf("prune removed only %d rows — it is not exercising the rollup", n)
	}
	afterPrune, err := l.CacheStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(beforePrune) != len(afterPrune) {
		t.Fatalf("account count changed across the prune: %d -> %d", len(beforePrune), len(afterPrune))
	}
	for i := range beforePrune {
		if beforePrune[i] != afterPrune[i] {
			t.Errorf("%s's lifetime total moved across a %d-row prune: %+v -> %+v",
				beforePrune[i].Account, n, beforePrune[i], afterPrune[i])
		}
	}
	assertMatchesSQL(t, l, "after the prune")

	// And across a restart, where the total is rebuilt from disk alone.
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertMatchesSQL(t, reopened, "after a reopen")

	restarted, err := reopened.CacheStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted) != len(afterPrune) {
		t.Fatalf("reopen changed the account count: %d -> %d", len(afterPrune), len(restarted))
	}
	for i := range afterPrune {
		if afterPrune[i] != restarted[i] {
			t.Errorf("%s's lifetime total did not survive a restart: %+v -> %+v",
				afterPrune[i].Account, afterPrune[i], restarted[i])
		}
	}
}

// Record now writes to shared state, and the proxy calls it from one
// goroutine per in-flight request while the dashboard and the status line
// read it. Exercised under -race, and checked for exactness: a mutex that
// prevents the race but drops an add would still be wrong.
func TestRunningTotalIsConcurrencySafeAndExact(t *testing.T) {
	l := openTest(t)
	const goroutines, each = 8, 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := l.Record(Entry{
					Account: fmt.Sprintf("acct-%d", g%2), Path: "/v1/messages",
					Status: 200, Event: EventServed,
					InputTokens: 1, OutputTokens: 2,
					CacheCreationInputTokens: 3, CacheReadInputTokens: 4,
				}); err != nil {
					t.Errorf("Record: %v", err)
					return
				}
			}
		}(g)
	}
	// Concurrent readers, so the race detector sees the read side too.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if _, err := l.CacheStats(); err != nil {
					t.Errorf("CacheStats: %v", err)
					return
				}
			}
		}
	}()
	wg.Wait()
	close(stop)
	readers.Wait()

	stats, err := l.CacheStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("got %d accounts, want 2: %+v", len(stats), stats)
	}
	wantPerAccount := int64(goroutines / 2 * each)
	for _, c := range stats {
		if c.InputTokens != wantPerAccount || c.OutputTokens != 2*wantPerAccount ||
			c.CacheCreationInputTokens != 3*wantPerAccount ||
			c.CacheReadInputTokens != 4*wantPerAccount {
			t.Errorf("%s = %+v, want input=%d output=%d create=%d read=%d — adds were lost",
				c.Account, c, wantPerAccount, 2*wantPerAccount,
				3*wantPerAccount, 4*wantPerAccount)
		}
	}
	assertMatchesSQL(t, l, "after concurrent records")
}
