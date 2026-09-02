package notify

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// recordingProvider captures every Send call it receives — the destination
// too, so tests can assert it never leaks anywhere it shouldn't (loggers
// are checked separately in TestDestinationNeverLogged).
type recordingProvider struct {
	mu    sync.Mutex
	sends []Notification
	// err, when set, is returned by every Send.
	err error
	// delay, when set, blocks Send for this long before returning — used to
	// prove one slow/unreachable channel cannot delay another.
	delay time.Duration
}

func (p *recordingProvider) Send(ctx context.Context, _ Destination, n Notification) error {
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.mu.Lock()
	p.sends = append(p.sends, n)
	p.mu.Unlock()
	return p.err
}

func (p *recordingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sends)
}

func testChannel(name string, events []string, p Provider) channel {
	evs := map[string]bool{}
	for _, e := range events {
		evs[e] = true
	}
	return channel{name: name, provider: p, events: evs, enabled: true}
}

func waitForCount(t *testing.T, get func() int, want int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if get() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for count >= %d, got %d", want, get())
}

// One event must fan out to every channel subscribed to it, and to no
// channel that isn't.
func TestFanOutOnlyToSubscribedChannels(t *testing.T) {
	phone := &recordingProvider{}
	desktop := &recordingProvider{}
	unrelated := &recordingProvider{}

	n := &Notifier{last: map[string]time.Time{}}
	n.channels = []channel{
		testChannel("phone", []string{EventExhausted, EventHeld}, phone),
		testChannel("desktop", []string{EventOverageCap}, desktop),
		testChannel("other", []string{EventAccountDisabled}, unrelated),
	}

	n.Notify(EventExhausted, "k1", "t", "b")
	waitForCount(t, phone.count, 1)

	time.Sleep(50 * time.Millisecond)
	if got := desktop.count(); got != 0 {
		t.Errorf("desktop (not subscribed to exhausted) got %d sends, want 0", got)
	}
	if got := unrelated.count(); got != 0 {
		t.Errorf("other (not subscribed to exhausted) got %d sends, want 0", got)
	}
	if got := phone.count(); got != 1 {
		t.Errorf("phone got %d sends, want 1", got)
	}
}

// A dedup key must fire once across many triggering calls, and that ONE
// firing must still reach every subscribed channel (issue #91: a 51-request
// pile-up produces one notification per channel, not 51 total).
func TestDedupOnceThenFanOutPerChannel(t *testing.T) {
	phone := &recordingProvider{}
	desktop := &recordingProvider{}

	n := &Notifier{last: map[string]time.Time{}}
	n.channels = []channel{
		testChannel("phone", []string{EventExhausted}, phone),
		testChannel("desktop", []string{EventExhausted}, desktop),
	}

	for i := 0; i < 51; i++ {
		n.Notify(EventExhausted, "pool-exhausted-all", "t", "b")
	}
	waitForCount(t, phone.count, 1)
	waitForCount(t, desktop.count, 1)
	time.Sleep(50 * time.Millisecond)
	if got := phone.count(); got != 1 {
		t.Errorf("phone got %d sends for 51 events on one key, want 1", got)
	}
	if got := desktop.count(); got != 1 {
		t.Errorf("desktop got %d sends for 51 events on one key, want 1", got)
	}
}

// An unreachable channel must never delay the caller, and must never stop
// another channel from delivering.
func TestUnreachableChannelDoesNotBlockOrStopOthers(t *testing.T) {
	slow := &recordingProvider{delay: 5 * time.Second} // far longer than sendTimeout
	fast := &recordingProvider{}

	n := &Notifier{last: map[string]time.Time{}}
	n.channels = []channel{
		testChannel("slow", []string{EventHeld}, slow),
		testChannel("fast", []string{EventHeld}, fast),
	}

	start := time.Now()
	n.Notify(EventHeld, "k", "t", "b")
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Notify blocked the caller for %s — sends must be fire-and-forget", elapsed)
	}
	waitForCount(t, fast.count, 1)
	if got := fast.count(); got != 1 {
		t.Errorf("fast channel got %d sends, want 1 — a slow sibling must not stop it", got)
	}
}

// A provider that errors on every Send must not affect a sibling channel.
func TestFailingChannelDoesNotStopSibling(t *testing.T) {
	failing := &recordingProvider{err: errors.New("boom")}
	ok := &recordingProvider{}

	n := &Notifier{last: map[string]time.Time{}}
	n.channels = []channel{
		testChannel("failing", []string{EventOverageCap}, failing),
		testChannel("ok", []string{EventOverageCap}, ok),
	}
	n.Notify(EventOverageCap, "k", "t", "b")
	waitForCount(t, ok.count, 1)
	waitForCount(t, failing.count, 1) // it was still tried
}

// No channels configured must reproduce today's exact local-only behaviour:
// every event still reaches the platform notifier, unconditionally.
func TestNoChannelsPreservesLocalOnlyBehaviour(t *testing.T) {
	n, rec := testNotifier() // channels is nil
	for _, ev := range ValidEvents() {
		n.Notify(ev, "key-"+ev, "t", "b")
	}
	waitFor(t, &rec.count, int32(len(ValidEvents())))
	if got := rec.count.Load(); got != int32(len(ValidEvents())) {
		t.Errorf("local sends = %d, want %d (one per event, no channels configured)",
			got, len(ValidEvents()))
	}
}

// syncBuffer is a mutex-guarded io.Writer — slog writes from the delivery
// goroutine while the test reads, and a plain bytes.Buffer is not safe for
// that (caught by -race: this test used to race on the buffer itself before
// switching to this type).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// A destination must never appear in any log output — not on send, not on
// failure. Only the channel NAME may be logged.
func TestDestinationNeverLogged(t *testing.T) {
	const secretURL = "https://hooks.example.com/T00/secret-token-xyz"
	const secretTopic = "unguessable-topic-abc123"

	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, nil))

	failing := &recordingProvider{err: errors.New("delivery refused")}
	n := &Notifier{last: map[string]time.Time{}, logger: logger}
	n.channels = []channel{
		{
			name:     "leaky-webhook",
			provider: failing,
			events:   map[string]bool{EventExhausted: true},
			dest:     Destination{URL: secretURL, Topic: secretTopic, Token: "tk_supersecret"},
			enabled:  true,
		},
	}

	n.Notify(EventExhausted, "k", "spillway: all accounts exhausted", "All accounts exhausted")
	waitForCount(t, failing.count, 1)
	// The log line is written just after Send returns, in the same
	// delivery goroutine — poll rather than assume it has already landed
	// the instant Send's own side effect is observed.
	var out string
	for i := 0; i < 200; i++ {
		if out = buf.String(); out != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if out == "" {
		t.Fatal("expected a warning log line for the failed send, got none")
	}
	for _, secret := range []string{secretURL, secretTopic, "tk_supersecret", "hooks.example.com"} {
		if bytesContains(out, secret) {
			t.Errorf("log output contains destination material %q:\n%s", secret, out)
		}
	}
	if !bytesContains(out, "leaky-webhook") {
		t.Errorf("log output should name the channel; got:\n%s", out)
	}
}

func bytesContains(haystack, needle string) bool {
	return len(needle) > 0 && bytes.Contains([]byte(haystack), []byte(needle))
}
