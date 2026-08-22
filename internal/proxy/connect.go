package proxy

// CONNECT handling: hosts of configured upstreams are MITM-terminated with a
// locally minted leaf and fed into the same pool handler as base-URL mode;
// everything else is a blind TCP relay — we only ever decrypt the configured
// vendor hosts (§3, §5).

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/http2"
)

func (h *Handler) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := splitConnectTarget(r.Host)
	if err != nil {
		http.Error(w, "spillway: bad CONNECT target", http.StatusBadRequest)
		return
	}
	if h.ca != nil && h.allowedHosts[host] {
		h.terminateConnect(w, r, host)
		return
	}
	h.tunnelConnect(w, r, host, port)
}

func splitConnectTarget(authority string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(authority)
	if err != nil {
		// CONNECT with no explicit port: 443 is the only sane default here.
		if host = authority; host != "" {
			return host, "443", nil
		}
		return "", "", err
	}
	return host, port, nil
}

// terminateConnect MITMs the tunnel: hijack, 200, TLS with the leaf for
// host, then serve the conn with this same handler — one request path, two
// front doors.
func (h *Handler) terminateConnect(w http.ResponseWriter, r *http.Request, host string) {
	leaf, err := h.ca.Leaf(host)
	if err != nil {
		h.logger.Error("leaf mint failed", "host", host, "err", err)
		http.Error(w, "spillway: leaf mint failed", http.StatusBadGateway)
		return
	}
	conn, err := hijack(w)
	if err != nil {
		h.logger.Error("CONNECT hijack failed", "err", err)
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		conn.Close()
		return
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		NextProtos:   []string{"h2", "http/1.1"},
	}
	tlsConn := tls.Server(conn, tlsCfg)

	go func() {
		srv := &http.Server{Handler: h}
		_ = http2.ConfigureServer(srv, &http2.Server{})
		// One shot: Accept yields the tunnel conn once, then EOF, and Serve
		// returns immediately — but the server's conn handler keeps serving
		// that conn (h1 keep-alive / h2 streams) afterwards. Do NOT close the
		// conn here: Serve returning does not mean the tunnel is done.
		_ = srv.Serve(&oneConnListener{conn: tlsConn})
	}()
}

// tunnelConnect blind-relays the tunnel: dial the target, 200, copy bytes
// both ways. No auth gate: a proxy checks Proxy-Authorization because it
// supports binding to non-loopback interfaces (a remote client could
// otherwise steal injected tokens or abuse the relay); spillway's listener
// is loopback-only by design (§5), so there is nothing to gate.
func (h *Handler) tunnelConnect(w http.ResponseWriter, r *http.Request, host, port string) {
	// Through the same egress as everything else: a corporate proxy that
	// only saw pooled requests, while tunnelled hosts went direct, would be
	// worse than none.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	up, err := h.egress.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		h.logger.Warn("tunnel dial failed", "host", host, "port", port, "err", err)
		// Reply before hijack so the client gets a real proxy error, not a
		// silent drop.
		http.Error(w, "spillway: tunnel dial failed", http.StatusBadGateway)
		return
	}
	conn, err := hijack(w)
	if err != nil {
		up.Close()
		h.logger.Error("CONNECT hijack failed", "err", err)
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		conn.Close()
		up.Close()
		return
	}

	// Either direction finishing tears down both — a one-sided close must
	// not leave the paired socket lingering.
	go relay(up, conn)
	go relay(conn, up)
}

func relay(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	dst.Close()
	src.Close()
}

// hijack extracts the client conn through the logging wrapper.
func hijack(w http.ResponseWriter) (net.Conn, error) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, errors.New("hijack: " + err.Error())
	}
	return conn, nil
}

// oneConnListener yields a single conn then EOF.
type oneConnListener struct {
	conn net.Conn
	done bool
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.done {
		return nil, io.EOF
	}
	l.done = true
	return l.conn, nil
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
