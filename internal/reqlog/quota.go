package reqlog

// Quota history: the request log's second table. Headroom over time is the
// one thing the dashboard cannot derive from live state alone — a burn rate
// needs a past to measure against, and that is what turns "34% used" into
// "dry in 2h10m, before its 3h22m refill".

import (
	"time"
)

// Sample is one observation of one quota window.
type Sample struct {
	Ts      time.Time `json:"ts"`
	Account string    `json:"account"`
	Window  string    `json:"window"`
	Limit   float64   `json:"limit"`
	Used    float64   `json:"used"`
	ResetAt time.Time `json:"resetAt,omitempty"`
}

// sampleInterval throttles writes: quota headers arrive on every response,
// but a point per account/window per minute is plenty to draw a curve and
// keeps the table small enough to never need pruning in practice.
const sampleInterval = time.Minute

func (l *Log) initQuota() error {
	_, err := l.db.Exec(`CREATE TABLE IF NOT EXISTS quota_samples (
		ts INTEGER NOT NULL,
		account TEXT NOT NULL,
		window TEXT NOT NULL,
		lim REAL NOT NULL,
		used REAL NOT NULL,
		reset_at INTEGER
	)`)
	if err != nil {
		return err
	}
	if _, err = l.db.Exec(`CREATE INDEX IF NOT EXISTS quota_ts ON quota_samples (ts)`); err != nil {
		return err
	}
	// Kimi's 7-day window used to be recorded as "weekly", before window
	// names were normalised onto Claude's 5h/7d vocabulary. The name is part
	// of the row, so without this the same window draws as two series that
	// each stop or start on the day of the upgrade. Idempotent, and a no-op
	// on every database that never held a Kimi account.
	_, err = l.db.Exec(`UPDATE quota_samples SET window = '7d' WHERE window = 'weekly'`)
	return err
}

// RecordQuota writes a sample unless one for this account+window landed
// within sampleInterval.
func (l *Log) RecordQuota(s Sample) error {
	if s.Ts.IsZero() {
		s.Ts = time.Now()
	}
	var last int64
	err := l.db.QueryRow(`SELECT ts FROM quota_samples WHERE account = ? AND window = ?
		ORDER BY ts DESC LIMIT 1`, s.Account, s.Window).Scan(&last)
	if err == nil && s.Ts.Sub(time.UnixMilli(last)) < sampleInterval {
		return nil
	}
	var reset any
	if !s.ResetAt.IsZero() {
		reset = s.ResetAt.UnixMilli()
	}
	_, err = l.db.Exec(`INSERT INTO quota_samples (ts, account, window, lim, used, reset_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		s.Ts.UnixMilli(), s.Account, s.Window, s.Limit, s.Used, reset)
	return err
}

// QuotaSince returns samples newer than cutoff, oldest first.
func (l *Log) QuotaSince(cutoff time.Time) ([]Sample, error) {
	rows, err := l.db.Query(`SELECT ts, account, window, lim, used, reset_at
		FROM quota_samples WHERE ts >= ? ORDER BY ts ASC`, cutoff.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var s Sample
		var ts int64
		var reset *int64
		if err := rows.Scan(&ts, &s.Account, &s.Window, &s.Limit, &s.Used, &reset); err != nil {
			return nil, err
		}
		s.Ts = time.UnixMilli(ts)
		if reset != nil {
			s.ResetAt = time.UnixMilli(*reset)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Bucket is one time bucket of request activity.
type Bucket struct {
	Ts      time.Time `json:"ts"`
	Count   int       `json:"count"`
	Errors  int       `json:"errors"`
	Rotated int       `json:"rotated"`
	P95Ms   int64     `json:"p95_ms"`
}

// Activity buckets requests since cutoff into fixed-width slots. Empty slots
// are included so the histogram shows real gaps rather than compressing time.
func (l *Log) Activity(cutoff time.Time, width time.Duration, now time.Time) ([]Bucket, error) {
	if width <= 0 {
		width = time.Minute
	}
	// Stored timestamps are millisecond-truncated; comparing them against a
	// nanosecond-precision cutoff drops boundary rows into the bucket before.
	cutoff = cutoff.Truncate(time.Millisecond)
	rows, err := l.db.Query(`SELECT ts, status, event, duration_ms FROM requests
		WHERE ts >= ? ORDER BY ts ASC`, cutoff.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	n := int(now.Sub(cutoff)/width) + 1
	if n < 1 {
		n = 1
	}
	out := make([]Bucket, n)
	durs := make([][]int64, n)
	for i := range out {
		out[i].Ts = cutoff.Add(time.Duration(i) * width)
	}
	for rows.Next() {
		var ts, dur int64
		var status int
		var event string
		if err := rows.Scan(&ts, &status, &event, &dur); err != nil {
			return nil, err
		}
		i := int(time.UnixMilli(ts).Sub(cutoff) / width)
		if i < 0 || i >= n {
			continue
		}
		out[i].Count++
		if status >= 400 {
			out[i].Errors++
		}
		if event == EventRotatedQuota || event == EventRotatedConn {
			out[i].Rotated++
		}
		durs[i] = append(durs[i], dur)
	}
	for i := range out {
		out[i].P95Ms = p95(durs[i])
	}
	return out, rows.Err()
}

func p95(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	for i := 1; i < len(v); i++ { // insertion sort: buckets are tiny
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
	idx := (len(v)*95 + 99) / 100
	if idx >= len(v) {
		idx = len(v) - 1
	}
	return v[idx]
}
