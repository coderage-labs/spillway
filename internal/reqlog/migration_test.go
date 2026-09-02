package reqlog

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// Issue #110 added five columns (session_hash and four usage counters) to
// `requests`. Two things must hold: reopening an already-migrated database
// is a no-op, and a database from before this change upgrades cleanly
// without losing its existing rows.

func TestUsageColumnsMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")

	l1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening re-runs the ALTER TABLE loop against a database that
	// already has every column it adds.
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("reopening an already-migrated database must not fail: %v", err)
	}
	defer l2.Close()

	cols := columnNames(t, path)
	seen := map[string]int{}
	for _, c := range cols {
		seen[c]++
	}
	for _, want := range []string{"session_hash", "input_tokens", "output_tokens",
		"cache_creation_input_tokens", "cache_read_input_tokens",
		// Issue #111 phase 1.
		"tool_count", "tools_order_hash", "tools_sorted_hash", "tools_raw_hash",
		"system_hash", "first_msg_shape_hash", "first_msg_blocks", "prefix_bytes"} {
		if seen[want] != 1 {
			t.Errorf("column %q appears %d times after two opens, want 1", want, seen[want])
		}
	}
}

// TestExistingDatabaseWithoutUsageColumnsUpgradesCleanly builds the table
// exactly as it looked before issue #110 (hand-rolled, mirroring the
// pre-#110 CREATE TABLE plus the three older ALTER-added columns), seeds a
// row the way a real deployment would have, then opens it through the
// current Log.Open. The old row must still read back, and the new columns
// must default to zero/empty rather than the open failing.
func TestExistingDatabaseWithoutUsageColumnsUpgradesCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE requests (
		ts INTEGER NOT NULL,
		account TEXT NOT NULL,
		path TEXT NOT NULL,
		status INTEGER NOT NULL,
		duration_ms INTEGER NOT NULL,
		bytes INTEGER NOT NULL,
		event TEXT NOT NULL,
		model_asked TEXT NOT NULL DEFAULT '',
		model_served TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO requests
		(ts, account, path, status, duration_ms, bytes, event, model_asked, model_served, user_agent)
		VALUES (1000, 'old-acct', '/v1/messages', 200, 500, 2048, 'served', 'sonnet', 'sonnet', 'claude-cli/1.0')`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-#110 database must upgrade cleanly, got: %v", err)
	}
	defer l.Close()

	entries, err := l.Recent(10)
	if err != nil {
		t.Fatalf("Recent after upgrade: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want the pre-existing row still present", len(entries))
	}
	e := entries[0]
	if e.Account != "old-acct" || e.Bytes != 2048 {
		t.Errorf("pre-existing row corrupted by migration: %+v", e)
	}
	if e.SessionHash != "" {
		t.Errorf("SessionHash = %q, want empty default for a pre-#110 row", e.SessionHash)
	}
	if e.InputTokens != 0 || e.OutputTokens != 0 ||
		e.CacheCreationInputTokens != 0 || e.CacheReadInputTokens != 0 {
		t.Errorf("usage counters = %+v, want all zero for a pre-#110 row", e)
	}

	// And the log must still be writable with the new fields going forward.
	if err := l.Record(Entry{Account: "old-acct", Path: "/v1/messages", Status: 200,
		Event: EventServed, CacheReadInputTokens: 500}); err != nil {
		t.Fatalf("Record after upgrade: %v", err)
	}
}

func columnNames(t *testing.T, path string) []string {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rows, err := raw.Query(`SELECT name FROM pragma_table_info('requests')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// TestExistingDatabaseWithoutPrefixColumnsUpgradesCleanly is the #111 mirror
// of the test above: a database as it looked after #110 but before the
// prefix columns existed — which is what every installation running the
// current release actually has on disk — must upgrade in place, keep its
// rows, and default the new columns rather than failing to open.
func TestExistingDatabaseWithoutPrefixColumnsUpgradesCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "post110.db")

	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE requests (
		ts INTEGER NOT NULL,
		account TEXT NOT NULL,
		path TEXT NOT NULL,
		status INTEGER NOT NULL,
		duration_ms INTEGER NOT NULL,
		bytes INTEGER NOT NULL,
		event TEXT NOT NULL,
		model_asked TEXT NOT NULL DEFAULT '',
		model_served TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT '',
		session_hash TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read_input_tokens INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO requests
		(ts, account, path, status, duration_ms, bytes, event, model_asked, model_served, user_agent,
		 session_hash, input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens)
		VALUES (1000, 'work', '/v1/messages', 200, 500, 2048, 'served', 'sonnet', 'sonnet',
		        'claude-cli/2.1.0', 'abcd', 120, 340, 4165, 1816)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-#111 database must upgrade cleanly, got: %v", err)
	}
	defer l.Close()

	entries, err := l.Recent(10)
	if err != nil {
		t.Fatalf("Recent after upgrade: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want the pre-existing row still present", len(entries))
	}
	e := entries[0]
	if e.Account != "work" || e.CacheCreationInputTokens != 4165 {
		t.Errorf("pre-existing row corrupted by migration: %+v", e)
	}
	if e.ToolCount != 0 || e.PrefixBytes != 0 || e.FirstMsgBlocks != 0 {
		t.Errorf("prefix counts = %+v, want zero defaults for a pre-#111 row", e)
	}
	if e.ToolsOrderHash != "" || e.ToolsSortedHash != "" || e.ToolsRawHash != "" ||
		e.SystemHash != "" || e.FirstMsgShapeHash != "" {
		t.Errorf("prefix hashes = %+v, want empty defaults for a pre-#111 row", e)
	}

	// An empty fingerprint on an old row must not be readable as a change:
	// PrefixDrift skips rows with no fingerprint at all.
	report, err := l.PrefixDrift()
	if err != nil {
		t.Fatalf("PrefixDrift over an upgraded database: %v", err)
	}
	for _, c := range report {
		if c.Pairs != 0 {
			t.Errorf("a pre-#111 row was counted as a pair: %+v", c)
		}
	}

	// And the log must still be writable with the new fields going forward.
	if err := l.Record(Entry{Account: "work", Path: "/v1/messages", Status: 200,
		Event: EventServed, ToolCount: 30, ToolsOrderHash: "0123456789abcdef"}); err != nil {
		t.Fatalf("Record after upgrade: %v", err)
	}
}
