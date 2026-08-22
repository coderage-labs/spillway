// Package reqlog is the SQLite request log (design doc §5, §7): one row per
// request with metadata only — NEVER auth headers or bodies.
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
}

// Log is an open request-log database.
type Log struct {
	db *sql.DB
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
	for _, col := range []string{"model_asked", "model_served"} {
		if _, err := db.Exec(`ALTER TABLE requests ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("add %s column: %w", col, err)
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
// no header/body columns by design (§5 redaction).
func (l *Log) Record(e Entry) error {
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}
	_, err := l.db.Exec(`INSERT INTO requests
		(ts, account, path, status, duration_ms, bytes, event, model_asked, model_served)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Ts.UnixMilli(), e.Account, e.Path, e.Status, e.DurationMs, e.Bytes, e.Event,
		e.ModelAsked, e.ModelServed)
	return err
}

// Recent returns the newest limit entries, newest first.
func (l *Log) Recent(limit int) ([]Entry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := l.db.Query(`SELECT ts, account, path, status, duration_ms, bytes, event,
		model_asked, model_served
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
			&e.ModelAsked, &e.ModelServed); err != nil {
			return nil, err
		}
		e.Ts = time.UnixMilli(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
