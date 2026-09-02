package reqlog

// Prefix-instability report — issue #111, phase 1.
//
// #111 proposes six request transforms on the strength of a third party's
// numbers. This is the query that says which of the instabilities those
// transforms target actually occurs in THIS pool's traffic, and what each
// one costs in cache-creation tokens. Phase 2 builds only what this
// justifies.
//
// Method: walk the fingerprinted requests of each session in time order and
// compare each request with the one before it. Every consecutive pair is
// classified by WHICH fingerprints changed across it, and the later
// request's cache-creation and cache-read volume is attributed to each kind
// of change it exhibited.
//
// Two things to hold in mind when reading the output.
//
// The kinds are NOT mutually exclusive. One turn can change the system
// block and reorder its tools at the same time, and it counts under both;
// what the report answers is "how often did THIS region change, and how
// much cache-creation accompanied it", not "which single cause was it".
// KindStable is the exception and the control: no comparable fingerprint
// changed at all.
//
// Correlation, not causation, and deliberately so. A turn that adds a
// message also invalidates everything after the changed byte, so a pair
// with no prefix change but a big cache-creation number is not a
// contradiction — it is the ordinary cost of a growing conversation, and it
// is exactly the baseline KindStable exists to provide. What would justify
// phase 2's tool sorting is KindToolsOrderOnly happening at all, with
// cache-creation volume attached; what would sink it is that bucket being
// empty while KindStable carries the cost.
const (
	// KindToolsOrderOnly: the ordered tool-name hash changed while the
	// sorted one did not. Identical tool SET, different ORDER — the one
	// instability #111's "deterministically sort tools" would fix, and the
	// single most important number in this report.
	KindToolsOrderOnly = "tools-order-only"
	// KindToolsSet: the sorted tool-name hash changed, i.e. the set of
	// tools genuinely differs. Sorting cannot help this.
	KindToolsSet = "tools-set"
	// KindToolsRaw: the tools array's verbatim bytes changed while the
	// names (ordered) did not — a description, a schema, or whitespace
	// moved. Same set, same order, different bytes: still a cache bust,
	// and not something sorting addresses either.
	KindToolsRaw = "tools-raw"
	// KindSystem: the system block's bytes changed. #111's "fingerprint
	// instability" (a shifting cc_version) would show up here.
	KindSystem = "system"
	// KindFirstMsgShape: messages[0]'s content-block type sequence changed.
	// #111's "block scatter" would show up here.
	KindFirstMsgShape = "first-message-shape"
	// KindAny: at least one comparable fingerprint changed. The union, for
	// use as a denominator.
	KindAny = "any-change"
	// KindStable: no comparable fingerprint changed. The control group —
	// cache-creation volume here is what a stable prefix costs anyway.
	KindStable = "none"
)

// prefixDriftKinds is the fixed order rows come back in. Fixed so the report
// has a stable shape whether or not a given kind was ever observed, and so
// callers never have to range a map to render it.
var prefixDriftKinds = []string{
	KindToolsOrderOnly, KindToolsSet, KindToolsRaw,
	KindSystem, KindFirstMsgShape, KindAny, KindStable,
}

// PrefixChange is one (kind, account-changed) cell of the report.
//
// AccountChanged separates spillway's own rotation cost from in-session
// prefix churn: the prompt cache is per account, so a pair that crossed a
// rotation would have paid cache-creation regardless of what the client did
// to its prefix. #111's own point 5 asks precisely this — if rotation
// dominates, stabilising the prefix matters less than rotating less.
type PrefixChange struct {
	Kind                     string `json:"kind"`
	AccountChanged           bool   `json:"account_changed"`
	Pairs                    int64  `json:"pairs"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
	// PairsMissingUsage is how many of Pairs contributed NO usage figures
	// at all — every one of the four counters zero on the later request,
	// which for a served response means the sniffer could not read the
	// body rather than that the request was free.
	//
	// This column exists because of issue #126: Anthropic has started
	// returning Content-Encoding: br on some responses and the usage
	// sniffer only decodes gzip, so a slice of real traffic records zeros
	// (48 of 1399 /v1/messages rows, measured 2026-09-02). The fingerprints
	// are derived from the REQUEST and are recorded regardless, so those
	// rows still classify correctly here — but their token totals are
	// missing, and missing selectively. Averaging over that silently would
	// bias the phase-2 decision, so the coverage hole is reported as a
	// number beside the totals rather than left to be discovered.
	PairsMissingUsage int64 `json:"pairs_missing_usage"`
}

// PrefixDrift returns the full report: every kind in prefixDriftKinds, once
// for AccountChanged=false and once for true, in that fixed order, with
// zeros for kinds never observed.
//
// Only requests that carry a fingerprint at all participate — a request
// whose body was over the buffering cap, or was not parseable, has nothing
// to compare and is skipped entirely rather than counted as a change.
// Likewise an individual fingerprint is compared only when BOTH sides of the
// pair have it: comparing a recorded hash against an absent one would
// manufacture changes out of missing data, which is how #110 came to record
// zeros and be believed.
func (l *Log) PrefixDrift() ([]PrefixChange, error) {
	rows, err := l.db.Query(`SELECT session_hash, account,
		tools_order_hash, tools_sorted_hash, tools_raw_hash, system_hash, first_msg_shape_hash,
		cache_creation_input_tokens, cache_read_input_tokens,
		input_tokens, output_tokens
		FROM requests
		WHERE session_hash != ''
		  AND (tools_order_hash != '' OR system_hash != '' OR first_msg_shape_hash != '')
		ORDER BY session_hash ASC, ts ASC, rowid ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Indexed by kind then by accountChanged, so accumulation never depends
	// on map iteration order and the output below is deterministic.
	type cell struct{ pairs, create, read, missing int64 }
	acc := map[string][2]cell{}
	add := func(kind string, accountChanged bool, r rowUsage) {
		v := acc[kind]
		i := 0
		if accountChanged {
			i = 1
		}
		v[i].pairs++
		v[i].create += r.create
		v[i].read += r.read
		if r.usageMissing() {
			v[i].missing++
		}
		acc[kind] = v
	}

	type row struct {
		session, account                     string
		order, sorted, raw, system, msgShape string
		rowUsage
	}
	var prev row
	havePrev := false

	for rows.Next() {
		var r row
		if err := rows.Scan(&r.session, &r.account,
			&r.order, &r.sorted, &r.raw, &r.system, &r.msgShape,
			&r.create, &r.read, &r.input, &r.output); err != nil {
			return nil, err
		}
		if !havePrev || r.session != prev.session {
			// First fingerprinted request of a session: no predecessor, so
			// nothing to call changed or unchanged.
			prev, havePrev = r, true
			continue
		}

		changedAccount := r.account != prev.account
		orderCmp, orderChanged := compare(prev.order, r.order)
		sortedCmp, sortedChanged := compare(prev.sorted, r.sorted)
		rawCmp, rawChanged := compare(prev.raw, r.raw)
		sysCmp, sysChanged := compare(prev.system, r.system)
		shapeCmp, shapeChanged := compare(prev.msgShape, r.msgShape)

		anyComparable := orderCmp || sortedCmp || rawCmp || sysCmp || shapeCmp
		any := false

		// Pure ordering jitter: same set, different order. Requires BOTH
		// hashes to be comparable — an ordered hash that changed while the
		// sorted one is missing says nothing about the set.
		if orderCmp && sortedCmp && orderChanged && !sortedChanged {
			add(KindToolsOrderOnly, changedAccount, r.rowUsage)
		}
		if sortedCmp && sortedChanged {
			add(KindToolsSet, changedAccount, r.rowUsage)
		}
		// Raw bytes moved without the ordered name list moving: a
		// description or schema edit, distinct from both cases above.
		if rawCmp && rawChanged && orderCmp && !orderChanged {
			add(KindToolsRaw, changedAccount, r.rowUsage)
		}
		if sysCmp && sysChanged {
			add(KindSystem, changedAccount, r.rowUsage)
		}
		if shapeCmp && shapeChanged {
			add(KindFirstMsgShape, changedAccount, r.rowUsage)
		}
		if orderChanged || sortedChanged || rawChanged || sysChanged || shapeChanged {
			any = true
		}
		switch {
		case any:
			add(KindAny, changedAccount, r.rowUsage)
		case anyComparable:
			add(KindStable, changedAccount, r.rowUsage)
		}
		// A pair with nothing comparable at all falls through to neither
		// bucket, on purpose: it is missing data, not evidence of stability.

		prev = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]PrefixChange, 0, len(prefixDriftKinds)*2)
	for _, kind := range prefixDriftKinds {
		v := acc[kind]
		for i, changed := range []bool{false, true} {
			out = append(out, PrefixChange{
				Kind:                     kind,
				AccountChanged:           changed,
				Pairs:                    v[i].pairs,
				CacheCreationInputTokens: v[i].create,
				CacheReadInputTokens:     v[i].read,
				PairsMissingUsage:        v[i].missing,
			})
		}
	}
	return out, nil
}

// rowUsage is the four #110 counters for one request.
type rowUsage struct{ create, read, input, output int64 }

// usageMissing reports that nothing at all was decoded from the response.
// A served /v1/messages response always reports a non-zero input_tokens, so
// all four at zero means the sniffer could not read the body — today, most
// often a Brotli-encoded response it does not decode (issue #126).
func (u rowUsage) usageMissing() bool {
	return u.create == 0 && u.read == 0 && u.input == 0 && u.output == 0
}

// compare reports whether two recorded fingerprints can be compared at all
// (both present) and, if so, whether they differ. An absent hash on either
// side is missing data — never a change.
func compare(before, after string) (ok, changed bool) {
	if before == "" || after == "" {
		return false, false
	}
	return true, before != after
}
