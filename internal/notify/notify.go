// Package notify raises notifications when the pool needs attention (design
// doc §6.11, issue #101). The platform-local desktop notification —
// osascript on darwin, notify-send on Linux, a toast on Windows — is always
// available and always fires for the events it always fired for; issue #101
// adds optional channels (ntfy, a generic webhook, Pushover) that fan the
// same events out further, for the case a log line or an on-screen toast
// cannot reach: a request held for hours while nobody is looking at the
// screen.
//
// The point of `notify` mode is that a request is about to be parked for
// possibly hours. A log line does not tell someone who has walked away, and
// the alternative — a silent multi-hour hold — is exactly what §6.11 set out
// to avoid.
package notify

import (
	"context"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// coalesce is how long to suppress repeats. A dry pool produces one
// notification per burst, not one per queued request.
const coalesce = 10 * time.Minute

// sendTimeout bounds one channel's delivery. A channel that is unreachable
// must never delay the caller — this is what stands in for that deadline.
const sendTimeout = 10 * time.Second

// Notifier raises notifications, de-duplicated per subject, and — once
// SetChannels has been called — fanned out to every channel subscribed to
// the firing event.
type Notifier struct {
	mu   sync.Mutex
	last map[string]time.Time
	// send is the platform-local hook; swapped in tests, and wrapped as the
	// "os" provider by SetChannels.
	send func(ctx context.Context, title, body string) error
	// Enabled is false when the platform has no local notifier, so callers
	// can report honestly rather than pretending. This affects ONLY the
	// local/"os" delivery path — an explicitly configured webhook or ntfy
	// channel fires regardless, since it depends on nothing this process's
	// platform can or can't do.
	Enabled bool
	// channels are the explicitly configured destinations for Notify's
	// events (see SetChannels). Empty — the default — means "use the local
	// platform notifier for every event", i.e. today's exact behaviour
	// (issue #101: off by default).
	channels []channel
	logger   *slog.Logger
}

// New returns a Notifier for this platform, with no channels configured
// (today's local-only behaviour). Call SetChannels to add destinations.
func New() *Notifier {
	n := &Notifier{last: map[string]time.Time{}}
	switch runtime.GOOS {
	case "darwin":
		n.send, n.Enabled = notifyDarwin, true
	case "linux":
		if _, err := exec.LookPath("notify-send"); err == nil {
			n.send, n.Enabled = notifyLinux, true
		}
	case "windows":
		// Every supported Windows version ships PowerShell, but a locked-down
		// machine may not let us run it — check rather than promise.
		if _, err := exec.LookPath("powershell"); err == nil {
			n.send, n.Enabled = notifyWindows, true
		}
	}
	if n.send == nil {
		n.send = func(context.Context, string, string) error { return nil }
	}
	return n
}

// shouldSend applies the one dedup rule shared by every notification this
// package sends, channel-routed or local-only: a key firing again inside
// coalesce is suppressed. Returns true the first time a key is seen, and
// again after coalesce has elapsed.
func (n *Notifier) shouldSend(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if t, ok := n.last[key]; ok && time.Since(t) < coalesce {
		return false
	}
	n.last[key] = time.Now()
	return true
}

// Notify raises a notification for event, unless key was raised recently
// (see shouldSend). Never blocks the caller for long and never returns an
// error worth acting on: a failed notification must not affect request
// handling.
//
// Dedup happens once, here; every channel subscribed to event then receives
// that single firing — a pile-up produces one notification per channel, not
// one per request (issue #91).
//
// When no channels are configured (SetChannels never called, or called with
// nil), this is exactly today's behaviour: the local platform notifier,
// unconditionally, for every event.
func (n *Notifier) Notify(event, key, title, body string) {
	if !n.shouldSend(key) {
		return
	}
	n.mu.Lock()
	chans := n.channels
	n.mu.Unlock()

	if len(chans) == 0 {
		n.sendLocal(title, body)
		return
	}
	note := Notification{Event: event, Key: key, Title: title, Body: body}
	for _, ch := range chans {
		if !ch.enabled || !ch.events[event] {
			continue
		}
		go n.deliver(ch, note)
	}
}

// NotifyLocal raises a local-only notification, bypassing channels
// entirely. For the small set of notifications outside issue #101's event
// vocabulary (a replaced MITM certificate; an account starting to bill for
// extra usage) that remain exactly what they were before this package grew
// channels. Shares Notify's dedup map — one dedup rule for everything this
// package sends, not two.
func (n *Notifier) NotifyLocal(key, title, body string) {
	if !n.shouldSend(key) {
		return
	}
	n.sendLocal(title, body)
}

func (n *Notifier) sendLocal(title, body string) {
	if !n.Enabled {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.send(ctx, title, body)
	}()
}

// deliver sends one notification to one channel, bounded by sendTimeout so
// an unreachable destination can never delay the request that triggered it
// (issue #101). Failures are logged once, naming the channel — never the
// destination, which is secret — and dropped; delivery is never retried.
func (n *Notifier) deliver(ch channel, note Notification) {
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	if err := ch.provider.Send(ctx, ch.dest, note); err != nil {
		n.log().Warn("notification failed", "channel", ch.name, "event", note.Event, "err", err)
	}
}

// escapeAppleScript quotes a string for embedding in an osascript literal.
// Notification text includes account names, so this is not decorative.
func escapeAppleScript(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return r.Replace(s)
}

func notifyDarwin(ctx context.Context, title, body string) error {
	script := `display notification "` + escapeAppleScript(body) +
		`" with title "` + escapeAppleScript(title) + `"`
	return exec.CommandContext(ctx, "osascript", "-e", script).Run()
}

func notifyLinux(ctx context.Context, title, body string) error {
	return exec.CommandContext(ctx, "notify-send", "--app-name=spillway", title, body).Run()
}
