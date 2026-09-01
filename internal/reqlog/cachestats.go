package reqlog

// CacheStat is one account's aggregated token volume, for the dashboard's
// cache hit rate and cache-create/cache-read columns (issue #110) — a burn
// spike is explicable once these sit beside burn/h and dry-in, instead of
// being a mystery the operator has to guess at.
//
// No time window: the requests table is not pruned (unlike quota_samples —
// see QuotaRetention), so a lifetime aggregate is what's actually there.
// Once #110's numbers show whether a rolling window is worth the added
// complexity, this is the place to add one.
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

// CacheStats aggregates token usage per account across every logged
// request. Accounts with no requests yet are simply absent — the caller
// (admin.accounts) already iterates the pool's account list and treats a
// missing entry as "no data", the same way it already treats a missing
// quota-history series.
func (l *Log) CacheStats() ([]CacheStat, error) {
	rows, err := l.db.Query(`SELECT account,
		SUM(input_tokens), SUM(output_tokens),
		SUM(cache_creation_input_tokens), SUM(cache_read_input_tokens)
		FROM requests GROUP BY account`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheStat
	for rows.Next() {
		var c CacheStat
		if err := rows.Scan(&c.Account, &c.InputTokens, &c.OutputTokens,
			&c.CacheCreationInputTokens, &c.CacheReadInputTokens); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RotationCost splits cache-creation-token volume into what followed a
// same-session account change ("Rotated") versus everything else
// ("Stable") — the direct answer to issue #110's second question: whether a
// cache-create spike is attributable to rotation rather than to anything
// else. Requests with no session hash (identity/passthrough paths, or a
// database from before this column existed) are excluded from both, since
// there is nothing to compare them against.
//
// This is the schema's answer to "can it attribute cost to rotation": yes,
// via session_hash plus account, ordered by time within each session. It is
// not wired into the dashboard (out of scope for a proportionate change —
// see the PR description) but is exercised directly by
// TestRotationCostAttributesCorrectly.
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
