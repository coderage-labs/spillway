package proxy

// mitmFailLogger rate-limits the "mitm connection failed" warning emitted by
// slogWriter (connect.go) for a TLS server error on a terminated CONNECT.
//
// Issue #64: a client that permanently fails TLS verification against a
// MITM'd host (a non-Node subprocess with no trust for spillway's CA)
// retries indefinitely. Measured live: ~7 retries/minute, forever, producing
// 11,665 identical "mitm connection failed" lines and a 3.6MB log that was
// ~99% this one loop — which is exactly what buried the real Remote Control
// failure (issue #65's incident) in noise for hours. That problem exists
// independently of whatever causes the failures, so it needs fixing on its
// own terms rather than assuming the CA-bundle fix (bundle.go) eliminates
// every source of repeated identical failures.
//
// The key is (host, detail): a client failing for one reason must not
// suppress logging of a DIFFERENT failure on the same host, or the same
// failure detail recurring on a different host.
import (
	"log/slog"
	"sync"
	"time"
)

// defaultMitmFailWindow bounds how often a repeating (host, detail) pair
// gets logged again: the first occurrence always logs immediately, and
// afterwards at most once per window, carrying how many were suppressed.
const defaultMitmFailWindow = time.Minute

type mitmFailEntry struct {
	lastLog time.Time
	count   int // failures suppressed since lastLog, not counting the one that logged
}

// mitmFailLogger is safe for concurrent use — CONNECT handling runs one
// goroutine per tunnel.
type mitmFailLogger struct {
	mu     sync.Mutex
	logger *slog.Logger
	window time.Duration
	seen   map[string]*mitmFailEntry
}

func newMitmFailLogger(logger *slog.Logger) *mitmFailLogger {
	return newMitmFailLoggerWindow(logger, defaultMitmFailWindow)
}

// newMitmFailLoggerWindow lets tests use a window short enough to observe a
// second log line without a real-time sleep.
func newMitmFailLoggerWindow(logger *slog.Logger, window time.Duration) *mitmFailLogger {
	if logger == nil {
		logger = slog.Default()
	}
	return &mitmFailLogger{logger: logger, window: window, seen: map[string]*mitmFailEntry{}}
}

// log records one failure for (host, detail) and emits a slog warning either
// immediately (first time this pair is seen, or the window has elapsed since
// the last time it was logged) or not at all — in which case it is folded
// into the count the next window-boundary log line reports.
func (l *mitmFailLogger) log(host, detail string) {
	key := host + "\x00" + detail
	now := time.Now()

	l.mu.Lock()
	e, ok := l.seen[key]
	if !ok {
		e = &mitmFailEntry{}
		l.seen[key] = e
	}
	var suppressed int
	logNow := !ok || now.Sub(e.lastLog) >= l.window
	if logNow {
		suppressed = e.count
		e.count = 0
		e.lastLog = now
	} else {
		e.count++
	}
	l.mu.Unlock()

	if !logNow {
		return
	}
	if suppressed > 0 {
		l.logger.Warn("mitm connection failed", "host", host, "detail", detail,
			"suppressed_repeats", suppressed, "window", l.window.String())
		return
	}
	l.logger.Warn("mitm connection failed", "host", host, "detail", detail)
}
