package reqlog

// Tests for issue #111 phase 1's prefix columns and the report over them.
// Everything here goes through a real on-disk SQLite store — the point of
// the exercise is that a row written by the proxy can be read back and
// counted, and a mock proves nothing about that.

import (
	"database/sql"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func openTestLog(t *testing.T) (*Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "requests.db")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

// The columns must survive a real write and a real read. #110 shipped a
// measurement that recorded zeros in production; a round trip through the
// actual store is the cheapest thing that would have caught it.
func TestPrefixFingerprintColumnsRoundTrip(t *testing.T) {
	l, _ := openTestLog(t)

	want := Entry{
		Account: "work", Path: "/v1/messages", Status: 200, Event: EventServed,
		SessionHash:              "sess1",
		CacheCreationInputTokens: 4165,
		ToolCount:                31,
		ToolsOrderHash:           "0123456789abcdef",
		ToolsSortedHash:          "fedcba9876543210",
		ToolsRawHash:             "aaaabbbbccccdddd",
		SystemHash:               "1111222233334444",
		FirstMsgShapeHash:        "5555666677778888",
		FirstMsgBlocks:           3,
		PrefixBytes:              474373,
	}
	if err := l.Record(want); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := l.Recent(10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	g := got[0]
	if g.ToolCount != want.ToolCount || g.FirstMsgBlocks != want.FirstMsgBlocks || g.PrefixBytes != want.PrefixBytes {
		t.Errorf("counts did not round-trip: %+v", g)
	}
	if g.ToolsOrderHash != want.ToolsOrderHash || g.ToolsSortedHash != want.ToolsSortedHash ||
		g.ToolsRawHash != want.ToolsRawHash || g.SystemHash != want.SystemHash ||
		g.FirstMsgShapeHash != want.FirstMsgShapeHash {
		t.Errorf("hashes did not round-trip: %+v", g)
	}
	// The ordered/sorted pair must stay distinguishable through the store:
	// if the two columns were ever wired to the same value, every ordering
	// question this whole change exists to answer would come back "no".
	if g.ToolsOrderHash == g.ToolsSortedHash {
		t.Error("ordered and sorted hashes read back identical from distinct written values — the columns are crossed")
	}
}

var hashCol = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Redaction, checked against stored VALUES rather than only column names:
// every hash column holds hex or nothing. A row that stored a tool name or
// a snippet of a system prompt would fail here even if the column set test
// still passed.
func TestPrefixColumnsStoreOnlyHashes(t *testing.T) {
	l, path := openTestLog(t)
	if err := l.Record(Entry{
		Account: "a", Path: "/v1/messages", Status: 200, Event: EventServed,
		ToolsOrderHash: "0123456789abcdef", ToolsSortedHash: "fedcba9876543210",
		ToolsRawHash: "aaaabbbbccccdddd", SystemHash: "1111222233334444",
		FirstMsgShapeHash: "5555666677778888",
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(Entry{Account: "a", Path: "/v1/messages", Status: 200, Event: EventServed}); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rows, err := raw.Query(`SELECT tools_order_hash, tools_sorted_hash, tools_raw_hash,
		system_hash, first_msg_shape_hash FROM requests`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var vals [5]string
		if err := rows.Scan(&vals[0], &vals[1], &vals[2], &vals[3], &vals[4]); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			if v != "" && !hashCol.MatchString(v) {
				t.Errorf("stored fingerprint %q is not a hash — content must never reach these columns", v)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// driftRow is one seeded request, spelled out so the intent of each case is
// visible at the call site rather than buried in field order.
type driftRow struct {
	session, account                     string
	order, sorted, raw, system, msgShape string
	create, read, input                  int64
}

func seedDrift(t *testing.T, l *Log, rows []driftRow) {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	for i, r := range rows {
		e := Entry{
			Ts: base.Add(time.Duration(i) * time.Second), Account: r.account,
			Path: "/v1/messages", Status: 200, Event: EventServed,
			SessionHash:              r.session,
			InputTokens:              r.input,
			CacheCreationInputTokens: r.create,
			CacheReadInputTokens:     r.read,
			ToolsOrderHash:           r.order,
			ToolsSortedHash:          r.sorted,
			ToolsRawHash:             r.raw,
			SystemHash:               r.system,
			FirstMsgShapeHash:        r.msgShape,
		}
		if err := l.Record(e); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
}

func cell(t *testing.T, report []PrefixChange, kind string, accountChanged bool) PrefixChange {
	t.Helper()
	for _, c := range report {
		if c.Kind == kind && c.AccountChanged == accountChanged {
			return c
		}
	}
	t.Fatalf("report has no cell for kind %q accountChanged=%v", kind, accountChanged)
	return PrefixChange{}
}

// The measurement #111 turns on: ordering jitter must be counted separately
// from a genuinely changed tool set, and its cache-creation cost attributed
// to it. If these two collapsed into one bucket, the data could not tell
// anyone whether sorting tools would help.
func TestPrefixDriftSeparatesOrderingJitterFromSetChanges(t *testing.T) {
	l, _ := openTestLog(t)
	seedDrift(t, l, []driftRow{
		// Baseline.
		{session: "s1", account: "work", order: "AAAA", sorted: "SORT", raw: "R1", system: "SYS", msgShape: "SHP", input: 10},
		// Pure ordering jitter: ordered moved, sorted held. 900 tokens.
		{session: "s1", account: "work", order: "BBBB", sorted: "SORT", raw: "R2", system: "SYS", msgShape: "SHP", create: 900, input: 10},
		// Genuinely different tool set: both moved. 700 tokens.
		{session: "s1", account: "work", order: "CCCC", sorted: "SORT2", raw: "R3", system: "SYS", msgShape: "SHP", create: 700, input: 10},
		// Nothing changed at all. 50 tokens — the stable baseline cost.
		{session: "s1", account: "work", order: "CCCC", sorted: "SORT2", raw: "R3", system: "SYS", msgShape: "SHP", create: 50, read: 9000, input: 10},
	})

	report, err := l.PrefixDrift()
	if err != nil {
		t.Fatalf("PrefixDrift: %v", err)
	}

	if c := cell(t, report, KindToolsOrderOnly, false); c.Pairs != 1 || c.CacheCreationInputTokens != 900 {
		t.Errorf("tools-order-only = %+v, want 1 pair / 900 cache-creation tokens", c)
	}
	if c := cell(t, report, KindToolsSet, false); c.Pairs != 1 || c.CacheCreationInputTokens != 700 {
		t.Errorf("tools-set = %+v, want 1 pair / 700 tokens", c)
	}
	// The set change must NOT also be counted as ordering jitter: sorting
	// tools would not have prevented it, and counting it there would
	// overstate the case for building the transform.
	if c := cell(t, report, KindToolsOrderOnly, false); c.Pairs != 1 {
		t.Errorf("a genuine tool-set change leaked into the ordering-jitter bucket: %+v", c)
	}
	if c := cell(t, report, KindStable, false); c.Pairs != 1 || c.CacheCreationInputTokens != 50 || c.CacheReadInputTokens != 9000 {
		t.Errorf("none = %+v, want 1 pair / 50 create / 9000 read — the control group", c)
	}
	if c := cell(t, report, KindAny, false); c.Pairs != 2 || c.CacheCreationInputTokens != 1600 {
		t.Errorf("any-change = %+v, want 2 pairs / 1600 tokens", c)
	}
	// A raw-bytes change that rode along with a name change is not a
	// "tools-raw" finding: that bucket is for churn the name hashes cannot
	// see, e.g. an edited description.
	if c := cell(t, report, KindToolsRaw, false); c.Pairs != 0 {
		t.Errorf("tools-raw = %+v, want 0 — every raw change here came with a name change", c)
	}
}

func TestPrefixDriftCountsEachRegionSeparately(t *testing.T) {
	l, _ := openTestLog(t)
	seedDrift(t, l, []driftRow{
		{session: "s1", account: "a", order: "O", sorted: "S", raw: "R", system: "SYS1", msgShape: "SHP1", input: 1},
		// System only.
		{session: "s1", account: "a", order: "O", sorted: "S", raw: "R", system: "SYS2", msgShape: "SHP1", create: 100, input: 1},
		// First-message shape only.
		{session: "s1", account: "a", order: "O", sorted: "S", raw: "R", system: "SYS2", msgShape: "SHP2", create: 200, input: 1},
		// Tool bytes only — same names, same order, different bytes.
		{session: "s1", account: "a", order: "O", sorted: "S", raw: "R2", system: "SYS2", msgShape: "SHP2", create: 300, input: 1},
	})
	report, err := l.PrefixDrift()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind   string
		pairs  int64
		create int64
	}{
		{KindSystem, 1, 100},
		{KindFirstMsgShape, 1, 200},
		{KindToolsRaw, 1, 300},
		{KindToolsOrderOnly, 0, 0},
		{KindToolsSet, 0, 0},
		{KindStable, 0, 0},
	} {
		c := cell(t, report, tc.kind, false)
		if c.Pairs != tc.pairs || c.CacheCreationInputTokens != tc.create {
			t.Errorf("%s = %d pairs / %d tokens, want %d / %d", tc.kind, c.Pairs, c.CacheCreationInputTokens, tc.pairs, tc.create)
		}
	}
}

// Rotation cost has to be separable from in-session prefix churn, or the
// report cannot answer #111's own point 5: if the cache is per account, a
// rotation pays cache-creation no matter how stable the prefix was.
func TestPrefixDriftSplitsByAccountChange(t *testing.T) {
	l, _ := openTestLog(t)
	seedDrift(t, l, []driftRow{
		{session: "s1", account: "work", order: "O", sorted: "S", raw: "R", system: "SYS", msgShape: "SHP", input: 1},
		// Identical prefix, different account: pure rotation cost.
		{session: "s1", account: "side", order: "O", sorted: "S", raw: "R", system: "SYS", msgShape: "SHP", create: 5000, input: 1},
		// Ordering jitter, still on the new account.
		{session: "s1", account: "side", order: "O2", sorted: "S", raw: "R2", system: "SYS", msgShape: "SHP", create: 400, input: 1},
	})
	report, err := l.PrefixDrift()
	if err != nil {
		t.Fatal(err)
	}
	if c := cell(t, report, KindStable, true); c.Pairs != 1 || c.CacheCreationInputTokens != 5000 {
		t.Errorf("none/account-changed = %+v, want 1 pair / 5000 tokens — rotation cost on an unchanged prefix", c)
	}
	if c := cell(t, report, KindStable, false); c.Pairs != 0 {
		t.Errorf("none/same-account = %+v, want 0 — the unchanged pair crossed a rotation", c)
	}
	if c := cell(t, report, KindToolsOrderOnly, false); c.Pairs != 1 || c.CacheCreationInputTokens != 400 {
		t.Errorf("tools-order-only/same-account = %+v, want 1 pair / 400 tokens", c)
	}
	if c := cell(t, report, KindToolsOrderOnly, true); c.Pairs != 0 {
		t.Errorf("tools-order-only/account-changed = %+v, want 0", c)
	}
}

// Sessions must not bleed into each other: the first request of a session
// has no predecessor, and pairing it with the last request of an unrelated
// session would invent changes wholesale.
func TestPrefixDriftDoesNotPairAcrossSessions(t *testing.T) {
	l, _ := openTestLog(t)
	seedDrift(t, l, []driftRow{
		{session: "s1", account: "a", order: "O1", sorted: "S1", raw: "R1", system: "SYS1", msgShape: "M1", input: 1},
		{session: "s2", account: "a", order: "O2", sorted: "S2", raw: "R2", system: "SYS2", msgShape: "M2", create: 8000, input: 1},
	})
	report, err := l.PrefixDrift()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report {
		if c.Pairs != 0 || c.CacheCreationInputTokens != 0 {
			t.Errorf("two one-request sessions produced %+v — there is no pair to compare", c)
		}
	}
}

// A fingerprint absent on one side of a pair is missing data, not a change.
// Manufacturing changes out of unrecorded values is precisely how a
// measurement comes to confirm whatever it was built to look for.
func TestPrefixDriftTreatsAbsentFingerprintsAsIncomparable(t *testing.T) {
	l, _ := openTestLog(t)
	seedDrift(t, l, []driftRow{
		{session: "s1", account: "a", order: "O1", sorted: "S1", raw: "R1", system: "SYS", msgShape: "M", input: 1},
		// Tools were not measurable this time (over the buffering cap, say)
		// but the system block was, and it did not change.
		{session: "s1", account: "a", system: "SYS", msgShape: "M", create: 120, input: 1},
	})
	report, err := l.PrefixDrift()
	if err != nil {
		t.Fatal(err)
	}
	if c := cell(t, report, KindToolsOrderOnly, false); c.Pairs != 0 {
		t.Errorf("an absent tools hash was counted as ordering jitter: %+v", c)
	}
	if c := cell(t, report, KindToolsSet, false); c.Pairs != 0 {
		t.Errorf("an absent tools hash was counted as a set change: %+v", c)
	}
	if c := cell(t, report, KindStable, false); c.Pairs != 1 {
		t.Errorf("none = %+v, want 1 — the regions that WERE comparable did not change", c)
	}
}

// Issue #126: a Brotli response records no usage at all. The fingerprints
// come from the request and are recorded regardless, so the pair must still
// be classified — but its missing token totals must be visible as a
// coverage hole rather than averaged in as a zero.
func TestPrefixDriftReportsMissingUsageSeparately(t *testing.T) {
	l, _ := openTestLog(t)
	seedDrift(t, l, []driftRow{
		{session: "s1", account: "a", order: "O1", sorted: "S", raw: "R1", system: "SYS", msgShape: "M", input: 10},
		// Ordering jitter, usage decoded.
		{session: "s1", account: "a", order: "O2", sorted: "S", raw: "R2", system: "SYS", msgShape: "M", create: 900, input: 10},
		// Ordering jitter, response was Brotli: every counter zero.
		{session: "s1", account: "a", order: "O3", sorted: "S", raw: "R3", system: "SYS", msgShape: "M"},
	})
	report, err := l.PrefixDrift()
	if err != nil {
		t.Fatal(err)
	}
	c := cell(t, report, KindToolsOrderOnly, false)
	if c.Pairs != 2 {
		t.Errorf("pairs = %d, want 2 — a response the sniffer could not read must still be classified", c.Pairs)
	}
	if c.PairsMissingUsage != 1 {
		t.Errorf("PairsMissingUsage = %d, want 1 — the coverage hole must be visible, not averaged over", c.PairsMissingUsage)
	}
	if c.CacheCreationInputTokens != 900 {
		t.Errorf("cache-creation = %d, want 900", c.CacheCreationInputTokens)
	}
}

// The report's shape must be stable and deterministic: every kind, both
// account-changed values, same order every call, regardless of what was or
// was not observed. A caller rendering it should never have to range a map.
func TestPrefixDriftShapeIsStable(t *testing.T) {
	l, _ := openTestLog(t)
	first, err := l.PrefixDrift()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(prefixDriftKinds)*2 {
		t.Fatalf("report has %d cells, want %d", len(first), len(prefixDriftKinds)*2)
	}
	seedDrift(t, l, []driftRow{
		{session: "s1", account: "a", order: "O1", sorted: "S", raw: "R", system: "SYS", msgShape: "M", input: 1},
		{session: "s1", account: "a", order: "O2", sorted: "S", raw: "R2", system: "SYS", msgShape: "M", create: 5, input: 1},
	})
	for i := 0; i < 20; i++ {
		got, err := l.PrefixDrift()
		if err != nil {
			t.Fatal(err)
		}
		for j := range got {
			if got[j].Kind != first[j].Kind || got[j].AccountChanged != first[j].AccountChanged {
				t.Fatalf("cell %d moved between calls: %v then %v", j, first[j], got[j])
			}
		}
	}
}
