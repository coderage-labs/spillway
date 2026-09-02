package notify

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorder captures what was sent. Notify dispatches on its own goroutine —
// deliberately, so a slow osascript cannot stall a request — so distinct keys
// send concurrently and the record must be guarded.
type recorder struct {
	mu     sync.Mutex
	count  atomic.Int32
	bodies []string
}

func (r *recorder) sent() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...)
}

func testNotifier() (*Notifier, *recorder) {
	rec := &recorder{}
	no := &Notifier{last: map[string]time.Time{}, Enabled: true}
	no.send = func(_ context.Context, title, body string) error {
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, title+"|"+body)
		rec.mu.Unlock()
		rec.count.Add(1)
		return nil
	}
	return no, rec
}

// A dry pool queues many requests; the user wants one notification, not one
// per request.
func TestNotifyCoalescesRepeats(t *testing.T) {
	n, rec := testNotifier()
	for i := 0; i < 5; i++ {
		n.Notify(EventExhausted, "pool-exhausted", "t", "b")
	}
	waitFor(t, &rec.count, 1)
	if got := rec.count.Load(); got != 1 {
		t.Errorf("sent %d notifications for one burst, want 1", got)
	}
}

func TestNotifyDistinctKeysAreIndependent(t *testing.T) {
	n, rec := testNotifier()
	n.Notify(EventExhausted, "a", "t", "b")
	n.Notify(EventExhausted, "b", "t", "b")
	waitFor(t, &rec.count, 2)
	if got := rec.count.Load(); got != 2 {
		t.Errorf("sent %d, want 2 — different subjects must not coalesce", got)
	}
	if got := len(rec.sent()); got != 2 {
		t.Errorf("recorded %d notifications, want 2", got)
	}
}

// Disabled platforms must be silent, not error, and callers must be able to
// tell rather than assume a notification happened. This is the no-channels
// default path (issue #101): SetChannels was never called, so Notify falls
// back to the local platform notifier exactly as before channels existed.
func TestDisabledNotifierIsSilent(t *testing.T) {
	n := &Notifier{last: map[string]time.Time{}, Enabled: false}
	sent := 0
	n.send = func(context.Context, string, string) error { sent++; return nil }
	n.Notify(EventExhausted, "k", "t", "b")
	time.Sleep(50 * time.Millisecond)
	if sent != 0 {
		t.Error("a disabled notifier must not send")
	}
}

// NotifyLocal is the escape hatch for the two notifications outside the
// channel-eligible event set (MITM cert replaced, started billing extra
// usage) — it must still reach the local platform notifier, and still obey
// the one shared dedup rule.
func TestNotifyLocalSendsAndCoalesces(t *testing.T) {
	n, rec := testNotifier()
	for i := 0; i < 3; i++ {
		n.NotifyLocal("mitm-ca-regenerated", "t", "b")
	}
	waitFor(t, &rec.count, 1)
	if got := rec.count.Load(); got != 1 {
		t.Errorf("sent %d, want 1", got)
	}
}

// Notification text carries account names, so a quote must not break out of
// the AppleScript literal.
func TestEscapeAppleScript(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:            `plain`,
		`say "hi"`:         `say \"hi\"`,
		`back\slash`:       `back\\slash`,
		`both "x" and \ y`: `both \"x\" and \\ y`,
	} {
		if got := escapeAppleScript(in); got != want {
			t.Errorf("escapeAppleScript(%q) = %q, want %q", in, got, want)
		}
	}
}

func waitFor(t *testing.T, c *atomic.Int32, want int32) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if c.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Toast text carries account names, which come from config. An unescaped
// angle bracket ends the <text> element and the rest becomes markup the
// toast engine tries to interpret.
func TestEscapeXML(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:             `plain`,
		`a & b`:             `a &amp; b`,
		`<text>`:            `&lt;text&gt;`,
		`say "hi"`:          `say &quot;hi&quot;`,
		`it's`:              `it&apos;s`,
		`</text><audio s="`: `&lt;/text&gt;&lt;audio s=&quot;`,
	} {
		if got := escapeXML(in); got != want {
			t.Errorf("escapeXML(%q) = %q, want %q", in, got, want)
		}
	}
}
