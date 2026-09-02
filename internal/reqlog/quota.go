package reqlog

// Quota history: the request log's second table. Headroom over time is the
// one thing the dashboard cannot derive from live state alone — a burn rate
// needs a past to measure against, and that is what turns "34% used" into
// "dry in 2h10m, before its 3h22m refill".

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Sample is one observation of one quota window.
//
// Ts and FetchedAt answer different questions (issue #138). Ts is when
// spillway's sampler wrote this row — it exists for pruning and for
// throttling how often a row lands (sampleInterval), and it advances on
// every sampler tick regardless of whether the window itself was
// re-measured. FetchedAt is when the *provider* actually reported this
// reading — the header's own timestamp, carried through from
// pool.QuotaWindow.FetchedAt — and it only changes when a real response (or
// probe, or poll) touches the window. Before this field existed, SeedQuota
// had only Ts to install as a window's FetchedAt, so a window that hadn't
// been re-measured in days still seeded looking as fresh as the last
// sampler tick: exactly the illusion issue #135's fix depends on FetchedAt
// NOT being under (retireCycleSiblingsLocked compares FetchedAt values to
// tell "re-measured by this response" from "just sitting there").
//
// FetchedAt is nullable at the SQL layer (see migration below): a row
// written before this column existed carries no honest measurement time at
// all, and guessing one — falling back to Ts, say — would silently
// reintroduce the exact illusion this field exists to remove. Such a row's
// FetchedAt comes back as the zero time.Time, which every consumer already
// treats as "unknown, treat as maximally stale" (the same value
// QuotaWindow.FetchedAt would carry for a window that has genuinely never
// been measured) — never as fresh.
type Sample struct {
	Ts        time.Time `json:"ts"`
	Account   string    `json:"account"`
	Window    string    `json:"window"`
	Limit     float64   `json:"limit"`
	Used      float64   `json:"used"`
	ResetAt   time.Time `json:"resetAt,omitempty"`
	FetchedAt time.Time `json:"fetchedAt,omitempty"`
}

// sampleInterval throttles writes: quota headers arrive on every response,
// but a point per account/window per minute is plenty to draw a curve.
const sampleInterval = time.Minute

// QuotaRetention bounds how long quota_samples is kept. The dashboard's
// oldest query (/api/quota-history?hours=168, admin.hoursParam's cap) is 7
// days, so this keeps a full 2x buffer past the longest thing that reads it.
// Issue #104: with nothing pruning it, the table grew from 1.4MB to 40MB (8
// account/window pairs, 57,617 rows) in a week, which is what made the
// unindexed startup query slow enough to hang the daemon.
const QuotaRetention = 14 * 24 * time.Hour

func (l *Log) initQuota() error {
	_, err := l.db.Exec(`CREATE TABLE IF NOT EXISTS quota_samples (
		ts INTEGER NOT NULL,
		account TEXT NOT NULL,
		window TEXT NOT NULL,
		lim REAL NOT NULL,
		used REAL NOT NULL,
		reset_at INTEGER
	)`)
	if err != nil {
		return err
	}
	if _, err = l.db.Exec(`CREATE INDEX IF NOT EXISTS quota_ts ON quota_samples (ts)`); err != nil {
		return err
	}
	// Issue #138: fetched_at carries the provider's own measurement time,
	// separate from ts (see Sample's doc). Added after the first release —
	// existing databases need it too — so this runs unconditionally, the
	// same pattern reqlog.go's Open uses for the requests table's own
	// ALTER-added columns: tolerate "duplicate column" so reopening an
	// already-migrated database is a no-op, not an error. Nullable and with
	// no DEFAULT, deliberately: a row that predates this column has no
	// honest measurement time to default to, and NULL is what lets
	// scanSamples tell "never recorded" apart from "recorded as exactly
	// this timestamp".
	if _, err = l.db.Exec(`ALTER TABLE quota_samples ADD COLUMN fetched_at INTEGER`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return err
	}
	// Issue #104: LatestQuotaSamples (startup seeding) filters on
	// (account, window); quota_ts alone can't help that, so the query
	// devolved into a per-row correlated subquery over the whole table —
	// 34s at 57,617 rows. CREATE INDEX IF NOT EXISTS is idempotent and
	// harmless if a DB already has an equivalent index under a different
	// name (one was added by hand as a stopgap): sqlite just carries two.
	if _, err = l.db.Exec(`CREATE INDEX IF NOT EXISTS quota_account_window_ts
		ON quota_samples (account, window, ts)`); err != nil {
		return err
	}
	// Bound the table itself, not just the index that makes scanning it
	// fast — see QuotaRetention. Run at every open, not only on a timer, so
	// a database that only ever sees short-lived restarts (the Homebrew
	// cask restarts the service on every upgrade, issue #34) still gets
	// pruned regularly.
	if _, err = l.db.Exec(`DELETE FROM quota_samples WHERE ts < ?`,
		time.Now().Add(-QuotaRetention).UnixMilli()); err != nil {
		return err
	}
	// Kimi's 7-day window used to be recorded as "weekly", before window
	// names were normalised onto Claude's 5h/7d vocabulary. The name is part
	// of the row, so without this the same window draws as two series that
	// each stop or start on the day of the upgrade. Idempotent, and a no-op
	// on every database that never held a Kimi account.
	if _, err = l.db.Exec(`UPDATE quota_samples SET window = '7d' WHERE window = 'weekly'`); err != nil {
		return err
	}
	// "parallel" was Kimi's concurrency cap recorded as a quota window. It
	// had no used value, so every sample is the same flat line at zero
	// against a limit — nothing a chart can say anything with.
	_, err = l.db.Exec(`DELETE FROM quota_samples WHERE window = 'parallel'`)
	return err
}

// RecordQuota writes a sample unless one for this account+window landed
// within sampleInterval.
func (l *Log) RecordQuota(s Sample) error {
	if s.Ts.IsZero() {
		s.Ts = time.Now()
	}
	var last int64
	err := l.db.QueryRow(`SELECT ts FROM quota_samples WHERE account = ? AND window = ?
		ORDER BY ts DESC LIMIT 1`, s.Account, s.Window).Scan(&last)
	if err == nil && s.Ts.Sub(time.UnixMilli(last)) < sampleInterval {
		return nil
	}
	var reset any
	if !s.ResetAt.IsZero() {
		reset = s.ResetAt.UnixMilli()
	}
	var fetchedAt any
	if !s.FetchedAt.IsZero() {
		fetchedAt = s.FetchedAt.UnixMilli()
	}
	_, err = l.db.Exec(`INSERT INTO quota_samples (ts, account, window, lim, used, reset_at, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.Ts.UnixMilli(), s.Account, s.Window, s.Limit, s.Used, reset, fetchedAt)
	return err
}

// QuotaSince returns samples newer than cutoff, oldest first.
func (l *Log) QuotaSince(cutoff time.Time) ([]Sample, error) {
	rows, err := l.db.Query(`SELECT ts, account, window, lim, used, reset_at, fetched_at
		FROM quota_samples WHERE ts >= ? ORDER BY ts ASC`, cutoff.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSamples(rows)
}

// LatestQuotaSamples returns the newest recorded sample for each
// account/window pair — last-known state, read once at startup to seed
// in-memory quota windows before anything has probed or polled (issue #34:
// without this, an account with no reading at all is probed unconditionally,
// including when that would bill).
//
// Issue #104: this used to be a correlated subquery
// (`WHERE ts = (SELECT MAX(ts) FROM quota_samples q2 WHERE q2.account =
// q.account AND q2.window = q.window)`), which nothing indexed — SQLite ran
// the inner MAX() once per outer row, an O(n²)-ish scan that took 34.3s over
// 57,617 rows on the live database to produce 8 result rows. It is rewritten
// here as a join against a derived table of per-(account, window) maximums,
// which is the same computation without repeating it once per row: the
// GROUP BY runs a single pass over the table, and the join back to the base
// table (to recover lim/used/reset_at, and to preserve every row exactly at
// the max — see below) is driven by the quota_account_window_ts index. The
// two forms are provably equivalent: "ts equals the max ts for this
// account+window" is exactly what both the correlated subquery and the
// derived-table join test, row by row.
//
// This includes ties on ts deliberately: if two rows for the same
// account+window happen to share the newest ts, the original query returned
// both (its WHERE clause matches every row at the max, not just one), and a
// naive `SELECT account, window, MAX(ts), lim, used, reset_at ... GROUP BY
// account, window` would instead silently pick one of them (SQLite's
// bare-column-with-aggregate extension resolves ties arbitrarily). The join
// form preserves the original's "return every tied row" behavior.
//
// latestQuotaSamplesQuery is a package-level const (rather than inlined)
// so a test can run `EXPLAIN QUERY PLAN` against the exact text this
// executes, instead of a hand-maintained copy that could drift from it.
func (l *Log) LatestQuotaSamples(ctx context.Context) ([]Sample, error) {
	rows, err := l.db.QueryContext(ctx, latestQuotaSamplesQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSamples(rows)
}

const latestQuotaSamplesQuery = `
	SELECT q.ts, q.account, q.window, q.lim, q.used, q.reset_at, q.fetched_at
	FROM quota_samples q
	JOIN (
		SELECT account, window, MAX(ts) AS max_ts
		FROM quota_samples
		GROUP BY account, window
	) latest
		ON latest.account = q.account
		AND latest.window = q.window
		AND latest.max_ts = q.ts`

// PruneQuota deletes quota_samples older than cutoff and reports how many
// rows it removed. Called once at Open (via initQuota) and periodically by
// the daemon (main), so both a long-lived process and one that only ever
// sees short restarts stay bounded — see QuotaRetention.
func (l *Log) PruneQuota(cutoff time.Time) (int64, error) {
	res, err := l.db.Exec(`DELETE FROM quota_samples WHERE ts < ?`, cutoff.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanSamples(rows *sql.Rows) ([]Sample, error) {
	var out []Sample
	for rows.Next() {
		var s Sample
		var ts int64
		var reset, fetchedAt *int64
		if err := rows.Scan(&ts, &s.Account, &s.Window, &s.Limit, &s.Used, &reset, &fetchedAt); err != nil {
			return nil, err
		}
		s.Ts = time.UnixMilli(ts)
		if reset != nil {
			s.ResetAt = time.UnixMilli(*reset)
		}
		// NULL (a row from before fetched_at existed) leaves s.FetchedAt at
		// its zero value — see Sample's doc for why that must never be
		// treated as fresh.
		if fetchedAt != nil {
			s.FetchedAt = time.UnixMilli(*fetchedAt)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Bucket is one time bucket of request activity.
type Bucket struct {
	Ts      time.Time `json:"ts"`
	Count   int       `json:"count"`
	Errors  int       `json:"errors"`
	Rotated int       `json:"rotated"`
	P95Ms   int64     `json:"p95_ms"`
}

// Activity buckets requests since cutoff into fixed-width slots. Empty slots
// are included so the histogram shows real gaps rather than compressing time.
func (l *Log) Activity(cutoff time.Time, width time.Duration, now time.Time) ([]Bucket, error) {
	if width <= 0 {
		width = time.Minute
	}
	// Stored timestamps are millisecond-truncated; comparing them against a
	// nanosecond-precision cutoff drops boundary rows into the bucket before.
	cutoff = cutoff.Truncate(time.Millisecond)
	rows, err := l.db.Query(`SELECT ts, status, event, duration_ms FROM requests
		WHERE ts >= ? ORDER BY ts ASC`, cutoff.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	n := int(now.Sub(cutoff)/width) + 1
	if n < 1 {
		n = 1
	}
	out := make([]Bucket, n)
	durs := make([][]int64, n)
	for i := range out {
		out[i].Ts = cutoff.Add(time.Duration(i) * width)
	}
	for rows.Next() {
		var ts, dur int64
		var status int
		var event string
		if err := rows.Scan(&ts, &status, &event, &dur); err != nil {
			return nil, err
		}
		i := int(time.UnixMilli(ts).Sub(cutoff) / width)
		if i < 0 || i >= n {
			continue
		}
		out[i].Count++
		if status >= 400 {
			out[i].Errors++
		}
		if event == EventRotatedQuota || event == EventRotatedConn {
			out[i].Rotated++
		}
		durs[i] = append(durs[i], dur)
	}
	for i := range out {
		out[i].P95Ms = p95(durs[i])
	}
	return out, rows.Err()
}

func p95(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	for i := 1; i < len(v); i++ { // insertion sort: buckets are tiny
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
	idx := (len(v)*95 + 99) / 100
	if idx >= len(v) {
		idx = len(v) - 1
	}
	return v[idx]
}
