package reqlog

import (
	"sort"
)

// CacheStat is one account's aggregated token volume, for the dashboard's
// cache hit rate and cache-create/cache-read columns (issue #110) — a burn
// spike is explicable once these sit beside burn/h and dry-in, instead of
// being a mystery the operator has to guess at.
//
// No time window: this is a lifetime total, and it stays one even though
// the requests table is now pruned (issue #162, RequestRetention). It used
// to be lifetime only because nothing ever deleted a row; it is lifetime
// now because PruneRequests folds each account's four counters into
// request_totals in the same transaction that deletes the rows, and
// seedTotals adds that rollup back in below. The window over which the
// numbers were collected is therefore unbounded, while the rows scanned to
// produce them are bounded by RequestRetention — and since issue #165 they
// are scanned once per process rather than once per request.
//
// If a ROLLING window is ever wanted instead, it cannot be had by simply
// filtering on ts — that would count the rollup twice over, or read as a
// window while silently including everything before it. Drop the rollup
// from that query in the same change, and note that a rolling window is no
// longer maintainable incrementally at all (rows leaving the window would
// have to be subtracted), so it would mean going back to a per-call scan or
// to a different data structure — see the running-total note below.
type CacheStat struct {
	Account                  string
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

// HitRate is cache_read / (cache_read + cache_creation) — the fraction of
// cacheable input that actually hit the cache rather than paying to write
// it. nil (not zero) when there is no cache volume yet to divide, so the
// dashboard can render "—" instead of a misleading 0%.
func (c CacheStat) HitRate() *float64 {
	total := c.CacheReadInputTokens + c.CacheCreationInputTokens
	if total == 0 {
		return nil
	}
	r := float64(c.CacheReadInputTokens) / float64(total)
	return &r
}

// cacheStatsQuery is a package-level const so a test can run EXPLAIN QUERY
// PLAN against the exact text seedTotals executes rather than a copy that
// could drift from it (#104's shape, reused by #162).
//
// Naming the four SUM columns after the group key in requests_account_tokens
// (see requestIndexes) makes this a COVERING index scan: the plan reads
// `SCAN requests USING COVERING INDEX requests_account_tokens`, answering
// the aggregate from the index without visiting the 23-column rows.
//
// Since issue #165 this runs ONCE per process, in seedTotals at Open, rather
// than on every /api/accounts request — but it still runs over the whole
// table, so the covering plan is still what keeps that one scan bounded.
const cacheStatsQuery = `SELECT account,
		SUM(input_tokens), SUM(output_tokens),
		SUM(cache_creation_input_tokens), SUM(cache_read_input_tokens)
		FROM requests GROUP BY account`

// ── the running total (issue #165) ───────────────────────────────────────
//
// CacheStats used to execute cacheStatsQuery on every call. Even on #163's
// covering index that is O(rows): 695,969 rows measured 0.068 s in C SQLite
// and 0.747 s under modernc.org/sqlite (issue #104's pure-Go amplification
// again). /api/accounts is polled every 5 s by the dashboard AND on every
// render by the status line, whose whole HTTP budget is 350 ms — so the
// status line stopped rendering at all.
//
// These are display totals that move only when a request is logged, so they
// are maintained incrementally instead: one scan at Open seeds them, Record
// adds to them, and CacheStats reads them. That is EXACT, not an
// approximation, because of a property the schema already had:
//
//	lifetime total = SUM(requests) + SUM(request_totals)
//
// and PruneRequests is sum-preserving over that pair by construction — the
// same transaction that deletes rows adds their counters to request_totals
// (see retention.go). So the only event that can move the combined number
// is a Record. Nothing else needs to invalidate anything, and there is no
// refresh interval and therefore no staleness: a memoised copy refreshed on
// the 30 s quota sampler would have been up to 30 s behind a dashboard that
// polls every 5 s, which is a worse number to put on screen than a slow one.
//
// The correctness of this rests on Record being the only writer of
// `requests` rows on a given Log, which it is: one process opens the
// database (cmd/spillway/main.go), and every other surface — `spillway
// status`, the status line, the dashboard — reads it through the admin API.
// Test fixtures that bulk-insert rows behind Record's back call seedTotals
// afterwards, exactly as Open does, and TestCacheStatsMatchesTheSQLAggregate
// pins the in-memory figure against the SQL truth so any drift fails loudly
// rather than being displayed.

// seedTotals computes the lifetime per-account totals once and installs them
// as the live figure. Called from Open AFTER retention has pruned — prune
// moves counters between the two tables without changing their sum, so the
// order is immaterial to the answer, but scanning the smaller table is
// cheaper and the dependency is easier to reason about this way.
func (l *Log) seedTotals() error {
	// The rollup of already-pruned rows first, then everything still in the
	// table added on top. Added in Go rather than folded into the SQL as a
	// UNION ALL subquery on purpose: a subquery costs the covering-index
	// plan above, while this way the expensive half keeps a plan a test can
	// assert, and the other half is a handful of rows.
	totals, err := l.requestTotals()
	if err != nil {
		return err
	}
	rows, err := l.db.Query(cacheStatsQuery)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c CacheStat
		if err := rows.Scan(&c.Account, &c.InputTokens, &c.OutputTokens,
			&c.CacheCreationInputTokens, &c.CacheReadInputTokens); err != nil {
			return err
		}
		// An account whose every row has been pruned is already in the map
		// from the rollup and must keep those counters: dropping it would
		// make the displayed number fall to zero after a quiet fortnight
		// rather than hold at what was spent.
		p := totals[c.Account]
		c.InputTokens += p.InputTokens
		c.OutputTokens += p.OutputTokens
		c.CacheCreationInputTokens += p.CacheCreationInputTokens
		c.CacheReadInputTokens += p.CacheReadInputTokens
		totals[c.Account] = c
	}
	if err := rows.Err(); err != nil {
		return err
	}
	l.totalsMu.Lock()
	l.totals = totals
	l.totalsMu.Unlock()
	return nil
}

// addTotals folds one recorded request into the running figure. Called by
// Record after the row is safely written, so a failed INSERT cannot inflate
// a total that no row backs.
//
// The account is registered even when all four counters are zero, matching
// what the SQL aggregate did: `GROUP BY account` produced a row for every
// account with any request at all, and admin.accounts distinguishes "present
// with nothing cached" from "absent" when it decides whether to emit the
// cache columns.
func (l *Log) addTotals(e Entry) {
	l.totalsMu.Lock()
	defer l.totalsMu.Unlock()
	if l.totals == nil {
		l.totals = map[string]CacheStat{}
	}
	c := l.totals[e.Account]
	c.Account = e.Account
	c.InputTokens += e.InputTokens
	c.OutputTokens += e.OutputTokens
	c.CacheCreationInputTokens += e.CacheCreationInputTokens
	c.CacheReadInputTokens += e.CacheReadInputTokens
	l.totals[e.Account] = c
}

// CacheStats returns token usage per account across every logged request —
// including requests whose rows retention has since deleted, whose counters
// live on in request_totals (see PruneRequests). Accounts with no requests
// yet are simply absent — the caller (admin.accounts) already iterates the
// pool's account list and treats a missing entry as "no data", the same way
// it already treats a missing quota-history series.
//
// Reads the running total described above: no query, no row scan, and a
// cost that depends on the number of ACCOUNTS rather than the number of
// requests. The figure is current as of the last Record, never stale.
//
// The error return is kept although this can no longer fail: every caller
// already handles it, and the signature should not have to change again if
// a future total needs a read that can.
func (l *Log) CacheStats() ([]CacheStat, error) {
	l.totalsMu.Lock()
	out := make([]CacheStat, 0, len(l.totals))
	for _, c := range l.totals {
		out = append(out, c)
	}
	l.totalsMu.Unlock()
	// Map order is not deterministic and the dashboard renders these in the
	// order they arrive, so sort before handing them out.
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// RotationCost splits cache-creation-token volume into what followed a
// same-session account change ("Rotated") versus everything else
// ("Stable") — the direct answer to issue #110's second question: whether a
// cache-create spike is attributable to rotation rather than to anything
// else. Requests with no session hash (identity/passthrough paths, or a
// database from before this column existed) are excluded from both, since
// there is nothing to compare them against.
//
// Unlike CacheStats above, this is NOT a lifetime figure: it pairs
// consecutive rows, so it covers only what the table still holds — the last
// RequestRetention (issue #162). request_totals cannot restore the
// difference, because a rotation cost is a property of two adjacent rows
// and the rollup keeps sums, not rows.
//
// This is the schema's answer to "can it attribute cost to rotation": yes,
// via session_hash plus account, ordered by time within each session. It is
// not wired into the dashboard (out of scope for a proportionate change —
// see the PR description) but is exercised directly by
// TestRotationCostAttributesCorrectly.
//
// HEURISTIC, and knowingly so (issue #141). session_hash is per Claude Code
// SESSION, which is the finest boundary anything on the wire offers — it is
// not per conversation. Subagents of one session share their parent's id, a
// resume or a /clear may reuse it, and a compaction certainly does. So
// consecutive rows in one bucket can be two concurrent agents interleaving,
// and an account change between them is attributed as a rotation cost when
// it may just be two agents on two accounts. Before #141 the bucket was the
// whole client (2 distinct values over 5,637 requests), which made this
// wrong far more often; it is now approximate rather than meaningless.
type RotationCost struct {
	RotatedCacheCreationTokens int64
	StableCacheCreationTokens  int64
}

func (l *Log) RotationCost() (RotationCost, error) {
	rows, err := l.db.Query(`SELECT session_hash, account, cache_creation_input_tokens
		FROM requests
		WHERE session_hash != ''
		ORDER BY session_hash ASC, ts ASC, rowid ASC`)
	if err != nil {
		return RotationCost{}, err
	}
	defer rows.Close()

	var out RotationCost
	var lastSession, lastAccount string
	haveLast := false
	for rows.Next() {
		var session, account string
		var create int64
		if err := rows.Scan(&session, &account, &create); err != nil {
			return RotationCost{}, err
		}
		switch {
		case !haveLast || session != lastSession:
			// First request seen for this session: nothing to rotate away
			// from, so it can't be a rotation cost.
			out.StableCacheCreationTokens += create
		case account != lastAccount:
			out.RotatedCacheCreationTokens += create
		default:
			out.StableCacheCreationTokens += create
		}
		lastSession, lastAccount, haveLast = session, account, true
	}
	return out, rows.Err()
}
