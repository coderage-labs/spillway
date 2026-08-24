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
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/http2"
)

func (h *Handler) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := splitConnectTarget(r.Host)
	if err != nil {
		http.Error(w, "spillway: bad CONNECT target", http.StatusBadRequest)
		return
	}
	// Log what is intercepted and what is tunnelled. Without this a MITM
	// problem presents as "some requests are missing" with nothing to say
	// which host, which mode, or whether the client ever got that far —
	// which is exactly how a Remote Control failure looked.
	if h.ca != nil && h.allowedHosts[host] {
		h.logger.Debug("connect", "host", host, "mode", "mitm")
		h.terminateConnect(w, r, host)
		return
	}
	h.logger.Debug("connect", "host", host, "mode", "tunnel")
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

	// ALPN has to be negotiated tolerantly, not imposed.
	//
	// With a fixed NextProtos, Go answers a client whose ALPN list does not
	// overlap ours with `no_application_protocol` (alert 120) and kills the
	// connection — the client sees a reset during the handshake, before it
	// has sent a byte of HTTP. Node's h2 server, which other proxies of this
	// shape are built on, instead selects nothing and carries on as HTTP/1.1.
	//
	// That difference broke Remote Control: its realtime channel is a
	// WebSocket, and a WebSocket client that offers an ALPN we do not list
	// never completes the handshake, so the receive channel silently never
	// exists while every ordinary request keeps working.
	ours := []string{"h2", "http/1.1"}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		NextProtos:   ours,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			// Log every hello. A CONNECT that never produces one means the
			// client hung up before speaking TLS at all, which is a very
			// different problem from one that rejects what we send back.
			h.logger.Debug("tls hello",
				"host", host, "sni", hello.ServerName,
				"alpn", hello.SupportedProtos, "versions", hello.SupportedVersions)
			if len(hello.SupportedProtos) == 0 {
				return nil, nil // no ALPN offered; nothing to negotiate
			}
			for _, want := range hello.SupportedProtos {
				for _, have := range ours {
					if want == have {
						return nil, nil // normal path
					}
				}
			}
			// No overlap: negotiate NOTHING, and never mirror.
			//
			// Go's http.Server, after the handshake:
			//
			//   if proto := ...NegotiatedProtocol; validNextProto(proto) {
			//       if fn := c.server.TLSNextProto[proto]; fn != nil { fn(...) }
			//       return          // returns even with no handler
			//   }
			//
			// validNextProto is false only for "", "http/1.1" and "http/1.0".
			// So echoing back a protocol we cannot serve — Remote Control's
			// WebSocket offers "websocket" — makes the server find no handler
			// and close the connection without a word. An earlier attempt
			// mirrored it and moved the failure from the handshake to
			// immediately after it, which is worse: still dead, now silent.
			//
			// Selecting nothing leaves proto "", so the connection is served
			// as HTTP/1.1 and the Upgrade request reaches relayUpgrade, which
			// is what a WebSocket needs.
			h.logger.Debug("alpn: no overlap, negotiating none so this serves as http/1.1",
				"host", host, "client_offered", hello.SupportedProtos)
			cfg := &tls.Config{Certificates: []tls.Certificate{*leaf}}
			return cfg, nil
		},
	}
	tlsConn := tls.Server(conn, tlsCfg)

	go func() {
		srv := &http.Server{Handler: h}
		// Route the server's own errors through slog, tagged with the host.
		// TLS handshake failures otherwise reach the default logger as a bare
		// "TLS handshake error from 127.0.0.1:PORT" — no host, no SNI, and no
		// way to tell which upstream the client gave up on.
		srv.ErrorLog = log.New(&slogWriter{
			limiter: h.mitmFails,
			host:    host,
		}, "", 0)
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

// slogWriter adapts http.Server's ErrorLog to slog, keeping the CONNECT host
// alongside the message.
type slogWriter struct {
	limiter *mitmFailLogger
	host    string
}

func (w *slogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	// A client that walks away mid-handshake is ordinary — a cancelled
	// request, a closed tab — so this is a warning about the host, not an
	// error about the proxy. Routed through mitmFailLogger (issue #64)
	// rather than logged directly: a client stuck retrying a MITM'd host it
	// doesn't trust produces this same (host, detail) pair every attempt,
	// forever, and did once produce 11,665 lines in a single afternoon.
	w.limiter.log(w.host, normalizeHandshakeDetail(msg))
	return len(p), nil
}

// handshakeErrFromAddr matches the leading part of Go's http.Server TLS
// handshake error text that is guaranteed to differ on every single
// attempt: "http: TLS handshake error from <client-addr>: ". <client-addr>
// is the client's ephemeral source port — a fresh CONNECT is a fresh TCP
// connection with a fresh port every time, whether it is one client
// retrying forever or a hundred different ones failing once each.
//
// \S+ (rather than trying to parse an IPv4/IPv6/port triple) is deliberate:
// it matches either address form, since neither contains a space, and it is
// anchored by the literal ": " that always follows the address in this
// message, so it cannot run past it.
var handshakeErrFromAddr = regexp.MustCompile(`^http: TLS handshake error from \S+: `)

// ipPortPattern matches ANY bare IPv4:port or bracketed [IPv6]:port pair,
// wherever it occurs in the message — not just the leading one
// handshakeErrFromAddr strips. A handshake failure that bottoms out in a
// network read/write error carries a SECOND, independent address pair of
// its own: a *net.OpError formats as "<op> <net> <local>-><remote>: <err>",
// e.g. "read tcp 127.0.0.1:7654->127.0.0.1:61018: read: connection reset by
// peer" — and <remote> is the client's ephemeral port, exactly as volatile
// as the one already stripped above. Measured against a real production
// log: roughly a fifth of all "mitm connection failed" occurrences were
// this exact shape, and every one carried a distinct port, so stripping
// only the leading address left both #64's dedup and this detector blind
// to about 20% of failures.
var ipPortPattern = regexp.MustCompile(`\b(?:\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}|\[[0-9a-fA-F:]+\]):\d+\b`)

// normalizeHandshakeDetail turns a raw handshake-failure message into a
// stable key for the SAME underlying failure recurring — the exact byte
// text of whatever address happened to be involved is not load-bearing;
// what matters is "same host, same kind of failure".
//
// Two passes: first the guaranteed leading "from <addr>: " Go always adds,
// then a general sweep for any OTHER address:port pair anywhere else in
// the message (the read/write-error case above). Without both, "detail" is
// never actually the same string twice for the same underlying failure
// against the same host — a fresh CONNECT is a fresh TCP connection with a
// fresh source port, always. That silently limited #64's own fix to a
// coincidence that never happens in practice, and it defeats issue #66's
// stale-CA detector outright: that detector's whole signal is the SAME
// failure recurring, which never compares equal without this.
func normalizeHandshakeDetail(msg string) string {
	msg = handshakeErrFromAddr.ReplaceAllString(msg, "")
	msg = ipPortPattern.ReplaceAllString(msg, "<addr>")
	return msg
}
