package proxy

// Plain-HTTP traffic for hosts spillway does not own (issue #177).
//
// The README's MITM-mode contract is explicit: "hosts of configured
// upstreams are terminated ... every other host is a blind TCP relay, so
// only configured vendor hosts are ever decrypted". handleConnect has
// always honoured that — allowedHosts decides, and anything else is
// tunnelled untouched. Plain HTTP never got the same rule: every
// non-CONNECT request went to route(), and BOTH downstream builders
// (buildRequest on the pooled path, rawRequest on the passthrough path)
// rewrote scheme/host to the configured upstream unconditionally. So a
// request the client addressed to some other host was delivered to
// api.anthropic.com instead — with a pooled account's credential injected
// on the pooled path.
//
// Found live: AWS SDKs probe the instance metadata service at
// http://169.254.169.254/latest/api/token as a standard step in their
// credential chain. IMDS is plain HTTP, so with HTTP_PROXY exported for
// spillway those probes arrived here and were forwarded to Anthropic, which
// answered 404 3,991 times. No Claude Code involvement at all.
//
// Why forward rather than refuse. Both were defensible and the choice is
// deliberate. Refusing would make spillway a proxy that relays
// https://anything (CONNECT blind-tunnels it today) but rejects
// http://anything — a new policy that contradicts the documented one, and a
// machine-wide breakage for anyone who exports HTTP_PROXY globally, which is
// the way spillway is meant to be pointed at a CLI that has no per-tool
// proxy setting. Forwarding instead RESTORES the stated invariant on the one
// path that never implemented it: spillway claims only the hosts it owns,
// and everything else passes through untouched. Either way the hard
// requirement is the same and is met here — nothing spillway issues is ever
// attached to a request aimed somewhere else.

import (
	"net"
	"net/http"
	"strings"

	"github.com/coderage-labs/spillway/internal/netaddr"
)

// foreignHost returns the host:port the CLIENT named when that host is not
// one spillway owns, or "" when the request is spillway's to route.
//
// Only a proxy-form request line carries a host at all (r.URL.Host is set
// exactly when the client sent an absolute URI, which is what an HTTP client
// does when it is talking to a proxy). Everything else — base-URL mode, and
// every request read back out of a MITM-terminated CONNECT tunnel — arrives
// origin-form with an empty r.URL.Host, addressed to spillway itself, and
// must keep being rewritten to the upstream exactly as before.
//
// Ownership is decided by hostAllowed, the same set and the same lock
// handleConnect consults, so the two front doors cannot drift apart.
func (h *Handler) foreignHost(r *http.Request) string {
	if r.URL == nil || r.URL.Host == "" {
		return ""
	}
	if h.hostAllowed(r.URL.Hostname()) {
		return ""
	}
	// A client with HTTP_PROXY *and* ANTHROPIC_BASE_URL both pointed at
	// spillway sends an absolute URI naming spillway's own listener. That
	// host is not an upstream, so without this it would be "forwarded" to
	// spillway — which is where it already was.
	//
	// It does not recurse forever: forwardDirect dials the listener
	// directly, so the second arrival is origin-form and routes normally.
	// The cost is a pointless extra hop through our own listener and, more
	// visibly, two request-log entries for one client request — the first
	// labelled "(forward)", which is a lie about where it went. Recognising
	// ourselves is cheaper and truthful.
	if addressedToSelf(r) {
		return ""
	}
	return r.URL.Host
}

// addressedToSelf reports whether the absolute-form target names the
// listener this request arrived on: same port, and both sides loopback (or
// literally the same host).
func addressedToSelf(r *http.Request) bool {
	local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if local == nil {
		return false
	}
	_, localPort, err := net.SplitHostPort(local.String())
	if err != nil {
		return false
	}
	host, port := splitHostPortScheme(r.URL.Host, r.URL.Scheme)
	if port != localPort {
		return false
	}
	// netaddr.IsLoopback, not a local copy: its package doc exists to stop
	// exactly this predicate drifting between callers.
	return strings.EqualFold(r.URL.Host, local.String()) ||
		(netaddr.IsLoopback(host) && netaddr.IsLoopback(local.String()))
}

// splitHostPortScheme splits an authority, defaulting the port from the URL
// scheme when the authority carries none.
func splitHostPortScheme(authority, scheme string) (host, port string) {
	if h, p, err := net.SplitHostPort(authority); err == nil {
		return h, p
	}
	if scheme == "https" {
		return authority, "443"
	}
	return authority, "80"
}

// forwardDirect relays a request to the host the CLIENT asked for: the
// plain-HTTP equivalent of handleConnect's blind tunnel. No pool selection,
// no credential injection, no body buffering, no failover — spillway is a
// wire here and nothing more. The client's own headers go out verbatim
// (hop-by-hop stripped, per RFC 7230), which for a foreign host is the only
// correct thing to do with them: they were the client's to send.
//
// Protocol upgrades to a foreign host are not supported — Upgrade is a
// hop-by-hop header and is stripped, so the origin sees an ordinary request
// and answers however it answers. That is still the client's own host
// answering, which is the whole point; relaying it to the Anthropic upstream
// instead, as this did before issue #177, was never useful to anyone.
func (h *Handler) forwardDirect(w http.ResponseWriter, r *http.Request) {
	h.logger.Debug("forward", "host", r.URL.Host, "path", r.URL.Path, "mode", "direct")
	out := r.Clone(r.Context())
	// The destination is left exactly as the client wrote it. This is the
	// one request path in the package that does NOT touch out.URL.Scheme /
	// out.URL.Host / out.Host, and that is the entire fix for #177.
	out.RequestURI = ""
	out.Host = r.URL.Host
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

	resp, err := h.Transport.RoundTrip(out)
	if err != nil {
		h.logger.Debug("forward failed", "host", r.URL.Host, "err", err)
		http.Error(w, "spillway: forward to "+r.URL.Host+" failed", http.StatusBadGateway)
		return
	}
	writeResponse(w, resp, h.logger)
}
