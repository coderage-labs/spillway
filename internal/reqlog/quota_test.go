package reqlog

import (
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Log {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestRecordQuotaThrottlesWithinInterval(t *testing.T) {
	l := openTest(t)
	base := time.Now()
	for _, offset := range []time.Duration{0, 10 * time.Second, 30 * time.Second} {
		if err := l.RecordQuota(Sample{Ts: base.Add(offset), Account: "a", Window: "5h",
			Limit: 1, Used: 0.1}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.QuotaSince(base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("samples inside the throttle window should collapse to 1, got %d", len(got))
	}
	// Past the interval a new point lands.
	if err := l.RecordQuota(Sample{Ts: base.Add(2 * time.Minute), Account: "a", Window: "5h",
		Limit: 1, Used: 0.3}); err != nil {
		t.Fatal(err)
	}
	got, _ = l.QuotaSince(base.Add(-time.Hour))
	if len(got) != 2 {
		t.Fatalf("expected a second sample past the interval, got %d", len(got))
	}
}

func TestRecordQuotaSeparatesWindows(t *testing.T) {
	l := openTest(t)
	now := time.Now()
	for _, w := range []string{"5h", "7d"} {
		if err := l.RecordQuota(Sample{Ts: now, Account: "a", Window: w, Limit: 1, Used: 0.2}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := l.QuotaSince(now.Add(-time.Hour))
	if len(got) != 2 {
		t.Fatalf("windows are throttled independently; want 2 samples, got %d", len(got))
	}
}

func TestQuotaSinceOrdersOldestFirstAndKeepsReset(t *testing.T) {
	l := openTest(t)
	now := time.Now().Truncate(time.Millisecond)
	reset := now.Add(3 * time.Hour).Truncate(time.Millisecond)
	for i, used := range []float64{0.1, 0.4, 0.8} {
		if err := l.RecordQuota(Sample{Ts: now.Add(time.Duration(i*5) * time.Minute),
			Account: "a", Window: "5h", Limit: 1, Used: used, ResetAt: reset}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := l.QuotaSince(now.Add(-time.Hour))
	if len(got) != 3 {
		t.Fatalf("want 3 samples, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Ts.Before(got[i-1].Ts) {
			t.Fatal("samples must come back oldest first")
		}
	}
	if !got[0].ResetAt.Equal(reset) {
		t.Errorf("resetAt round-trip: got %v want %v", got[0].ResetAt, reset)
	}
	if got[2].Used != 0.8 {
		t.Errorf("used round-trip: got %v", got[2].Used)
	}
}

func TestQuotaSinceExcludesOlderThanCutoff(t *testing.T) {
	l := openTest(t)
	now := time.Now()
	_ = l.RecordQuota(Sample{Ts: now.Add(-3 * time.Hour), Account: "a", Window: "5h", Limit: 1, Used: 0.1})
	_ = l.RecordQuota(Sample{Ts: now, Account: "a", Window: "5h", Limit: 1, Used: 0.5})
	got, _ := l.QuotaSince(now.Add(-time.Hour))
	if len(got) != 1 || got[0].Used != 0.5 {
		t.Fatalf("cutoff not applied: %+v", got)
	}
}

func TestActivityBucketsIncludeEmptySlots(t *testing.T) {
	l := openTest(t)
	now := time.Now()
	cutoff := now.Add(-10 * time.Minute)
	// Two requests in the same minute, one 5 minutes later, nothing between.
	for _, e := range []Entry{
		{Ts: cutoff.Add(30 * time.Second), Account: "a", Path: "/v1/messages", Status: 200, DurationMs: 100, Event: EventServed},
		{Ts: cutoff.Add(40 * time.Second), Account: "a", Path: "/v1/messages", Status: 200, DurationMs: 300, Event: EventServed},
		{Ts: cutoff.Add(5 * time.Minute), Account: "a", Path: "/v1/messages", Status: 429, DurationMs: 900, Event: EventRotatedQuota},
	} {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	buckets, err := l.Activity(cutoff, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) < 10 {
		t.Fatalf("empty slots must be present so gaps show as gaps; got %d buckets", len(buckets))
	}
	if buckets[0].Count != 2 {
		t.Errorf("first bucket count: got %d want 2", buckets[0].Count)
	}
	if buckets[1].Count != 0 {
		t.Errorf("quiet minute should be an empty bucket, got %d", buckets[1].Count)
	}
	b5 := buckets[5]
	if b5.Count != 1 || b5.Errors != 1 || b5.Rotated != 1 {
		t.Errorf("bucket 5: got count=%d errors=%d rotated=%d, want 1/1/1", b5.Count, b5.Errors, b5.Rotated)
	}
	if buckets[0].P95Ms != 300 {
		t.Errorf("p95 of {100,300}: got %d want 300", buckets[0].P95Ms)
	}
}

func TestP95(t *testing.T) {
	for _, tc := range []struct {
		in   []int64
		want int64
	}{
		{nil, 0},
		{[]int64{5}, 5},
		{[]int64{3, 1, 2}, 3},
		{[]int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}, 100},
	} {
		if got := p95(append([]int64(nil), tc.in...)); got != tc.want {
			t.Errorf("p95(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Samples written under the old "weekly" name must be carried over, or the
// history chart draws one window as two series that hand over on upgrade day.
func TestOpenRenamesLegacyWeeklySamples(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "q.db")

	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Write a sample under the old name, the way an older build would have.
	if _, err := l.db.Exec(
		`INSERT INTO quota_samples (ts, account, window, lim, used) VALUES (?,?,?,?,?)`,
		time.Now().UnixMilli(), "kimi", "weekly", 100.0, 52.0); err != nil {
		t.Fatal(err)
	}
	l.Close()

	// Reopening runs the migration.
	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()

	var name string
	var used float64
	if err := l2.db.QueryRow(`SELECT window, used FROM quota_samples`).Scan(&name, &used); err != nil {
		t.Fatal(err)
	}
	if name != "7d" {
		t.Errorf("window = %q, want the legacy sample renamed to 7d", name)
	}
	if used != 52 {
		t.Errorf("used = %v, want the sample's data left alone", used)
	}
}
