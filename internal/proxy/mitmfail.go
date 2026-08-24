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

	// Stale-CA warning (issue #66) — see the doc comment on activate/log
	// below. Kept in this struct rather than a separate type because it
	// rides the exact same (host, detail) failures this logger already
	// throttles; there is no second place these are observed.
	strandedArmed    bool
	strandedLastSeen map[string]time.Time
	strandedEvidence time.Time
	// strandedRecur and strandedStale default to strandedRecurWindow and
	// strandedStaleWindow; overridable only by
	// newMitmFailLoggerStrandedWindows, so tests can observe the decay
	// without a real 15-minute sleep.
	strandedRecur time.Duration
	strandedStale time.Duration
}

// strandedRecurWindow bounds how soon a repeat of the exact same (host,
// detail) pair has to arrive to count as the SAME client still stuck
// retrying against a replaced CA, rather than an unrelated later failure
// that happens to produce an identical — often generic ("EOF") — detail
// string on the same host. A client stuck on a stale trust anchor retries
// far more often than this; two failures this far apart are treated as
// unrelated, so it takes a fresh pairing to count as evidence again.
const strandedRecurWindow = 2 * time.Minute

// strandedStaleWindow is how long Stranded keeps reporting true after the
// most recent recurrence. Once every affected session has been restarted,
// failures stop, and after this much quiet the warning clears itself —
// deliberately, so it cannot latch forever after a single regeneration
// (issue #66's ask).
const strandedStaleWindow = 15 * time.Minute

func newMitmFailLogger(logger *slog.Logger) *mitmFailLogger {
	return newMitmFailLoggerWindow(logger, defaultMitmFailWindow)
}

// newMitmFailLoggerWindow lets tests use a window short enough to observe a
// second log line without a real-time sleep.
func newMitmFailLoggerWindow(logger *slog.Logger, window time.Duration) *mitmFailLogger {
	return newMitmFailLoggerStrandedWindows(logger, window, strandedRecurWindow, strandedStaleWindow)
}

// newMitmFailLoggerStrandedWindows lets tests shrink the stale-CA recur and
// decay windows so both can be observed without a real multi-minute sleep.
func newMitmFailLoggerStrandedWindows(logger *slog.Logger, window, recur, stale time.Duration) *mitmFailLogger {
	if logger == nil {
		logger = slog.Default()
	}
	return &mitmFailLogger{
		logger:           logger,
		window:           window,
		seen:             map[string]*mitmFailEntry{},
		strandedLastSeen: map[string]time.Time{},
		strandedRecur:    recur,
		strandedStale:    stale,
	}
}

// activate marks that this run genuinely replaced the MITM CA (see
// mitm.CA.Regenerated) — never on an ordinary restart with an unchanged host
// set (#70) and never on a first-ever install. Before this is called,
// Stranded always reports false: a handshake failure with no preceding
// regeneration is ordinary churn, not evidence (issue #66).
func (l *mitmFailLogger) activate() {
	l.mu.Lock()
	l.strandedArmed = true
	l.mu.Unlock()
}

// noteStranded folds one raw handshake failure into the stale-CA detector.
// Must be called with l.mu held.
func (l *mitmFailLogger) noteStranded(key string, now time.Time) {
	if !l.strandedArmed {
		return
	}
	if last, ok := l.strandedLastSeen[key]; ok && now.Sub(last) < l.strandedRecur {
		// The identical failure has recurred quickly enough to be the same
		// client still stuck retrying — that is the symptom, not a single
		// mundane disconnect.
		l.strandedEvidence = now
	}
	l.strandedLastSeen[key] = now
}

// Stranded reports whether at least one client currently looks stuck
// trusting a CA this run replaced: armed by activate, and true only while a
// recurring handshake failure has been seen within strandedStaleWindow. It
// decays back to false on its own once the failures stop, so a warning
// raised here never latches forever (issue #66).
func (l *mitmFailLogger) Stranded() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.strandedArmed && !l.strandedEvidence.IsZero() &&
		time.Since(l.strandedEvidence) < l.strandedStale
}

// log records one failure for (host, detail) and emits a slog warning either
// immediately (first time this pair is seen, or the window has elapsed since
// the last time it was logged) or not at all — in which case it is folded
// into the count the next window-boundary log line reports.
func (l *mitmFailLogger) log(host, detail string) {
	key := host + "\x00" + detail
	now := time.Now()

	l.mu.Lock()
	l.noteStranded(key, now)
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
