package reqlog

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/testmode"
)

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	for i, e := range []Entry{
		{Account: "work", Path: "/v1/messages", Status: 200, DurationMs: 1200, Bytes: 4096, Event: EventServed},
		{Account: "kimi", Path: "/v1/messages", Status: 200, DurationMs: 800, Bytes: 1024, Event: EventRotatedQuota,
			UserAgent: "python-requests/2.32.0", SessionHash: "deadbeef",
			InputTokens: 120, OutputTokens: 340, CacheCreationInputTokens: 4165, CacheReadInputTokens: 1816},
	} {
		if err := l.Record(e); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}

	got, err := l.Recent(10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	// Newest first.
	if got[0].Account != "kimi" || got[0].Event != EventRotatedQuota || got[0].Status != 200 || got[0].Bytes != 1024 {
		t.Errorf("entry[0] = %+v", got[0])
	}
	// UserAgent (issue #64): a request log that can't tell a CLI request
	// apart from anything else calling through the pool is a request log
	// that can't show the quota-consumption consequence of that issue.
	if got[0].UserAgent != "python-requests/2.32.0" {
		t.Errorf("UserAgent = %q, want round-tripped value", got[0].UserAgent)
	}
	// Issue #110: the four usage counters and the session hash round-trip
	// exactly like every other column here.
	if got[0].SessionHash != "deadbeef" {
		t.Errorf("SessionHash = %q, want round-tripped value", got[0].SessionHash)
	}
	if got[0].InputTokens != 120 || got[0].OutputTokens != 340 ||
		got[0].CacheCreationInputTokens != 4165 || got[0].CacheReadInputTokens != 1816 {
		t.Errorf("usage counters = %+v, want round-tripped values", got[0])
	}
	if got[0].Ts.IsZero() {
		t.Error("ts not populated")
	}
	if time.Since(got[0].Ts) > time.Minute {
		t.Errorf("ts = %v, not recent", got[0].Ts)
	}

	testmode.AssertPrivateFile(t, path)
}

// The schema is the redaction (§5): there are no header/body columns to
// leak into, and nothing resembling a token may appear in stored rows.
func TestRedactionBySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	rows, err := l.db.Query(`SELECT name FROM pragma_table_info('requests')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	// Every column here is a deliberate choice. model_asked/model_served were
	// added for doc §6.18 (the served model is invisible otherwise); model
	// names are not credentials. user_agent was added for issue #64 (telling
	// a non-CLI caller apart after the fact); a User-Agent string is
	// identifying metadata, same tier as a model name, never a credential.
	//
	// session_hash and the four *_tokens columns were added for issue #110:
	// the four are read from the response's `usage` block, the one deliberate
	// exception to "never bodies" (see the package doc), and are token
	// COUNTS — integers describing volume, not the credential tokens the
	// banned-substring check below exists to catch. session_hash is an
	// fnv32a hash of proxy.sessionKey's value, hashed again before it
	// reaches this package, so nothing identifying (a raw client IP, in the
	// no-user-id fallback case) is ever stored — see RotationCost for why
	// it exists.
	//
	// Anything new must be justified the same way, which is the point of
	// asserting the exact set.
	// The eight prefix columns were added for issue #111 phase 1. They are
	// derived from the REQUEST body, which makes them the second deliberate
	// widening after #110's response counters, and they were allowed on the
	// same terms: each is a truncated SHA-256 of a region or an integer
	// counting things in it, and none of them can carry content. There is
	// no column here that holds prompt text, a tool description, a tool
	// input or an attachment path, because there is no column here that
	// holds a value the request could have chosen the bytes of. The
	// assertion below that every hash column reads back as hex is what
	// stops that being merely an intention.
	want := map[string]bool{
		"ts": true, "account": true, "path": true, "status": true,
		"duration_ms": true, "bytes": true, "event": true,
		"model_asked": true, "model_served": true, "user_agent": true,
		"session_hash": true, "input_tokens": true, "output_tokens": true,
		"cache_creation_input_tokens": true, "cache_read_input_tokens": true,
		"tool_count": true, "tools_order_hash": true, "tools_sorted_hash": true,
		"tools_raw_hash": true, "system_hash": true, "first_msg_shape_hash": true,
		"first_msg_blocks": true, "prefix_bytes": true,
	}
	if len(cols) != len(want) {
		t.Errorf("columns = %v", cols)
	}
	// usageCountColumns are the only columns allowed to contain "token": a
	// token COUNT, never a credential. Enumerated explicitly (not "ends
	// with _tokens") so a new column still has to earn its way onto this
	// list rather than merely matching a pattern.
	usageCountColumns := map[string]bool{
		"input_tokens": true, "output_tokens": true,
		"cache_creation_input_tokens": true, "cache_read_input_tokens": true,
	}
	for _, c := range cols {
		if !want[c] {
			t.Errorf("unexpected column %q — redaction surface widened", c)
		}
		for _, banned := range []string{"header", "body", "authorization", "api_key", "apikey"} {
			if strings.Contains(c, banned) {
				t.Errorf("column %q smells like credential storage", c)
			}
		}
		if strings.Contains(c, "token") && !usageCountColumns[c] {
			t.Errorf("column %q smells like credential storage", c)
		}
	}
}
