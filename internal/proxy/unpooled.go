package proxy

// The counter-risk of inverting the routing default (issue #176), and the
// mitigation that has to ship with it.
//
// inference.go pools only what spillway RECOGNISES as inference. The cost
// of that is a silent one: if Anthropic ships a new inference endpoint —
// /v1/responses, say — an inverted spillway forwards it on the user's own
// credential. No rotation, no quota tracking, no holds. The product quietly
// stops doing its job for that traffic, and the user finds out when one
// account exhausts while seven sit idle. That is strictly worse than the
// bug being fixed, because it is invisible.
//
// So the inversion is only safe with three things, all of which are here or
// wired from here:
//
//  1. WARN ONCE per unseen path that receives a POST with a body — the
//     shape of an inference request — so a new endpoint surfaces on day one
//     rather than after 7,500 requests.
//  2. COUNT it somewhere durable (/api/state, `spillway status`), because
//     silence is what allowed both of the bugs this issue generalises.
//  3. Let the user ACT on it without waiting for a release —
//     Handler.SetInferencePaths, fed by proxy.inferencePaths in the config
//     and applied live by the reload watcher.
//
// THE WARNING'S RATE LIMIT IS ITS KEY, NOT A TIMER. The bookkeeping key is
// the REDACTED path template (see redactPath), not the raw path. That is
// load-bearing twice over:
//
//   - Without redaction, "once per path" IS "once per request" for any
//     /collection/<uuid> endpoint — /api/frame/<id> alone would have
//     produced 79 warnings. Collapsing every id to {id} bounds the warning
//     at one line per ENDPOINT SHAPE for the life of the process, which is
//     the only rate that makes a day-one signal readable.
//   - A path can carry identifying material. /v1/code/sessions/<uuid>,
//     /api/frame/read/<uuid> and /v1/environments/<uuid>/work/poll are all
//     real shapes in this codebase's identity trees, and a session or
//     artifact id is exactly the sort of value §5 keeps out of the log.
//     Redaction is applied BEFORE the value is stored or logged, and the
//     query string is never touched at all.
//
// It also must not fire for paths already known not to be inference —
// see knownNonInferenceTrees, seeded from issue #176's own measurements, so
// the warning starts quiet on real traffic and only speaks for something
// genuinely new.

import (
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// unpooledBodyHint is the body size at or above which a POST to an
// unrecognised path is treated as inference-shaped and warned about once.
//
// Deliberately low. The two errors are not symmetric: a false positive
// costs ONE WARN line per endpoint shape for the life of the process, while
// a false negative is the exact failure this whole file exists to prevent —
// a new inference endpoint quietly unpooled. A request with a model id and
// a messages array does not fit in 512 bytes even at its most trivial, and
// nothing in the measured non-inference traffic reaches here at all (it is
// suppressed by name below), so this threshold only ever has to separate
// "a new endpoint someone is POSTing real content to" from "a ping".
const unpooledBodyHint = 512

// maxUnpooledPaths bounds the tracked-template maps. Redaction already
// collapses per-item endpoints, so reaching this needs hundreds of distinct
// endpoint SHAPES — which no real client produces, but a stray scanner
// pointed at the proxy port would. Bounded rather than trusted: this map is
// fed by arbitrary request paths and lives for the life of the daemon.
const maxUnpooledPaths = 256

// maxPathSegments bounds the depth a recorded template keeps. Beyond it the
// tail is elided, so a pathologically deep path cannot become a long key.
const maxPathSegments = 8

// knownNonInferenceTrees are path trees MEASURED as non-inference on live
// traffic (issue #176's table) — every family that was being pool-routed to
// a real account before the inversion. They still pass through unpooled,
// like anything else spillway does not pool; listing them here only says
// "this is not news", so the day-one warning stays a signal.
//
// Distinct from isNonQuotaPath (passthrough.go), which is a narrower and
// stronger claim: those three are CONFIRMED to consume no quota and get
// their own request-log label. These are merely known not to be inference.
// Keeping the two lists apart keeps "confirmed free" and "seen, not
// inference" from being read as the same statement.
//
// Matched as trees by underPath, so /api/claude_code covers
// /api/claude_code/metrics and /api/claude_code/notification/preferences
// without either having to be listed. A config-supplied inference path
// still wins: isPooled is consulted before any of this.
var knownNonInferenceTrees = []string{
	// EC2 instance-metadata probes from AWS SDKs — 3,974 requests. These
	// reach spillway origin-form only in base-URL mode; the proxy-form
	// case is forwarded to the client's own host by #177's gate.
	"/latest",
	// Telemetry, settings, policy limits, notification preferences.
	"/api/claude_code",
	"/api/claude_cli",
	"/api/event_logging",
	// Evals, artifacts, domain lookups, oauth validation/bootstrap.
	"/api/eval",
	"/api/frame",
	"/api/web",
	"/api/oauth",
	// MCP server registry.
	"/mcp-registry",
}

// isKnownNonInference reports whether a path was measured as non-inference
// and therefore should not raise the unseen-path warning.
func isKnownNonInference(path string) bool {
	// The bare root, 39 requests in the measured traffic. Exact match only:
	// as a tree "/" is every path there is.
	if path == "" || path == "/" {
		return true
	}
	for _, base := range knownNonInferenceTrees {
		if underPath(path, base) {
			return true
		}
	}
	return false
}

// UnpooledStats is what the daemon reports about traffic it declined to
// pool because it did not recognise the path. Counted for the life of the
// process, like every other live signal on /api/state — this answers "is
// spillway missing something right now", not "what has it ever seen"; the
// request log is the durable record and already labels these "(unpooled)".
type UnpooledStats struct {
	// Requests is every request passed through because spillway did not
	// recognise the path. Identity-bound and confirmed-non-quota paths are
	// NOT counted: those are recognised, just not pooled.
	Requests int `json:"requests"`
	// Paths is how many distinct redacted templates those fell on.
	Paths int `json:"paths"`
	// InferenceShaped lists the redacted templates that received a POST
	// with a body and therefore raised the warning. This is the list a user
	// checks against proxy.inferencePaths.
	InferenceShaped []string `json:"inferenceShaped,omitempty"`
}

// unpooledTracker counts unrecognised-path traffic and warns once per
// inference-shaped template. Safe for concurrent use: every request path
// runs on its own goroutine.
type unpooledTracker struct {
	mu       sync.Mutex
	logger   *slog.Logger
	requests int
	// templates counts requests per redacted template; warned records the
	// templates that have already raised the warning, so it fires once each
	// rather than once per request.
	templates map[string]int
	warned    map[string]bool
	// overflow counts requests whose template could not be tracked because
	// maxUnpooledPaths was reached. They still count in requests.
	overflow int
}

func newUnpooledTracker(logger *slog.Logger) *unpooledTracker {
	return &unpooledTracker{
		logger:    logger,
		templates: map[string]int{},
		warned:    map[string]bool{},
	}
}

// note records one request spillway passed through without recognising its
// path, and warns if this is the first inference-shaped request on a
// template spillway has never been told about.
//
// Only r.Method, r.URL.Path and r.ContentLength are read. The body is never
// touched — these requests stream straight through, and buffering one to
// look at it would cost the streaming behaviour the passthrough path exists
// to provide.
func (t *unpooledTracker) note(r *http.Request) {
	template := redactPath(r.URL.Path)
	// The suppression is on the WARNING, not on the count. The count
	// answers "how much traffic is spillway declining to claim", which is
	// true of a measured family as much as an unknown one; the warning
	// answers "is something NEW happening", which a measured family is by
	// definition not. Tested by the raw path, before redaction — the
	// measured families are named, not templated.
	shaped := inferenceShaped(r) && !isKnownNonInference(r.URL.Path)

	t.mu.Lock()
	t.requests++
	if _, tracked := t.templates[template]; tracked || len(t.templates) < maxUnpooledPaths {
		t.templates[template]++
	} else {
		t.overflow++
	}
	warn := false
	if shaped && !t.warned[template] && len(t.warned) < maxUnpooledPaths {
		t.warned[template] = true
		warn = true
	}
	t.mu.Unlock()

	if !warn || t.logger == nil {
		return
	}
	// WARN, not Info: this is the one line that stands between a new
	// inference endpoint and the pool silently not being used for it. It is
	// emitted at most once per template per daemon run.
	t.logger.Warn("unrecognised path passed through unpooled",
		"path", template,
		"method", r.Method,
		"detail", "POST with a body — the shape of inference — on a path spillway does not pool",
		"action", "if this is an inference endpoint, add it to proxy.inferencePaths in the config; it applies without a restart")
}

// inferenceShaped reports whether a request looks like a completion call:
// a POST carrying a body.
//
// An unknown length (chunked, ContentLength -1) counts as shaped. The point
// of this predicate is not to be right about every request — it is to make
// sure a new inference endpoint cannot slip past unnoticed, and "I cannot
// tell how big this is" must not be the reason it does.
func inferenceShaped(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	return r.ContentLength < 0 || r.ContentLength >= unpooledBodyHint
}

// stats snapshots the tracker.
func (t *unpooledTracker) stats() UnpooledStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := UnpooledStats{Requests: t.requests, Paths: len(t.templates)}
	for p := range t.warned {
		s.InferenceShaped = append(s.InferenceShaped, p)
	}
	sort.Strings(s.InferenceShaped)
	return s
}

// UnpooledStats reports traffic spillway passed through without a pooled
// credential because it did not recognise the path (issue #176). Wired into
// /api/state, and from there into `spillway status`.
func (h *Handler) UnpooledStats() UnpooledStats { return h.unpooled.stats() }

// redactPath turns a request path into a stable template: every segment
// that looks like an identifier becomes "{id}", and the tail beyond
// maxPathSegments is elided.
//
// Two jobs at once, and both are why the tracker keys on this rather than
// on the raw path. It keeps identifying material — session ids, artifact
// ids, environment ids — out of anything stored or logged (§5), and it
// bounds the warning at one line per endpoint SHAPE instead of one per
// item, which is the difference between a day-one signal and a firehose.
func redactPath(path string) string {
	if path == "" {
		return "/"
	}
	segs := strings.Split(path, "/")
	if len(segs) > maxPathSegments+1 {
		segs = append(segs[:maxPathSegments+1:maxPathSegments+1], "…")
	}
	for i, s := range segs {
		if opaqueSegment(s) {
			segs[i] = "{id}"
		}
	}
	return strings.Join(segs, "/")
}

// opaqueSegment reports whether a path segment looks like an identifier
// rather than a name.
//
// Conservative on purpose in the direction that matters: a real name
// wrongly redacted costs nothing but a slightly vaguer log line, whereas a
// uuid left in place is an identifier in the log and an unbounded number of
// warnings. The three rules cover what actually appears here — pure
// numbers, uuids and hex blobs, and long opaque tokens — and a segment
// containing anything outside [0-9A-Za-z_-] is treated as a name, since
// every id shape spillway sees is drawn from that alphabet.
func opaqueSegment(s string) bool {
	if len(s) < 2 {
		return false
	}
	digits, hexish, wordish := 0, 0, 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
			hexish++
			wordish++
		case (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'):
			hexish++
			wordish++
		case (r >= 'g' && r <= 'z') || (r >= 'G' && r <= 'Z'):
			wordish++
		case r == '-' || r == '_':
			hexish++
			wordish++
		default:
			return false
		}
	}
	n := len(s)
	switch {
	case digits == n:
		return true // a bare number
	case n >= 16 && hexish == n && digits > 0:
		return true // uuid or hex blob
	case n >= 20 && wordish == n && digits > 0:
		return true // long opaque token
	}
	return false
}
