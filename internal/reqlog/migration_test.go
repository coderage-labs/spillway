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
		"cache_creation_input_tokens", "cache_read_input_tokens"} {
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
