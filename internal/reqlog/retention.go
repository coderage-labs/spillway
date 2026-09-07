package reqlog

// Retention for the `requests` table — issue #162.
//
// Until now nothing pruned it. That was not an oversight: CacheStats is
// displayed as a LIFETIME per-account total (admin.accountJSON's
// CacheHitRate/CacheCreateTokens/CacheReadTokens), and with every row kept
// forever a lifetime aggregate was simply what the table contained.
//
// The cost of that choice had become the whole issue. 584,114 rows / 112MB
// after 17 days on one real installation, growing at ~34k rows a day, and
// CacheStats scans all of them on every 5-second dashboard poll — an
// indexed scan is still O(rows), so indexing alone buys a constant factor
// and then the same curve resumes.
//
// Three honest options existed: keep every row and pay forever; prune and
// let the displayed number quietly become "recent"; or prune while carrying
// the pruned rows' sums forward, so the aggregate outlives its rows. The
// third is what this implements, because the second silently changes what a
// number on screen means, and a lifetime total is exactly the kind of
// figure an operator reads across a restart. PruneRequests folds each
// account's four counters into request_totals in the same transaction that
// deletes the rows, and CacheStats adds that table back in — so "lifetime"
// stays true, and the per-poll scan stays bounded by the retention window.
//
// What retention DOES change is the row-level queries, and those say so at
// their own definitions: PrefixDrift and RotationCost walk consecutive
// requests, so both now describe the retention window rather than all of
// history.
//
// request_totals widens the redaction surface by nothing: it holds an
// account name and SUMS of the same four #110 usage counters `requests`
// already stores, and no other column — pinned by
// TestRequestTotalsSchemaIsFourCountersAndAnAccount, the same way
// TestRedactionBySchema pins the requests table. Retention strictly reduces
// what is on disk.

import (
	"time"
)

// RequestRetention bounds how long `requests` rows are kept, mirroring
// QuotaRetention's reasoning: the longest thing that reads the table by
// time is the activity histogram at ?hours=168 (admin.hoursParam's cap), so
// 14 days keeps a full 2x margin past it.
//
// Token totals are NOT bounded by this — see PruneRequests, which rolls
// them into request_totals before deleting anything.
const RequestRetention = 14 * 24 * time.Hour

// initRequestRetention creates the rollup table and prunes once, at every
// open. Pruning here rather than only on a timer is the same reasoning
// #104 applied to quota_samples: a daemon that only ever sees short-lived
// restarts (the Homebrew cask restarts the service on every upgrade, issue
// #34) would otherwise never prune at all.
func (l *Log) initRequestRetention() error {
	// One row per account, holding the summed counters of every request row
	// that retention has since deleted. account is the PRIMARY KEY so the
	// upsert in PruneRequests has something to conflict on.
	if _, err := l.db.Exec(`CREATE TABLE IF NOT EXISTS request_totals (
		account TEXT PRIMARY KEY,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read_input_tokens INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		return err
	}
	_, err := l.PruneRequests(time.Now().Add(-RequestRetention))
	return err
}

// PruneRequests deletes `requests` rows older than cutoff and reports how
// many it removed, after folding their token counters into request_totals
// so CacheStats' lifetime totals survive the deletion.
//
// Both halves run in ONE transaction, which is what makes the totals exact:
// the same cutoff selects the rows to sum and the rows to delete, so no row
// can be counted twice (summed, then left behind to be summed again on the
// next prune) and none can vanish uncounted. A failure anywhere rolls both
// back, leaving the table and the rollup consistent with each other.
//
// Called once at Open (via initRequestRetention) and periodically by the
// daemon (main), so a long-lived process and one that only ever sees short
// restarts both stay bounded.
func (l *Log) PruneRequests(cutoff time.Time) (int64, error) {
	tx, err := l.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	ms := cutoff.UnixMilli()
	// Unqualified column names on the right of DO UPDATE SET refer to the
	// existing row; they are written out as request_totals.x anyway so the
	// "old + new" reading needs no knowledge of that rule.
	if _, err := tx.Exec(`INSERT INTO request_totals
		(account, input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens)
		SELECT account, SUM(input_tokens), SUM(output_tokens),
			SUM(cache_creation_input_tokens), SUM(cache_read_input_tokens)
		FROM requests WHERE ts < ? GROUP BY account
		ON CONFLICT(account) DO UPDATE SET
			input_tokens = request_totals.input_tokens + excluded.input_tokens,
			output_tokens = request_totals.output_tokens + excluded.output_tokens,
			cache_creation_input_tokens = request_totals.cache_creation_input_tokens
				+ excluded.cache_creation_input_tokens,
			cache_read_input_tokens = request_totals.cache_read_input_tokens
				+ excluded.cache_read_input_tokens`, ms); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM requests WHERE ts < ?`, ms)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// requestTotals reads the rollup: the summed counters of every request row
// retention has deleted, per account. Accounts whose rows have never been
// pruned are simply absent.
func (l *Log) requestTotals() (map[string]CacheStat, error) {
	rows, err := l.db.Query(`SELECT account, input_tokens, output_tokens,
		cache_creation_input_tokens, cache_read_input_tokens FROM request_totals`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CacheStat{}
	for rows.Next() {
		var c CacheStat
		if err := rows.Scan(&c.Account, &c.InputTokens, &c.OutputTokens,
			&c.CacheCreationInputTokens, &c.CacheReadInputTokens); err != nil {
			return nil, err
		}
		out[c.Account] = c
	}
	return out, rows.Err()
}
