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
	"strconv"
	"strings"
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
	// ca, when non-nil, enables CONNECT termination for allowedHosts.
	ca           caIssuer
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
}

// Hooks are optional observability sinks wired by the daemon.
type Hooks struct {
	Log    *reqlog.Log
	Events *events.Broker
}

// SetHooks wires observability sinks. nil-safe to leave unset (tests).
func (h *Handler) SetHooks(hooks Hooks) { h.hooks = hooks }

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
		},
		allowedHosts:  map[string]bool{},
		exhaustedMode: cfg.Pool.ExhaustedMode,
		holdMax:       cfg.PoolHoldMax(),
	}
	h.SetMITM(nil)
	return h, nil
}

// SetMITM enables CONNECT termination: ca mints leaves for the hosts of the
// configured upstreams (global + per-account). nil disables termination —
// every CONNECT blind-tunnels.
func (h *Handler) SetMITM(ca caIssuer) {
	h.ca = ca
	h.allowedHosts = map[string]bool{}
	if u, err := url.Parse(h.upstream); err == nil {
		h.allowedHosts[u.Hostname()] = true
	}
	for _, a := range h.pool.Accounts() {
		if u, err := url.Parse(a.Upstream); err == nil && u.Hostname() != "" {
			h.allowedHosts[u.Hostname()] = true
		}
	}
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
	)
	if h.hooks.Log != nil {
		// Metadata only — never headers or bodies (§5 redaction).
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
	event := reqlog.EventServed
	// Computed once from the buffered body: mapping is a pure lookup, so this
	// needs no coordination with buildRequest.
	modelAsked := ""
	if buffered {
		modelAsked = modelOf(body)
	}
	modelServed := modelAsked
	modelFor := func(a *pool.Account) string {
		if a != nil && a.ModelMap != nil {
			if m, ok := a.ModelMap[modelAsked]; ok {
				return m
			}
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

		is401 := resp.StatusCode == http.StatusUnauthorized
		// Which statuses are worth reading a body for is the provider's call:
		// Kimi overloads 401 and 403, Anthropic does not (§6.20).
		classifiable := buffered && provider.For(acct.Type).Classifiable(resp.StatusCode)
		if !classifiable {
			// Terminal: stream through. Once the first byte is written no
			// failover is possible (§6.1) — mid-stream aborts are the client's
			// own retry behaviour.
			modelServed = modelFor(acct)
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
			until := quotaReset(acct, resp.Header)
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
			h.pool.MarkExhausted(acct, until)
			h.pool.Done(acct)
			tried[name] = true
			acct = nil
			rotatedQuota = true
			h.logger.Info("quota exhausted, rotating",
				"account", name, "until", until.UTC().Format(time.RFC3339))
			h.publish(events.Event{Type: reqlog.EventRotatedQuota, Account: name,
				Detail: "quota exhausted until " + until.UTC().Format(time.RFC3339)})
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
		if acct.ModelMap != nil {
			// §4 allowed mutation #3 (cross-provider only): map the model
			// id. Unmapped → hard error, never forward a claude id to Kimi.
			var err error
			body, err = rewriteModel(body, acct.ModelMap)
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

// isQuotaRejection distinguishes a durable quota-429 (rotate accounts) from a
// transient rate-limit-429 (retry same account) using Anthropic's unified
// rate-limit status headers. Anthropic
// accounts only; kimi classification is body-based (see classifyError).
func isQuotaRejection(h http.Header) bool {
	return h.Get("anthropic-ratelimit-unified-5h-status") == "rejected" ||
		h.Get("anthropic-ratelimit-unified-7d-status") == "rejected"
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

// quotaReset bounds how long an exhausted account sits out. The provider
// decides how: response headers, its own polled quota state, or a default.
func quotaReset(acct *pool.Account, h http.Header) time.Time {
	now := time.Now()
	fallback := time.Time{}
	if reset, ok := acct.EarliestQuotaReset(); ok {
		fallback = reset
	}
	return provider.For(acct.Type).ResetHint(h, now, fallback)
}

// resetHint derives when an exhausted account becomes eligible again: the
// latest unified reset header (unix seconds), else retry-after, else 1h.
func resetHint(h http.Header, now time.Time) time.Time {
	var reset time.Time
	for _, k := range []string{
		"anthropic-ratelimit-unified-5h-reset",
		"anthropic-ratelimit-unified-7d-reset",
	} {
		if v := h.Get(k); v != "" {
			if sec, err := strconv.ParseFloat(v, 64); err == nil {
				if t := time.Unix(int64(sec), 0); t.After(reset) {
					reset = t
				}
			}
		}
	}
	if !reset.IsZero() && reset.After(now) {
		return reset
	}
	if ra := retryAfterSeconds(h); ra > 0 {
		return now.Add(time.Duration(ra) * time.Second)
	}
	return now.Add(time.Hour)
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
