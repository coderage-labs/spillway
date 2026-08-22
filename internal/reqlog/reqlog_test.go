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
		{Account: "kimi", Path: "/v1/messages", Status: 200, DurationMs: 800, Bytes: 1024, Event: EventRotatedQuota},
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
	// names are not credentials. Anything new must be justified the same way,
	// which is the point of asserting the exact set.
	want := map[string]bool{
		"ts": true, "account": true, "path": true, "status": true,
		"duration_ms": true, "bytes": true, "event": true,
		"model_asked": true, "model_served": true,
	}
	if len(cols) != len(want) {
		t.Errorf("columns = %v", cols)
	}
	for _, c := range cols {
		if !want[c] {
			t.Errorf("unexpected column %q — redaction surface widened", c)
		}
		for _, banned := range []string{"header", "body", "token", "authorization", "api_key", "apikey"} {
			if strings.Contains(c, banned) {
				t.Errorf("column %q smells like credential storage", c)
			}
		}
	}
}
