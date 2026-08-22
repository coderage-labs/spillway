// Package notify raises a desktop notification when the pool runs dry
// (design doc §6.11).
//
// The point of `notify` mode is that a request is about to be parked for
// possibly hours. A log line does not tell someone who has walked away, and
// the alternative — a silent multi-hour hold — is exactly what §6.11 set out
// to avoid.
package notify

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// coalesce is how long to suppress repeats. A dry pool produces one
// notification per burst, not one per queued request.
const coalesce = 10 * time.Minute

// Notifier raises desktop notifications, de-duplicated per subject.
type Notifier struct {
	mu   sync.Mutex
	last map[string]time.Time
	// send is the platform hook; swapped in tests.
	send func(ctx context.Context, title, body string) error
	// Enabled is false when the platform has no notifier, so callers can
	// report honestly rather than pretending.
	Enabled bool
}

// New returns a Notifier for this platform.
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

// Notify raises a notification unless one with the same key was raised
// recently. Never blocks the caller for long and never returns an error worth
// acting on: a failed notification must not affect request handling.
func (n *Notifier) Notify(key, title, body string) {
	if !n.Enabled {
		return
	}
	n.mu.Lock()
	if t, ok := n.last[key]; ok && time.Since(t) < coalesce {
		n.mu.Unlock()
		return
	}
	n.last[key] = time.Now()
	n.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.send(ctx, title, body)
	}()
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
