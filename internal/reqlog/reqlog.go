// Package reqlog is the SQLite request log (design doc §5, §7): one row per
// request with metadata only — NEVER auth headers, bodies, or any prompt or
// completion content.
//
// Issue #110 widened that redaction deliberately, once, to four integers:
// input_tokens, output_tokens, cache_creation_input_tokens and
// cache_read_input_tokens, read from the response's `usage` block. Nothing
// else in that body is ever unmarshaled, stored or logged — no content
// block, no tool input, no stop reason, no text of any kind. See
// internal/proxy/usage.go for the parser, which decodes only those four
// fields by construction (its target struct has no other fields to decode
// into), and TestRedactionBySchema below, which pins the exact column set
// so a future widening has to be just as deliberate as this one was.
//
// Issue #111 phase 1 added eight more columns, and they are not an
// exception to the rule above — they are hashes and counts describing the
// SHAPE of the request prefix (how many tools, in what order, how big),
// derived from the request body but containing none of it. Truncated
// SHA-256 of a region, or an integer counting things in it. No prompt text,
// no tool description, no tool input, no attachment path, nothing
// reconstructable. See internal/proxy/prefixfp.go for the computation and
// prefixdrift.go here for the query they exist to make possible.
package reqlog

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Event kinds recorded per request.
const (
	EventServed       = "served"
	EventRotatedQuota = "rotated-quota"
	// EventRotatedConn: pre-first-byte rotation away from an account that
	// wasn't a quota rejection — a dead upstream connection, or an upstream
	// 5xx (issue #26). Neither is the account's fault, so it isn't marked
	// exhausted; it's simply excluded from this request's retries.
	EventRotatedConn = "rotated-conn"
	EventRateRetry   = "rate-retry"
	EventHeld        = "held"
	EventExhausted   = "exhausted"
	EventPassthrough = "passthrough"
	// EventOverage: served past the subscription quota, at cost. Recorded
	// separately from EventServed because it is the one outcome the user
	// pays for, and a bill is not something to find out about from a bill.
	EventOverage = "overage"
)

// Entry is one logged request.
type Entry struct {
	Ts         time.Time `json:"ts"`
	Account    string    `json:"account"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	DurationMs int64     `json:"duration_ms"`
	Bytes      int64     `json:"bytes"`
	Event      string    `json:"event"`
	// ModelAsked is what the client sent; ModelServed is what actually went
	// upstream after any modelMap rewrite. They differ on a cross-provider
	// account, and that difference is invisible everywhere else (doc §6.18).
	ModelAsked  string `json:"model_asked,omitempty"`
	ModelServed string `json:"model_served,omitempty"`
	// UserAgent is the client's own User-Agent header, verbatim (issue #64).
	// Once a combined CA bundle (internal/mitm.EnsureCABundle) lets a
	// non-Node subprocess's TLS verification succeed against a MITM'd host,
	// its requests get pooled and billed exactly like the CLI's own — a
	// behaviour change worth being able to see after the fact. The CLI's own
	// UA is distinctive ("claude-cli/x.y.z (external, cli)"); most other
	// HTTP clients (python-requests, urllib, curl) are not, so this is only
	// ever a hint, never something to gate or route on.
	UserAgent string `json:"user_agent,omitempty"`
	// SessionHash groups requests from the same Claude Code SESSION
	// (proxy.sessionKeys' conversation key, issue #141), WITHOUT storing
	// the raw value that feeds it — that is a client-side session id, or a
	// client IP in the no-metadata fallback. It is an fnv32a hash of that
	// value, taken before it ever reaches this package, kept only so a
	// query can tell "this request's account differs from the previous
	// request in the same session" (issue #110's rotation-cost question)
	// apart from "a fresh session picked a fresh account", which looks
	// identical without it. See RotationCost.
	//
	// It is NOT the key the pool routed by: routing stays per client blob
	// on purpose (conversation.go). And a session is not a conversation —
	// subagents share their parent's id, so consumers that pair
	// consecutive rows within a session are approximating, and say so.
	SessionHash string `json:"session_hash,omitempty"`
	// InputTokens, OutputTokens, CacheCreationInputTokens and
	// CacheReadInputTokens are the four counters from the response's
	// `usage` block (issue #110) — cache-create vs cache-read is what
	// answers "what does a rotation cost", the question spillway had never
	// measured. Parsed by internal/proxy without buffering the response;
	// zero on any parse failure, truncation, or unsupported encoding —
	// telemetry must never affect the response the client receives.
	InputTokens              int64 `json:"input_tokens,omitempty"`
	OutputTokens             int64 `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`

	// Prefix fingerprints (issue #111, phase 1). Structural hashes and
	// counts describing the REQUEST prefix — the region Anthropic's prompt
	// cache keys on byte-exactly — so a query can ask which part of the
	// prefix had changed when CacheCreationInputTokens spiked. Computed by
	// internal/proxy.fingerprintPrefix from the body the proxy has already
	// buffered for failover; empty/zero whenever the body was over the
	// buffering cap, absent, or not parseable.
	//
	// These are hashes and counts ONLY. No prompt text, no tool
	// description, no tool input, no attachment path — see the redaction
	// note in this package's doc comment and TestRedactionBySchema.
	//
	// ToolsOrderHash vs ToolsSortedHash is the pair that matters: ordered
	// changed while sorted held still means the tool SET was identical and
	// only its ORDER jittered, which is the single instability #111's
	// proposed tool sorting would actually fix. Nothing recorded before
	// this could tell that apart from the tool set genuinely changing.
	//
	// The account this request was routed to — the other half of #111's
	// "is the cache per account" question — needs no new column: it is the
	// existing Account field above, which PrefixDrift joins against.
	ToolCount         int    `json:"tool_count,omitempty"`
	ToolsOrderHash    string `json:"tools_order_hash,omitempty"`
	ToolsSortedHash   string `json:"tools_sorted_hash,omitempty"`
	ToolsRawHash      string `json:"tools_raw_hash,omitempty"`
	SystemHash        string `json:"system_hash,omitempty"`
	FirstMsgShapeHash string `json:"first_msg_shape_hash,omitempty"`
	FirstMsgBlocks    int    `json:"first_msg_blocks,omitempty"`
	// PrefixBytes is the combined byte length of the system block, the
	// tools array and messages[0] — a size for the region the hashes above
	// cover, so a change can be read against how much prefix there was to
	// re-write.
	PrefixBytes int64 `json:"prefix_bytes,omitempty"`
}

// Log is an open request-log database.
type Log struct {
	db *sql.DB
}

// migrationColumn is one column added to `requests` after the first
// release. Type is spelled out per-column (rather than assuming TEXT, as
// the original single-type loop did) because issue #110 added integer
// counters alongside the existing text ones.
type migrationColumn struct{ name, sqlType string }

// requestColumns lists every ALTER-added column, in the order Record and
// Recent read and write them. Adding a column here is a redaction-surface
// change — see TestRedactionBySchema, which must be updated in the same
// change for the same reason.
var requestColumns = []migrationColumn{
	{"model_asked", "TEXT NOT NULL DEFAULT ''"},
	{"model_served", "TEXT NOT NULL DEFAULT ''"},
	{"user_agent", "TEXT NOT NULL DEFAULT ''"},
	{"session_hash", "TEXT NOT NULL DEFAULT ''"},
	{"input_tokens", "INTEGER NOT NULL DEFAULT 0"},
	{"output_tokens", "INTEGER NOT NULL DEFAULT 0"},
	{"cache_creation_input_tokens", "INTEGER NOT NULL DEFAULT 0"},
	{"cache_read_input_tokens", "INTEGER NOT NULL DEFAULT 0"},
	// Issue #111 phase 1: structural fingerprints of the request prefix.
	// Hashes and counts only — see Entry's doc for what each one is and
	// TestRedactionBySchema for why each was allowed.
	{"tool_count", "INTEGER NOT NULL DEFAULT 0"},
	{"tools_order_hash", "TEXT NOT NULL DEFAULT ''"},
	{"tools_sorted_hash", "TEXT NOT NULL DEFAULT ''"},
	{"tools_raw_hash", "TEXT NOT NULL DEFAULT ''"},
	{"system_hash", "TEXT NOT NULL DEFAULT ''"},
	{"first_msg_shape_hash", "TEXT NOT NULL DEFAULT ''"},
	{"first_msg_blocks", "INTEGER NOT NULL DEFAULT 0"},
	{"prefix_bytes", "INTEGER NOT NULL DEFAULT 0"},
}

// Open opens (creating if needed) the log at path with 0600 perms and WAL
// journaling.
func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open request log: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS requests (
		ts INTEGER NOT NULL,
		account TEXT NOT NULL,
		path TEXT NOT NULL,
		status INTEGER NOT NULL,
		duration_ms INTEGER NOT NULL,
		bytes INTEGER NOT NULL,
		event TEXT NOT NULL
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create request log schema: %w", err)
	}
	// Added after the first release; existing databases need the columns.
	// Run unconditionally (a brand-new database gets them this way too,
	// same as the original three) and tolerate "duplicate column" so
	// re-opening an already-migrated database is a no-op, not an error.
	for _, col := range requestColumns {
		if _, err := db.Exec(`ALTER TABLE requests ADD COLUMN ` + col.name + ` ` + col.sqlType); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("add %s column: %w", col.name, err)
		}
	}
	if err := (&Log{db: db}).initQuota(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create quota schema: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return &Log{db: db}, nil
}

// Close closes the database.
func (l *Log) Close() error { return l.db.Close() }

// Record writes one request row. Inputs are metadata only — the schema has
// no header/body columns by design (§5 redaction), and the four usage
// counters are the only body-derived values it accepts at all (§110).
func (l *Log) Record(e Entry) error {
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}
	_, err := l.db.Exec(`INSERT INTO requests
		(ts, account, path, status, duration_ms, bytes, event, model_asked, model_served, user_agent,
		 session_hash, input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
		 tool_count, tools_order_hash, tools_sorted_hash, tools_raw_hash, system_hash,
		 first_msg_shape_hash, first_msg_blocks, prefix_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Ts.UnixMilli(), e.Account, e.Path, e.Status, e.DurationMs, e.Bytes, e.Event,
		e.ModelAsked, e.ModelServed, e.UserAgent,
		e.SessionHash, e.InputTokens, e.OutputTokens, e.CacheCreationInputTokens, e.CacheReadInputTokens,
		e.ToolCount, e.ToolsOrderHash, e.ToolsSortedHash, e.ToolsRawHash, e.SystemHash,
		e.FirstMsgShapeHash, e.FirstMsgBlocks, e.PrefixBytes)
	return err
}

// Recent returns the newest limit entries, newest first.
func (l *Log) Recent(limit int) ([]Entry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := l.db.Query(`SELECT ts, account, path, status, duration_ms, bytes, event,
		model_asked, model_served, user_agent,
		session_hash, input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
		tool_count, tools_order_hash, tools_sorted_hash, tools_raw_hash, system_hash,
		first_msg_shape_hash, first_msg_blocks, prefix_bytes
		FROM requests ORDER BY ts DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var ts int64
		if err := rows.Scan(&ts, &e.Account, &e.Path, &e.Status, &e.DurationMs, &e.Bytes, &e.Event,
			&e.ModelAsked, &e.ModelServed, &e.UserAgent,
			&e.SessionHash, &e.InputTokens, &e.OutputTokens, &e.CacheCreationInputTokens, &e.CacheReadInputTokens,
			&e.ToolCount, &e.ToolsOrderHash, &e.ToolsSortedHash, &e.ToolsRawHash, &e.SystemHash,
			&e.FirstMsgShapeHash, &e.FirstMsgBlocks, &e.PrefixBytes); err != nil {
			return nil, err
		}
		e.Ts = time.UnixMilli(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
