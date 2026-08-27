// Package proxy implements spillway's streaming reverse proxy and its
// account-pool request path: sticky selection, credential injection, and
// pre-first-byte failover (design doc §3, §4, §6.1, §6.6, §6.10).
package proxy

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/egress"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/notify"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

const (
	// defaultBodyCap bounds buffered retry when the config says nothing:
	// only POST /v1/messages bodies at or under this size are buffered for
	// failover; larger bodies stream straight through with no retry (§6.10).
	defaultBodyCap = 8 << 20
	// maxRateLimitTries caps inline same-account retries on a
	// rate-limit-429 (quota-429 rotates instead).
	maxRateLimitTries = 3
	// maxInlineBackoff bounds how long we hold the client connection
	// waiting out a rate-limit-429 before passing it through.
	maxInlineBackoff = 60 * time.Second
	// max429Body caps how much of a 429 body we read for classification
	// and pass-through.
	max429Body = 1 << 20
)

var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Handler proxies every request to a pool-selected account's upstream.
type Handler struct {
	pool     *pool.Pool
	logger   *slog.Logger
	upstream string // global default; accounts may override
	// Transport dials upstreams. Exported so tests can pin RootCAs; treat
	// as read-only after construction.
	Transport *http.Transport
	// ca, when non-nil, enables CONNECT termination for allowedHosts. Set
	// once before serving starts (NewHandler, then main.go's post-EnsureCA
	// SetMITM call) and never reassigned after — safe to read unlocked, same
	// as before issue #87.
	ca caIssuer
	// allowedHosts is guarded by hostsMu (issue #87): RefreshAllowedHosts
	// can now recompute it — after Pool.Add, so a newly added account is
	// covered without a restart when its host was already pre-minted — WHILE
	// connect.go reads it on every CONNECT. Before #87 nothing mutated this
	// after SetMITM's single startup call, so the unguarded read in
	// connect.go was safe by construction; a runtime mutator makes it a data
	// race on the request path without a lock.
	hostsMu      sync.RWMutex
	allowedHosts map[string]bool
	// Pool-exhaustion behaviour (§6.11).
	exhaustedMode string
	holdMax       time.Duration
	// notifier raises a desktop notification when the pool runs dry in
	// notify mode (§6.11).
	notifier *notify.Notifier
	// bodyCap bounds the buffered-retry body (§6.10, configurable).
	bodyCap int64
	// egress reaches upstreams directly or via a corporate proxy (§6.13).
	egress *egress.Egress
	// hooks are optional observability sinks (request log, event broker).
	hooks Hooks
	// claims observes the Representative-Claim header against #24's static
	// guess (issue #53). Never touches selection/rotation/eligibility.
	claims *representativeClaimObserver
	// mitmFails rate-limits the "mitm connection failed" warning (issue
	// #64): a client stuck retrying a MITM'd host forever must not be able
	// to flood the log with one line per attempt.
	mitmFails *mitmFailLogger
}

// Hooks are optional observability sinks wired by the daemon.
type Hooks struct {
	Log    *reqlog.Log
	Events *events.Broker
}

// SetHooks wires observability sinks. nil-safe to leave unset (tests).
func (h *Handler) SetHooks(hooks Hooks) { h.hooks = hooks }

// NotifyCARegenerated tells this Handler that mitm.EnsureCA just replaced
// the MITM CA in place — mitm.CA.Regenerated was true, never on an ordinary
// restart with an unchanged host set (#70) or a first-ever install. Call
// this exactly once, right after SetMITM, only in that case.
//
// It does two things (issue #66): arms the stale-CA detector, so a later
// recurring MITM handshake failure on h.mitmFails can be treated as
// evidence a client is stuck trusting the old CA rather than ordinary churn
// (see mitmfail.go); and raises the one desktop notification the issue asks
// for at the moment of replacement, since whoever runs the config change
// that triggered this may not be looking at any statusline right then.
func (h *Handler) NotifyCARegenerated() {
	h.mitmFails.activate()
	if h.notifier != nil {
		h.notifier.Notify("mitm-ca-regenerated", "spillway: MITM certificate replaced",
			"Any other Claude Code session already running through spillway will fail every "+
				"request from now on — restart it.")
	}
}

// StaleCAWarning reports whether at least one client currently looks stuck
// trusting a CA this run replaced (issue #66) — see mitmfail.go's
// stranded-client detector. Always false unless NotifyCARegenerated was
// called this run. Read by admin's /api/state so the statusline can warn
// while it is true, and it decays back to false on its own once the
// failures stop.
func (h *Handler) StaleCAWarning() bool {
	return h.mitmFails.Stranded()
}

// publish fires a broker event when one is wired.
func (h *Handler) publish(e events.Event) {
	if h.hooks.Events != nil {
		h.hooks.Events.Publish(e)
	}
}

// caIssuer mints per-host leaf certificates (mitm.CA; interface for tests).
type caIssuer interface {
	Leaf(host string) (*tls.Certificate, error)
}

// NewHandler builds the pool-backed reverse-proxy handler. Responses stream
// chunk-by-chunk so SSE and other long-lived LLM streams are never buffered;
// retry/failover happens only before the first response byte reaches the
// client (§6.1). CONNECT requests are blind-tunneled unless SetMITM allows
// their host.
func NewHandler(cfg *config.Config, logger *slog.Logger, p *pool.Pool) (*Handler, error) {
	eg, err := egress.New(cfg.Egress.Mode, cfg.Egress.Proxy)
	if err != nil {
		return nil, err
	}
	cap := int64(cfg.Pool.MaxBufferBytes)
	if cap <= 0 {
		cap = defaultBodyCap
	}
	h := &Handler{
		notifier: notify.New(),
		bodyCap:  cap,
		egress:   eg,
		pool:     p,
		logger:   logger,
		upstream: cfg.Upstream,
		Transport: &http.Transport{
			// Both fields, deliberately: Proxy covers ordinary requests,
			// DialContext covers the CONNECT tunnels the MITM path opens.
			Proxy:               eg.ProxyFunc(),
			DialContext:         eg.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			// First response byte can legitimately take minutes on a
			// thinking-heavy LLM request; no timeout on the body after that.
			ResponseHeaderTimeout: 5 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
			// Explicitly false (also the zero value, but spelled out
			// because it matters): upstream traffic is long-lived SSE
			// completions, many at once when subagents fan out. Multiplexed
			// onto one h2 connection, a handful of live streams can exhaust
			// the stream budget and a trivial request waits behind them —
			// a stall that looks like the proxy hanging. One TCP connection
			// per concurrent request is deliberate; see issue #27 and
			// TestUpstreamSpeaksHTTP1.
			ForceAttemptHTTP2: false,
		},
		allowedHosts:  map[string]bool{},
		exhaustedMode: cfg.Pool.ExhaustedMode,
		holdMax:       cfg.PoolHoldMax(),
		claims:        newRepresentativeClaimObserver(),
		mitmFails:     newMitmFailLogger(logger),
	}
	h.SetMITM(nil)
	return h, nil
}

// SetMITM enables CONNECT termination: ca mints leaves for the hosts of the
// configured upstreams (global + per-account + every registered provider's
// DefaultUpstream — issue #87). nil disables termination — every CONNECT
// blind-tunnels. Called exactly twice in production, both before serving
// starts: once from NewHandler with nil (to compute the host set
// mitm.EnsureCA needs), once from main.go with the real CA it returned.
func (h *Handler) SetMITM(ca caIssuer) {
	h.ca = ca
	h.setAllowedHosts(h.wantedHosts(ca))
}

// RefreshAllowedHosts recomputes which hosts CONNECT termination covers,
// picking up a pool.Add'd account (issue #87) — normally a complete no-op,
// since every provider's DefaultUpstream is pre-minted at startup regardless
// of which accounts exist that day, so an ordinary account's host was
// already allowed before it existed. It only ever matters for a custom
// upstream host with no pre-minted leaf, and even then wantedHosts filters
// it right back out (see that method's doc) rather than adding a host the
// CA cannot serve — allowing CONNECT termination for a host with no leaf
// would turn a working blind tunnel into a hard "leaf mint failed" failure.
// Safe to call concurrently with in-flight CONNECTs; see hostsMu.
func (h *Handler) RefreshAllowedHosts() {
	h.setAllowedHosts(h.wantedHosts(h.ca))
}

// wantedHosts computes the candidate host set — the global upstream, every
// registered provider's DefaultUpstream (issue #87), and every current pool
// account's upstream override — then, whenever ca is non-nil, filters it
// down to hosts ca actually has a precomputed leaf for. The filter is a
// no-op on the one path that mints a CA (main.go: AllowedHosts() computed
// with ca=nil feeds mitm.EnsureCA, which mints exactly that set, before
// SetMITM(ca) filters against it) — it only ever removes something on a
// later RefreshAllowedHosts call, for a custom upstream host EnsureCA was
// never told about. That host must not become CONNECT-terminated with no
// leaf to serve; see #87's "still restart-only" case.
func (h *Handler) wantedHosts(ca caIssuer) map[string]bool {
	candidates := map[string]bool{}
	if u, err := url.Parse(h.upstream); err == nil && u.Hostname() != "" {
		candidates[u.Hostname()] = true
	}
	for _, host := range provider.DefaultUpstreamHosts() {
		candidates[host] = true
	}
	for _, a := range h.pool.Accounts() {
		if u, err := url.Parse(a.Upstream); err == nil && u.Hostname() != "" {
			candidates[u.Hostname()] = true
		}
	}
	if ca == nil {
		return candidates
	}
	out := make(map[string]bool, len(candidates))
	for host := range candidates {
		if _, err := ca.Leaf(host); err == nil {
			out[host] = true
		}
	}
	return out
}

// setAllowedHosts swaps in a freshly computed host set under hostsMu.
func (h *Handler) setAllowedHosts(hosts map[string]bool) {
	h.hostsMu.Lock()
	h.allowedHosts = hosts
	h.hostsMu.Unlock()
}

// hostAllowed reports whether host is currently CONNECT-terminated. Read on
// every CONNECT (connect.go) — see hostsMu for why this locks.
func (h *Handler) hostAllowed(host string) bool {
	h.hostsMu.RLock()
	defer h.hostsMu.RUnlock()
	return h.allowedHosts[host]
}

// MITMCovers reports whether host already has a pre-minted MITM leaf —
// the test a live account-add (issue #87) must pass before it can promise
// CONNECT-mode termination for that host without a restart. Always false
// while MITM is disabled (h.ca nil).
func (h *Handler) MITMCovers(host string) bool {
	if h.ca == nil {
		return false
	}
	_, err := h.ca.Leaf(host)
	return err == nil
}

// AllowedHosts returns the hostnames CONNECT termination covers right now
// (the global upstream, every provider's default upstream, and every
// account's upstream override), sorted. Exported so the caller can size the
// CA's precomputed leaf set correctly (issue #69): NewHandler already
// computes this via SetMITM(nil), so calling this right after NewHandler and
// before mitm.EnsureCA hands the full, real host set to EnsureCA before it
// mints anything.
func (h *Handler) AllowedHosts() []string {
	h.hostsMu.RLock()
	defer h.hostsMu.RUnlock()
	hosts := make([]string, 0, len(h.allowedHosts))
	for host := range h.allowedHosts {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.serve(w, r) }

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		h.handleConnect(w, r)
		return
	}
	start := time.Now()
	lw := &loggingWriter{ResponseWriter: w, status: http.StatusOK}
	oc := h.route(lw, r)
	dur := time.Since(start)
	h.logger.Info("request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", lw.status,
		"duration", dur.Round(time.Millisecond),
		"bytes", lw.bytes,
		"account", oc.account,
		"event", oc.event,
		"user_agent", r.Header.Get("User-Agent"),
	)
	if h.hooks.Log != nil {
		// Metadata only — never headers or bodies (§5 redaction), except
		// User-Agent: a single identifying header, kept so a request that
		// didn't come from the CLI (issue #64: an MCP server's own call,
		// once a combined CA bundle lets it verify a MITM'd host) can be
		// told apart after the fact. A hint only — most non-CLI clients
		// don't send a distinctive one either.
		_ = h.hooks.Log.Record(reqlog.Entry{
			Account:    oc.account,
			Path:       r.URL.Path,
			Status:     lw.status,
			DurationMs: dur.Milliseconds(),
			Bytes:      lw.bytes,
			Event:      oc.event,
			// What the client asked for vs what actually went upstream.
			ModelAsked:  oc.modelAsked,
			ModelServed: oc.modelServed,
			UserAgent:   r.Header.Get("User-Agent"),
		})
	}
}

// outcome is what route settled on for one request.
type outcome struct {
	account string
	event   string
	// What the client asked for versus what actually went upstream. They
	// differ under modelMap, and that difference is invisible anywhere else
	// (doc §6.18). Reported only — never routed on.
	modelAsked  string
	modelServed string
}

// route runs the request through selection, injection and failover, writing
// exactly one upstream response (or a synthesized error) to w. Returns the
// outcome for logging.
func (h *Handler) route(w http.ResponseWriter, r *http.Request) outcome {
	// Upgrade requests (Remote Control's realtime channel) relay raw — the
	// pool path can't speak WebSocket.
	if isUpgrade(r) {
		h.relayUpgrade(w, r)
		return outcome{account: "(upgrade)", event: reqlog.EventPassthrough}
	}
	// Identity-bound paths relay with the client's own credential — no
	// injection, no pool, no rewrite. This keeps Remote Control and the
	// CLI's own token refresh working through us (§3 request path step 1).
	if isIdentityPath(r.URL.Path) {
		h.passThrough(w, r)
		return outcome{account: "(passthrough)", event: reqlog.EventPassthrough}
	}

	// Buffer only POST /v1/messages bodies within the cap (§6.10).
	var body []byte
	buffered := false
	if r.Method == http.MethodPost && r.URL.Path == "/v1/messages" && r.Body != nil {
		b, overflow, err := readCapped(r.Body, h.bodyCap)
		if err != nil {
			r.Body.Close()
			http.Error(w, "spillway: read request body", http.StatusBadRequest)
			return outcome{event: reqlog.EventServed}
		}
		if overflow {
			// Over-cap: stitch the read prefix back onto the tail and stream
			// straight through with no retry.
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), r.Body))
		} else {
			r.Body.Close()
			body = b
			buffered = true
		}
	}

	session := sessionKey(r, body, buffered)

	tried := map[string]bool{}
	recovered := map[string]bool{}
	var acct *pool.Account
	var holdDeadline time.Time // set on first exhaustion
	rotatedQuota, rotatedConn, rateRetried, held := false, false, false, false
	onOverage := false
	// fiveXXHopUsed bounds the upstream-5xx rotation below to a single hop,
	// unlike rotatedQuota/rotatedConn which chain through every untried
	// account. See the rotation site for why.
	fiveXXHopUsed := false
	event := reqlog.EventServed
	// Computed once from the buffered body: mapping is a pure lookup, so this
	// needs no coordination with buildRequest.
	modelAsked := ""
	if buffered {
		modelAsked = modelOf(body)
	}
	modelServed := modelAsked
	modelFor := func(a *pool.Account) string {
		if a == nil {
			return modelAsked
		}
		// The same resolution the rewrite uses, globs included — reporting
		// the asked-for id while sending a mapped one is how a request log
		// comes to disagree with what actually happened.
		if m, ok := lookupModel(a.EffectiveModelMap(), modelAsked); ok {
			return m
		}
		return modelAsked
	}
	finish := func(account string) outcome {
		switch {
		// Overage outranks the rotation reasons: how the request got here
		// matters less than the fact that it cost money.
		case onOverage:
			event = reqlog.EventOverage
		case rotatedQuota:
			event = reqlog.EventRotatedQuota
		case rotatedConn:
			event = reqlog.EventRotatedConn
		case rateRetried:
			event = reqlog.EventRateRetry
		case held:
			event = reqlog.EventHeld
		}
		return outcome{account: account, event: event,
			modelAsked: modelAsked, modelServed: modelServed}
	}
	rateTries := 0
	for {
		// Reset every iteration, before an account is even chosen: the only
		// thing that should ever make modelServed disagree with modelAsked is
		// THIS iteration's account actually getting a response back. Without
		// this, a value set for an account that then rotated away (quota
		// reject, failed recovery, dead connection) would leak into whatever
		// this request ultimately logs as, if that next step never itself
		// reaches an upstream response (pool exhaustion, cancellation, a
		// buildRequest failure on the next account).
		modelServed = modelAsked
		if acct == nil {
			acct = h.pool.SelectExcept(session, body, tried)
			if acct == nil {
				// §6.11: park until the soonest reset rather than failing,
				// bounded by holdMax per request.
				if holdDeadline.IsZero() {
					holdDeadline = time.Now().Add(h.holdMax)
				}
				if h.waitForReset(r, holdDeadline) {
					// The hold waited out a reset, so the accounts that
					// failed before this point may now succeed. `tried`
					// means "failed in this round" — the wait starts a new
					// one, and not clearing it leaves the pool permanently
					// empty after the very first rotation.
					clear(tried)
					held = true
					h.publish(events.Event{Type: reqlog.EventHeld, Detail: "pool exhausted, holding until reset"})
					continue
				}
				if r.Context().Err() != nil {
					return finish("(cancelled)") // client gone — nothing to write to
				}
				h.logger.Warn("pool exhausted", "path", r.URL.Path)
				// Distinguish "nothing left" from "nothing that can serve
				// THIS": a capability mismatch is not a rate limit, and
				// reporting it as one sends the client into a pointless
				// backoff (§6.19).
				if why := h.pool.WhyUnavailable(body); why != nil {
					h.logger.Warn("no account can serve this request", "err", why)
					h.publish(events.Event{Type: reqlog.EventExhausted, Detail: why.Error()})
					writeJSON(w, http.StatusBadRequest,
						`{"type":"error","error":{"type":"invalid_request_error","message":`+
							jsonString(why.Error())+`}}`)
					return outcome{account: "(incompatible)", event: reqlog.EventExhausted,
						modelAsked: modelAsked, modelServed: modelServed}
				}
				h.publish(events.Event{Type: reqlog.EventExhausted, Detail: "all accounts exhausted"})
				writeJSON(w, http.StatusTooManyRequests,
					`{"type":"error","error":{"type":"rate_limit_error","message":"spillway: all accounts exhausted"}}`)
				return outcome{account: "(none available)", event: reqlog.EventExhausted,
					modelAsked: modelAsked, modelServed: modelServed}
			}
			// Ask the pool the same question it asked when selecting. The
			// earlier version tested StateExhausted, which only catches an
			// account that has actually been 429'd — and misses the ordinary
			// case of one whose quota headers say it is finished while
			// spillway rotates away from it. Those requests bill and would
			// have been logged as ordinary served traffic.
			if h.pool.WouldBill(acct) {
				onOverage = true
				h.logger.Warn("serving past quota on extra usage — this request is billed",
					"account", acct.Name, "path", r.URL.Path)
				h.publish(events.Event{Type: reqlog.EventOverage, Account: acct.Name,
					Detail: "quota exhausted — serving on paid extra usage"})
				if h.notifier != nil {
					h.notifier.Notify("overage-"+acct.Name, "spillway: paying for extra usage",
						acct.Name+" is out of quota and is now billing for extra usage")
				}
			}
			// Proactive refresh near expiry (§6.4). A dead refresh token
			// disables the account — rotate away from it.
			if err := h.pool.EnsureFresh(r.Context(), acct); err != nil {
				h.logger.Warn("credential refresh failed", "account", acct.Name, "err", err)
			}
			if acct.State() == pool.StateDisabled {
				h.pool.Done(acct)
				tried[acct.Name] = true
				acct = nil
				continue
			}
		}
		name := acct.Name

		upstream := acct.Upstream
		if upstream == "" {
			upstream = h.upstream
		}
		out, err := h.buildRequest(r, upstream, acct, body, buffered)
		if err != nil {
			h.pool.Done(acct)
			var unmapped *errUnmappedModel
			if errors.As(err, &unmapped) {
				// §6.12: never pass an unmapped model id to another provider.
				writeJSON(w, http.StatusBadGateway,
					`{"type":"error","error":{"type":"proxy_error","message":`+strconv.Quote(unmapped.Error())+`}}`)
				return finish(name)
			}
			http.Error(w, "spillway: build upstream request", http.StatusBadGateway)
			return finish(name)
		}

		resp, err := h.Transport.RoundTrip(out)
		if err != nil {
			// No response headers received: an unambiguous pre-work signal,
			// safe to retry on another account when the body is buffered (§6.6).
			h.pool.Done(acct)
			tried[name] = true
			acct = nil
			if buffered && len(tried) < len(h.pool.Accounts()) {
				rotatedConn = true
				h.logger.Warn("upstream connection failed, rotating", "account", name, "err", err)
				h.publish(events.Event{Type: reqlog.EventRotatedConn, Account: name, Detail: "upstream connection failed"})
				continue
			}
			h.logger.Error("upstream error", "method", r.Method, "path", r.URL.Path, "err", err)
			http.Error(w, "spillway: upstream error", http.StatusBadGateway)
			return finish(name)
		}

		// A response came back from THIS account: whatever we do with it —
		// stream it straight through or classify it into an error we hand
		// back — it is what actually went upstream for this request, mapped
		// model included. Set it here, once, common to every branch below
		// (classifiable or not), rather than in each branch that eventually
		// writes a response: a new early return added below inherits the
		// correct value instead of needing to remember to set it.
		modelServed = modelFor(acct)

		h.pool.RecordQuota(acct, resp.Header, time.Now())
		// The provider's own word, after the fact, that this response was
		// billed. It outranks the prediction made at selection: the first
		// live test served a billed request that spillway had judged free,
		// because the status header carried a value the parser did not know.
		// A prediction can be wrong; this cannot.
		//
		// Here, beside RecordQuota, because it must see EVERY response — the
		// first version sat in the error branch, where a successful billed
		// request never reached it, which is every billed request that works.
		if o := acct.Overage(); o.InUse && !onOverage {
			onOverage = true
			h.logger.Warn("served on extra usage — this request is billed",
				"account", name, "utilization", o.Utilization)
			h.publish(events.Event{Type: reqlog.EventOverage, Account: name,
				Detail: "served on paid extra usage"})
		}
		if q, at := acct.Quota(); len(q) > 0 {
			h.logger.Debug("quota signal", "account", name, "headers", q, "at", at)
		}
		// Issue #53: observe whether Anthropic's own Representative-Claim
		// header agrees with #24's static model-name guess. Purely
		// observational — it reads modelServed (what actually went
		// upstream, mapped model included) and never feeds back into
		// selection, rotation, or eligibility.
		h.claims.check(h.logger, provider.For(acct.Type), modelServed, resp.Header)

		// Upstream 5xx (issue #26): 529 Overloaded is the clear case, but this
		// treats the whole 5xx range alike. Every provider's
		// ClassifiableStatuses stays under 500 — nothing here ever means
		// "this account's credentials/quota are the problem" the way a
		// 401/403/429 can, so there is no per-status judgment call to make:
		// by definition (RFC 7231 §6.6) a 5xx is the server, or a gateway in
		// front of it, failing to fulfil an otherwise-valid request. Another
		// account, possibly hitting different upstream capacity, may simply
		// answer.
		//
		// Bounded to ONE hop, unlike rotatedQuota/rotatedConn which chain
		// through every untried account: those signals mean THIS account is
		// done, so trying the rest of the pool is straightforward progress.
		// A 5xx carries no such guarantee — if the upstream is down for
		// everyone, chaining through N accounts just serialises N failures
		// (each up to ResponseHeaderTimeout) before the client sees the exact
		// same 5xx it would have gotten immediately. One extra hop catches
		// the case this issue is actually about (a sibling account, on
		// different capacity, answers); a second 5xx is treated as the
		// outage it probably is.
		//
		// Not the account's fault, so no MarkExhausted (that would penalize
		// it for an upstream problem) — but it also must not be retried
		// forever on the same account, so it still joins `tried` like a dead
		// connection does. And only ever a hop, never a synthesized response:
		// when no untried account remains, this falls through to the
		// terminal branch below and the client gets the real 5xx, not a
		// fabricated exhaustion error that would hide an upstream outage.
		if buffered && !fiveXXHopUsed && resp.StatusCode >= 500 &&
			len(tried)+1 < len(h.pool.Accounts()) {
			resp.Body.Close()
			h.pool.Done(acct)
			tried[name] = true
			acct = nil
			fiveXXHopUsed = true
			rotatedConn = true
			h.logger.Warn("upstream 5xx, rotating once", "account", name, "status", resp.StatusCode)
			h.publish(events.Event{Type: reqlog.EventRotatedConn, Account: name,
				Detail: "upstream " + strconv.Itoa(resp.StatusCode) + ", rotating"})
			continue
		}

		is401 := resp.StatusCode == http.StatusUnauthorized
		// Which statuses are worth reading a body for is the provider's call:
		// Kimi overloads 401 and 403, Anthropic does not (§6.20).
		classifiable := buffered && provider.For(acct.Type).Classifiable(resp.StatusCode)
		if !classifiable {
			// Terminal: stream through. Once the first byte is written no
			// failover is possible (§6.1) — mid-stream aborts are the client's
			// own retry behaviour.
			//
			// SetLastModel belongs only here, not wherever modelServed is set:
			// this is the one branch where the account's response — success or
			// an error status the provider doesn't classify — is what the
			// client actually receives. Every classifiable-error branch below
			// hands the client an upstream error instead and rotates or gives
			// up; recording "last model served" there would let an account
			// that just failed (or one we're about to rotate away from) claim
			// credit for a response it didn't serve.
			acct.SetLastModel(modelServed)
			defer h.pool.Done(acct)
			writeResponse(w, resp)
			return finish(name)
		}

		// Classifiable error on a buffered request: classify before touching
		// the client.
		errBody, _, _ := readCapped(resp.Body, max429Body)
		resp.Body.Close()

		if is401 {
			// Kimi overloads 401 for request-shape errors (verified live
			// 2026-08-21: "Your model id does not exist ... set model id as
			// `k3`"). That is NOT credential death — pass it through
			// untouched, never Recover, never disable a healthy account.
			if classifyError(acct, resp, errBody) == errModelID {
				h.pool.Done(acct)
				writeCaptured(w, resp, errBody)
				return finish(name)
			}
			// One credential recovery attempt, then one retry with the fresh
			// token (§6.6: 401 pre-first-byte is an unambiguous signal). A
			// second 401 passes through untouched.
			if !recovered[name] {
				recovered[name] = true
				if err := h.pool.Recover(r.Context(), acct); err == nil {
					h.logger.Info("credential recovered, retrying", "account", name)
					h.publish(events.Event{Type: "recovered", Account: name, Detail: "credential recovered after 401"})
					continue
				}
				if acct.State() == pool.StateDisabled {
					h.pool.Done(acct)
					tried[name] = true
					acct = nil
					continue
				}
			}
			h.pool.Done(acct)
			writeCaptured(w, resp, errBody)
			return finish(name)
		}

		if classifyError(acct, resp, errBody) == errQuota {
			// A quota rejection while serving on extra usage is a different
			// event: the subscription was already gone, so what just ran out
			// is the credit. RecordQuota above has taken the provider's word
			// for it — the account stops being billable the moment the
			// header says so — but the user needs telling, because this is
			// the ceiling they set and it will not lift at the quota reset.
			if onOverage {
				reason := acct.Overage().Reason
				if reason == "" {
					reason = "no reason given"
				}
				h.logger.Warn("extra usage refused — the credit limit is reached",
					"account", name, "reason", reason)
				h.publish(events.Event{Type: reqlog.EventOverage, Account: name,
					Detail: "extra usage refused: " + reason})
				if h.notifier != nil {
					h.notifier.Notify("overage-cap-"+name, "spillway: extra usage exhausted",
						name+" has hit its extra usage limit ("+reason+")")
				}
				onOverage = false
			}

			// Issue #54: WHICH window(s) fired decides how far this
			// rejection reaches. A provider with no per-window signal
			// (RejectedWindows nil, e.g. Kimi) has nothing to narrow by and
			// degrades to today's account-wide behaviour rather than
			// silently stopping protecting the account's quota.
			var rejected []string
			if spec := provider.For(acct.Type); spec.RejectedWindows != nil {
				rejected = spec.RejectedWindows(resp.Header)
			}
			accountWide, wideNames := accountWideRejection(acct.Type, rejected)

			var until time.Time
			var detail string
			if accountWide {
				until = quotaReset(acct, resp.Header, wideNames)
				h.pool.MarkExhausted(acct, until)
				detail = "quota exhausted until " + until.UTC().Format(time.RFC3339)
				// A mixed rejection (e.g. 5h AND 7d-fable together) still
				// gets its family-only member(s) recorded on their own
				// reset, in addition to the account-wide exhaustion above:
				// otherwise, once the account-wide window resets and
				// StateExhausted clears, a fable bucket that is separately
				// still rejected (its own reset can be much further out)
				// would silently read as healthy again.
				for _, winName := range rejected {
					if containsStr(wideNames, winName) {
						continue
					}
					h.pool.MarkWindowRejected(acct, winName, quotaReset(acct, resp.Header, []string{winName}))
				}
			} else {
				// Only a family-scoped window (e.g. "7d-fable") rejected:
				// the account itself is not done — Sonnet/Haiku, which
				// never draw on that bucket (issue #24's
				// GoverningWindows), must keep serving on it. Exclude
				// selection for the family this window governs instead of
				// touching account state at all (pool.Account.MarkWindowRejected,
				// checked by SelectExcept's per-request eligibility, not
				// the softer OverThresholdFor demotion).
				for _, winName := range rejected {
					wUntil := quotaReset(acct, resp.Header, []string{winName})
					h.pool.MarkWindowRejected(acct, winName, wUntil)
					if wUntil.After(until) {
						until = wUntil
					}
				}
				detail = "window(s) " + strings.Join(rejected, ",") + " rejected until " + until.UTC().Format(time.RFC3339)
			}

			h.pool.Done(acct)
			tried[name] = true
			acct = nil
			rotatedQuota = true
			h.logger.Info("quota rejected, rotating",
				"account", name, "accountWide", accountWide, "windows", rejected,
				"until", until.UTC().Format(time.RFC3339))
			h.publish(events.Event{Type: reqlog.EventRotatedQuota, Account: name, Detail: detail})
			continue
		}

		// A kimi 403 that isn't quota/billing is terminal — pass it through.
		if resp.StatusCode == http.StatusForbidden {
			h.pool.Done(acct)
			writeCaptured(w, resp, errBody)
			return finish(name)
		}

		// Rate-limit-429: transient throttle — retry the SAME account with
		// backoff, never rotate (rotating just throttles the next account and
		// discards this one's prompt cache).
		if rateTries < maxRateLimitTries {
			rateTries++
			rateRetried = true
			wait := backoff(resp.Header)
			h.logger.Info("rate limited, retrying same account",
				"account", name, "wait", wait, "try", rateTries)
			h.publish(events.Event{Type: reqlog.EventRateRetry, Account: name,
				Detail: "rate limited, retrying in " + wait.String()})
			select {
			case <-time.After(wait):
			case <-r.Context().Done():
				h.pool.Done(acct)
				return finish(name)
			}
			continue
		}

		// Out of inline retries: the client sees the upstream 429 as-is.
		h.pool.Done(acct)
		writeCaptured(w, resp, errBody)
		return finish(name)
	}
}

// buildRequest clones r for the given account's upstream: method, path,
// query and headers preserved (hop-by-hop stripped), the buffered body
// re-attached when present, and the only mutation applied — x-api-key
// dropped, Authorization set to the account token (§4 fingerprint invariant).
func (h *Handler) buildRequest(r *http.Request, upstream string, acct *pool.Account, body []byte, buffered bool) (*http.Request, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	out := r.Clone(r.Context())
	out.URL.Scheme = u.Scheme
	out.URL.Host = u.Host
	out.URL.Path = singleJoiningSlash(u.Path, r.URL.Path)
	out.Host = u.Host
	out.RequestURI = ""

	out.Header = r.Header.Clone()
	for _, k := range hopByHop {
		out.Header.Del(k)
	}
	// Strip headers named by the Connection header, per RFC 7230 §6.1.
	for _, f := range strings.Split(r.Header.Get("Connection"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			out.Header.Del(f)
		}
	}
	out.Header.Del("X-Api-Key")
	out.Header.Set("Authorization", "Bearer "+acct.Token())

	if buffered {
		if acct.AccountUUID != "" {
			// §4 allowed mutation #2: align the embedded account UUID with
			// the injected token. Byte-exact, same length; absent/malformed
			// metadata passes through untouched.
			body = patchAccountUUID(body, acct.AccountUUID)
		}
		if mm := acct.EffectiveModelMap(); len(mm) > 0 {
			// §4 allowed mutations #3 and #4 (cross-provider only): map the
			// top-level model id AND any advisor model nested in tools[]
			// (issue #29). Unmapped → hard error, never forward a claude id
			// to Kimi from either place.
			var err error
			body, err = rewriteModel(body, mm)
			if err != nil {
				return nil, err
			}
		}
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		out.Header.Set("Content-Length", strconv.Itoa(len(body)))
	} else {
		out.Body = r.Body
	}

	return out, nil
}

// writeResponse streams an upstream response to the client header-faithfully,
// flushing after every chunk.
func writeResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	fw := &flushWriter{w: w}
	_, _ = io.Copy(fw, resp.Body)
	fw.Flush()
}

// writeCaptured replays an already-read upstream response (429 classification
// consumed the body) to the client unchanged.
func writeCaptured(w http.ResponseWriter, resp *http.Response, body []byte) {
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func copyResponseHeaders(dst, src http.Header) {
	for _, k := range hopByHop {
		src.Del(k)
	}
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// jsonString quotes a value for embedding in a hand-built JSON error.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"spillway: request cannot be served"`
	}
	return string(b)
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// errKind is the classification of an upstream error response.
type errKind int

const (
	errNone errKind = iota
	// errQuota is durable quota exhaustion — rotate accounts.
	errQuota
	// errModelID is an unknown model id wearing an auth status code: the
	// credential is fine, so neither recover nor disable.
	errModelID
	// errRate is a transient throttle — retry the same account.
	errRate
)

// classifyError asks the provider what an upstream failure means. The
// per-provider rules live in internal/provider (§6.20) so a third provider is
// one registry entry, not a hunt through switch statements.
func classifyError(acct *pool.Account, resp *http.Response, body []byte) errKind {
	switch provider.For(acct.Type).Classify(resp.StatusCode, resp.Header, body) {
	case provider.ErrQuota:
		return errQuota
	case provider.ErrRate:
		return errRate
	case provider.ErrModelID:
		return errModelID
	default:
		return errNone
	}
}

// quotaReset bounds how long the named window(s) sit out. The provider
// decides how: response headers restricted to windows, its own polled
// quota state, or a default (issue #54: windows is the rejected set —
// account-wide for a whole-account exhaustion, a single family name for a
// window-scoped one — never every window this provider knows about).
func quotaReset(acct *pool.Account, h http.Header, windows []string) time.Time {
	now := time.Now()
	fallback := time.Time{}
	if reset, ok := acct.EarliestQuotaReset(); ok {
		fallback = reset
	}
	return provider.For(acct.Type).ResetHint(h, windows, now, fallback)
}

// accountWideRejection reports whether any of the rejected window names is
// account-wide — draws on every request regardless of model — and returns
// that account-wide subset (issue #54).
//
// A thin wrapper: the actual scope decision now lives in
// provider.ScopeRejection (issue #90), so the background quota re-probe
// (internal/accounts/probe.go) makes exactly the same call instead of a
// second, hand-rolled copy that could drift from this one. Kept here, under
// its original name, so this package's own tests
// (TestAccountWideRejectionScoping) keep exercising it as "proxy.go's own
// scope decision" rather than needing to know it moved.
func accountWideRejection(acctType string, rejected []string) (wide bool, wideNames []string) {
	return provider.ScopeRejection(acctType, rejected)
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// backoff is the wait before a same-account rate-limit retry: the upstream
// retry-after clamped to [1s, maxInlineBackoff], defaulting to 5s.
func backoff(h http.Header) time.Duration {
	ra := retryAfterSeconds(h)
	if ra < 1 {
		ra = 5
	}
	wait := time.Duration(ra) * time.Second
	if wait > maxInlineBackoff {
		wait = maxInlineBackoff
	}
	return wait
}

func retryAfterSeconds(h http.Header) int {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return n
}

// sessionKey identifies a logical session for sticky selection: a hash of
// metadata.user_id when the buffered body carries one, else the client IP.
//
// Claude Code does not send a bare user id there. It sends a JSON string:
//
//	{"device_id":"…","account_uuid":"…","session_id":"…"}
//
// Hashing it whole therefore yields one key per *session*, not per user or per
// machine — measured 2026-08-23, two `claude -p` invocations produced
// different keys. That is what makes concurrent sessions spread across
// accounts rather than piling onto one, and it is why the tie-break on
// in-flight count in better() is reachable at all.
//
// Two consequences worth keeping in mind. The account_uuid inside that blob is
// rewritten per account (§4, mutation #2) — but the rewrite happens after this
// runs, on the outgoing copy, so a session's key never shifts underneath it.
// And a session that somehow omits metadata falls back to the client IP, which
// on one machine means every such session shares a key.
func sessionKey(r *http.Request, body []byte, buffered bool) string {
	if buffered {
		var v struct {
			Metadata struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		if json.Unmarshal(body, &v) == nil && v.Metadata.UserID != "" {
			h := fnv.New32a()
			_, _ = h.Write([]byte(v.Metadata.UserID))
			return "uid:" + strconv.FormatUint(uint64(h.Sum32()), 16)
		}
	}
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return "ip:" + ip
	}
	return "ip:" + r.RemoteAddr
}

// readCapped reads up to cap+1 bytes, reporting overflow when the content
// exceeds cap.
func readCapped(r io.Reader, cap int64) (b []byte, overflow bool, err error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, cap+1))
	if err != nil {
		return nil, false, err
	}
	return buf.Bytes(), n > cap, nil
}

// flushWriter flushes after every write so SSE chunks reach the client
// incrementally.
type flushWriter struct{ w http.ResponseWriter }

func (f *flushWriter) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

func (f *flushWriter) Flush() {
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
}

// loggingWriter counts status and response bytes while preserving http.Flusher
// so streaming responses still flush chunk-by-chunk.
type loggingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *loggingWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *loggingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *loggingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer (hijack,
// read-from, etc.).
func (w *loggingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func singleJoiningSlash(a, b string) string {
	aslash := len(a) > 0 && a[len(a)-1] == '/'
	bslash := len(b) > 0 && b[0] == '/'
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}
