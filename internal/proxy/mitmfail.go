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
//
// The stale-CA stranded-client detector (below) keys its OWN recurrence
// check differently — see classifyHandshakeFailure — because "don't log
// 11,665 near-identical lines" and "is this the same failure recurring"
// turned out to be different questions (issue #96).
import (
	"log/slog"
	"strings"
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

// strandedRecurWindow bounds how soon a repeat of the same (host, coarse
// failure class) pair has to arrive to count as the SAME client still stuck
// retrying against a replaced CA, rather than an unrelated later failure
// that happens to land in the same — often generic ("eof") — class on the
// same host. A client stuck on a stale trust anchor retries far more often
// than this; two failures this far apart are treated as unrelated, so it
// takes a fresh pairing to count as evidence again.
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

// Coarse failure classes for the stale-CA stranded-client detector (issue
// #96). What the detector needs is "same host, same KIND of failure" —
// never the exact bytes of a Go network error, which vary by platform for
// the identical underlying event: Windows reports a torn-down connection
// as "wsarecv: An existing connection was forcibly closed by the remote
// host" where Unix says "read: connection reset by peer", and a client
// rejecting our MITM leaf can surface as any of several TLS alerts
// ("bad certificate", "unknown certificate authority") or even a garbled
// decrypt ("bad record MAC") from a client still holding pre-regeneration
// key material. Keying recurrence on the raw normalized string (as
// normalizeHandshakeDetail produces, for the log throttle) missed exactly
// this: two occurrences of the same real-world failure landing on
// different text shapes never compared equal, so the recurrence the
// detector exists to catch was silently missed — worst on Windows, the
// platform this repo can least easily inspect (ci.yml is "written blind").
const (
	classHandshakeRejected = "tls-handshake-rejected"
	classConnReset         = "connection-reset"
	classEOF               = "eof"
	classTimeout           = "timeout"
	classOther             = "other"
)

// classifyHandshakeFailure buckets a handshake-failure detail into a
// coarse, platform-stable class. An unrecognised message still gets a
// stable class (classOther) rather than falling back to the raw text:
// falling back to raw text is exactly the bug this exists to fix — it
// would silently reintroduce #96 on whatever future platform or Go version
// emits a shape this function does not yet know about.
func classifyHandshakeFailure(detail string) string {
	d := strings.ToLower(detail)
	switch {
	case strings.Contains(d, "tls:"):
		// Any TLS-layer alert or crypto failure: the client rejecting our
		// leaf (bad certificate / unknown certificate authority /
		// certificate expired) or a garbled decrypt (bad record MAC) —
		// different wire-level shapes of the same root cause, a client
		// that does not hold key material this run's CA will vouch for.
		return classHandshakeRejected
	case strings.Contains(d, "connection reset by peer"),
		strings.Contains(d, "forcibly closed"),
		strings.Contains(d, "connection aborted"),
		strings.Contains(d, "was aborted"),
		strings.Contains(d, "broken pipe"),
		strings.Contains(d, "econnreset"):
		// "connection reset by peer" (Unix) and Windows' "wsarecv: An
		// existing connection was forcibly closed by the remote host" /
		// "wsasend: An established connection was aborted..." are the same
		// event — the peer tore the TCP connection down mid-handshake —
		// reported through different syscalls with different wording.
		return classConnReset
	case strings.Contains(d, "eof"):
		return classEOF
	case strings.Contains(d, "timeout"), strings.Contains(d, "deadline exceeded"):
		return classTimeout
	default:
		return classOther
	}
}

// noteStranded folds one raw handshake failure into the stale-CA detector.
// key is (host, coarse failure class) — see classifyHandshakeFailure — not
// (host, raw detail): recurrence needs "same kind of failure", which the
// raw text does not reliably give across platforms. Must be called with
// l.mu held.
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
	l.noteStranded(host+"\x00"+classifyHandshakeFailure(detail), now)
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
