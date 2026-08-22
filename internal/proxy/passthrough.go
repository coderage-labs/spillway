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
)

// isIdentityPath reports whether a path is identity-bound: it must reach the
// upstream with the client's own Authorization/x-api-key untouched. This is
// what keeps Remote Control and the CLI's own token refresh working through
// the proxy. The list is EMPIRICAL — RC server-mode paths observed live
// 2026-08-21 (e.g. /v1/environments/<id>/work/poll, /v1/sessions/<id>/archive)
// — extend it as new identity paths surface. Inference paths
// (/v1/messages) must never be added: those are pool-routed.
func isIdentityPath(path string) bool {
	switch path {
	case "/v1/oauth/token", "/api/oauth/file_upload":
		return true
	}
	return strings.HasPrefix(path, "/v1/code/") ||
		strings.HasPrefix(path, "/api/oauth/files/") ||
		strings.HasPrefix(path, "/v1/environments/") ||
		strings.HasPrefix(path, "/v1/sessions/")
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
	writeResponse(w, resp)
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
