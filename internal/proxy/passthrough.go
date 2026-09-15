package proxy

// Identity-bound pass-throughs and the WebSocket relay (design doc §3
// request path step 1): these paths belong to the CLIENT's own login, so
// they relay with the client's credential verbatim — no injection, no pool,
// no uuid rewrite, no retry.

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coderage-labs/spillway/internal/reqlog"
)

// identityTrees are the identity-bound path TREES: each base matches both
// the collection itself and everything under it (see underPath). They are
// listed as bases without a trailing slash precisely so the two cannot drift
// apart again — issue #166.
var identityTrees = []string{
	"/v1/code",
	"/api/oauth/files",
	"/v1/environments",
	"/v1/sessions",
	// /api/frame is the artifact endpoint (issue #175): publish
	// (/api/frame/deploy/*), read-back (/api/frame/read/<id>) and fetch
	// (/api/frame/<id>). An artifact belongs to the account that published
	// it, so pool-routing it scattered 79 artifacts across 5 accounts —
	// none of them the account the user works from, so none of them visible
	// to the user. The WebSocket form was already identity-bound by
	// accident, via route()'s isUpgrade branch; the plain HTTP calls were
	// not.
	"/api/frame",
}

// underPath reports whether path is the base itself or something beneath it
// — base, or base + "/" + anything — and nothing else.
//
// The "or the base itself" half is the whole point (issue #166). A bare
// strings.HasPrefix(path, base+"/") silently excludes the collection
// endpoint, which is where the item is CREATED: /v1/sessions/<id> was
// passed through on the client's own credential while the POST /v1/sessions
// that made that <id> was pool-routed to whichever account the pool happened
// to pick, so the two halves of one session disagreed about whose it was.
//
// The "and nothing else" half is why this is not HasPrefix(path, base):
// that would swallow /v1/sessionsfoo, and any future base that happens to be
// a prefix of an unrelated path. Matching on the segment boundary keeps the
// widening of this rule from reaching paths it was never meant to cover.
func underPath(path, base string) bool {
	return path == base || strings.HasPrefix(path, base+"/")
}

// isIdentityPath reports whether a path is identity-bound: it must reach the
// upstream with the client's own Authorization/x-api-key untouched. This is
// what keeps Remote Control and the CLI's own token refresh working through
// the proxy. The list is EMPIRICAL — RC server-mode paths observed live
// 2026-08-21 (e.g. /v1/environments/<id>/work/poll, /v1/sessions/<id>/archive)
// — extend it as new identity paths surface. Inference paths
// (/v1/messages) must never be added: those are pool-routed, and no entry in
// identityTrees may be a prefix of one at a segment boundary.
//
// 2026-09-14 (issue #166): the tree bases used to be written as "X/"
// prefixes, so the collection endpoints — /v1/sessions above all — fell
// through to pool routing. Measured on live traffic, the exact path
// /v1/sessions was served by 8 different pooled accounts while every
// /v1/sessions/<id> call went out as a passthrough.
func isIdentityPath(path string) bool {
	switch path {
	case "/v1/oauth/token", "/api/oauth/file_upload":
		return true
	}
	for _, base := range identityTrees {
		if underPath(path, base) {
			return true
		}
	}
	return false
}

// isNonQuotaPath reports whether a path is CONFIRMED non-inference: it
// consumes no quota and needs no pooled account (issue #91).
//
// ITS JOB CHANGED WITH ISSUE #176 AND IT IS WORTH BEING PRECISE ABOUT WHAT
// IS LEFT. It used to be a routing decision: route() pooled by default, so
// naming a path here was what kept it out of pool selection and the hold
// path. Since the default inverted, the default ALREADY does that for
// every path spillway does not pool, and this branch of routeUnpooled
// reaches the same passThrough the fall-through reaches. So it no longer
// changes where a request goes.
//
// It is not dead, though, and it did not collapse into the same branch by
// accident. What it still decides is what spillway is CLAIMING about these
// three paths, which is a stronger statement than the default makes:
// "confirmed to consume no quota", recorded as "(non-quota)" in the request
// log, versus "spillway does not recognise this", recorded as "(unpooled)"
// and counted against the unrecognised-path warning. Folding the two
// together would lose a distinction this very issue is about, and would
// silently change what historical log rows mean. See knownNonInferenceTrees
// in unpooled.go for the separate, weaker list that only suppresses the
// warning.
//
// The list is EMPIRICAL and deliberately narrow, confirmed from real
// traffic 2026-08-22 (issue #91): a 51-request queue formed when the pool
// went dry because these three were held for up to 53 minutes waiting on
// quota they never needed, and were separately served against (and logged
// against) a real pooled account for no reason. Do NOT add a path here on
// a guess — wrongly bypassing a path that DOES need a pooled account's
// credential means that request fails or goes out unauthenticated, which
// is worse than a pointless wait. Paths seen in the same traffic but not
// confirmed one way or the other (e.g. /mcp-registry/v0/servers,
// /latest/api/token) are deliberately left OUT of this list and instead
// covered by route()'s narrower hold gate: only POST /v1/messages is
// allowed to hold on exhaustion, so an unclassified path here still fails
// fast instead of queueing, without having to guess whether it needs an
// account's credential to be forwarded correctly.
//
// Issue #175 asked whether /api/frame belongs here as well as in
// identityTrees. It does not, and adding it would be dead code that reads
// as policy. route() checks isIdentityPath FIRST and both branches call the
// same passThrough — so an identity path ALREADY never reaches pool
// selection or the hold path, which is the entire behaviour non-quota
// status buys. A second entry could therefore never fire, and the only
// thing it could change is the request log's account label, which would
// then say "(non-quota)" for something better described as the client's own
// artifact. The two lists answer different questions — "whose login is
// this?" and "does this need a pooled account at all?" — and an artifact
// publish has an unambiguous answer to the first.
//
// Audited for issue #166's off-by-one-slash: this list is exact-match only
// and deliberately stays that way. These are three specific leaf endpoints,
// not trees — /api/claude_code/<anything else> is unclassified, and the
// paragraph above is exactly why it must stay that way — so there is no
// collection/item pair here to get wrong, and underPath must not be used.
func isNonQuotaPath(path string) bool {
	switch path {
	case "/api/event_logging/v2/batch",
		"/api/claude_code/settings",
		"/api/claude_code/policy_limits":
		return true
	}
	return false
}

// isUpgrade reports whether the request is a protocol upgrade (WebSocket).
func isUpgrade(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") &&
		r.Header.Get("Upgrade") != ""
}

// passThrough relays to the default upstream with client headers verbatim
// (hop-by-hop stripped, Host rewritten — nothing else). The body streams; no
// buffering, no failover: these paths are identity-bound, so rotating them
// would be wrong, not just unsafe.
func (h *Handler) passThrough(w http.ResponseWriter, r *http.Request) {
	out, err := h.rawRequest(r)
	if err != nil {
		http.Error(w, "spillway: build upstream request", http.StatusBadGateway)
		return
	}
	resp, err := h.Transport.RoundTrip(out)
	if err != nil {
		h.logger.Error("passthrough upstream error", "path", r.URL.Path, "err", err)
		http.Error(w, "spillway: upstream error", http.StatusBadGateway)
		return
	}
	writeResponse(w, resp, h.logger)
}

// rawRequest clones r for the default upstream with no credential mutation.
func (h *Handler) rawRequest(r *http.Request) (*http.Request, error) {
	u, err := url.Parse(h.upstream)
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
	for _, f := range strings.Split(r.Header.Get("Connection"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			out.Header.Del(f)
		}
	}
	out.Body = r.Body
	return out, nil
}

// relayUpgrade proxies a WebSocket (or any Upgrade) handshake and then the
// raw bidirectional stream: hijack the client, dial the upstream with TLS
// verification, forward the request with client headers verbatim, copy both
// ways until either side closes.
func (h *Handler) relayUpgrade(w http.ResponseWriter, r *http.Request) {
	u, err := url.Parse(h.upstream)
	if err != nil {
		http.Error(w, "spillway: bad upstream", http.StatusBadGateway)
		return
	}

	upConn, err := h.dialUpgrade(u)
	if err != nil {
		h.logger.Error("upgrade dial failed", "path", r.URL.Path, "err", err)
		http.Error(w, "spillway: upgrade dial failed", http.StatusBadGateway)
		return
	}

	clientConn, clientRW, err := http.NewResponseController(w).Hijack()
	if err != nil {
		upConn.Close()
		h.logger.Error("upgrade hijack failed", "err", err)
		return
	}

	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme = u.Scheme
	out.URL.Host = u.Host
	out.Host = u.Host
	if err := out.Write(upConn); err != nil {
		clientConn.Close()
		upConn.Close()
		return
	}
	// Flush anything the http server's reader buffered past the handshake.
	if clientRW.Reader.Buffered() > 0 {
		if _, err := io.CopyN(upConn, clientRW, int64(clientRW.Reader.Buffered())); err != nil {
			clientConn.Close()
			upConn.Close()
			return
		}
	}

	go relay(upConn, clientConn)
	go relay(clientConn, upConn)
}

// dialUpgrade dials the upstream for a raw upgrade relay: TLS (verified,
// honouring the test-pinned transport roots) or plain for http upstreams.
func (h *Handler) dialUpgrade(u *url.URL) (net.Conn, error) {
	addr := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			addr += ":443"
		} else {
			addr += ":80"
		}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if u.Scheme != "https" {
		return dialer.Dial("tcp", addr)
	}
	tlsCfg := &tls.Config{ServerName: u.Hostname()}
	if h.Transport != nil && h.Transport.TLSClientConfig != nil {
		tlsCfg = h.Transport.TLSClientConfig.Clone()
		tlsCfg.ServerName = u.Hostname()
	}
	return tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
}

// routeUnpooled handles everything spillway does not pool (issue #176):
// relay to the configured upstream with the client's own credential
// verbatim — no injection, no selection, no rotation, no hold.
//
// All three cases end in the same passThrough call, and that is the point:
// mechanically, "this belongs to the client's login", "this is confirmed to
// cost nothing", and "spillway has no idea what this is" all want the same
// treatment, which is to stay out of the way. What differs is what spillway
// is CLAIMING, and the request-log label is where that difference lives, so
// a user reading the log can tell a deliberate bypass from an absence of
// opinion.
//
// Is the client's own credential always the right fallback for the third
// case? Not always — it is the HONEST one. A client that sends no
// credential of its own gets whatever the upstream says to an
// unauthenticated request, where before the inversion it would have been
// handed a pooled account's token and "worked". That is a real behaviour
// change and it is the intended one: spillway supplying a credential to a
// request it does not understand is precisely the defect issue #176 is
// about, and the alternative — guessing — is how /api/oauth/validate came
// to be answered with a pooled account's token. The outcome is also the one
// the client would get with spillway out of the path entirely, and it is
// never silent: the request is counted and labelled "(unpooled)", and a
// POST with a body raises a warning naming the path.
func (h *Handler) routeUnpooled(w http.ResponseWriter, r *http.Request) outcome {
	account := "(unpooled)"
	switch {
	// Identity-bound paths belong to the CLIENT's own login — Remote
	// Control, the CLI's own token refresh, the user's own artifacts (§3
	// request path step 1). Checked first: it is the strongest claim of the
	// three, and the only one that would be actively WRONG to pool.
	case isIdentityPath(r.URL.Path):
		account = "(passthrough)"
	// Confirmed non-quota paths (issue #91). Since the inversion this no
	// longer decides routing — the default already passes these through —
	// but it still decides what the request log says, and it is the list
	// that says "confirmed free" rather than "not recognised".
	case isNonQuotaPath(r.URL.Path):
		account = "(non-quota)"
	// Everything else: spillway has no opinion about this path. Counted,
	// and warned about once if it looks like inference — see unpooled.go,
	// which is what keeps the inverted default from hiding a new endpoint.
	default:
		h.unpooled.note(r)
	}
	h.passThrough(w, r)
	return outcome{account: account, event: reqlog.EventPassthrough}
}
