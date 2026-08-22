// Package egress decides how spillway reaches an upstream: directly, or via
// a corporate HTTP proxy (design doc §6.13, specified for v1).
//
// Two paths need it and they are easy to get half-right. The pooled request
// path uses net/http, which takes a Proxy function. The MITM path dials a raw
// TCP tunnel itself, so it needs its own CONNECT dance. Sending only the
// first through a corporate proxy would leave every tunnelled host bypassing
// it — silently, and only for the traffic the user could least afford to
// leak.
package egress

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Mode selects how upstreams are reached.
type Mode string

const (
	// Direct dials upstreams itself. The default.
	Direct Mode = "direct"
	// HTTPConnect tunnels through a configured proxy.
	HTTPConnect Mode = "http-connect"
	// Environment honours HTTPS_PROXY/HTTP_PROXY/NO_PROXY. Opt-in rather
	// than the Go default: `spillway run` sets HTTPS_PROXY in its children
	// to point AT spillway, and a daemon started from such a shell would
	// otherwise try to proxy through itself.
	Environment Mode = "environment"
)

// Egress reaches upstreams according to its mode.
type Egress struct {
	mode  Mode
	proxy *url.URL
	dial  *net.Dialer
}

// New builds an Egress. proxyURL is required for HTTPConnect and ignored
// otherwise.
func New(mode string, proxyURL string) (*Egress, error) {
	e := &Egress{
		mode: Mode(mode),
		dial: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
	}
	if e.mode == "" {
		e.mode = Direct
	}
	switch e.mode {
	case Direct, Environment:
		return e, nil
	case HTTPConnect:
		if proxyURL == "" {
			return nil, errors.New(`egress.mode is "http-connect" but egress.proxy is empty`)
		}
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("egress.proxy: %w", err)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("egress.proxy: %q has no host", proxyURL)
		}
		e.proxy = u
		return e, nil
	default:
		return nil, fmt.Errorf("egress.mode: %q must be direct, http-connect or environment", mode)
	}
}

// Mode reports the configured mode, for logging.
func (e *Egress) Mode() Mode { return e.mode }

// ProxyURL reports the configured proxy, or nil.
func (e *Egress) ProxyURL() *url.URL { return e.proxy }

// ProxyFunc is for http.Transport.Proxy. Direct returns nil so the transport
// dials itself.
func (e *Egress) ProxyFunc() func(*http.Request) (*url.URL, error) {
	switch e.mode {
	case HTTPConnect:
		p := e.proxy
		return func(*http.Request) (*url.URL, error) { return p, nil }
	case Environment:
		return http.ProxyFromEnvironment
	default:
		return nil
	}
}

// DialContext opens a raw connection to addr, through the proxy when one
// applies. Used by the MITM path, which tunnels bytes rather than speaking
// HTTP and so cannot rely on Transport.Proxy.
func (e *Egress) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	proxy := e.proxy
	if e.mode == Environment {
		// Ask the same resolver net/http uses, so both paths agree about
		// NO_PROXY and scheme-specific settings.
		req := &http.Request{URL: &url.URL{Scheme: "https", Host: addr}}
		p, err := http.ProxyFromEnvironment(req)
		if err != nil {
			return nil, err
		}
		proxy = p
	}
	if proxy == nil {
		return e.dial.DialContext(ctx, network, addr)
	}
	return e.connectVia(ctx, proxy, addr)
}

// connectVia opens a CONNECT tunnel and hands back the raw socket. TLS to the
// real upstream is established by the caller on top of this, so the proxy
// only ever relays ciphertext.
func (e *Egress) connectVia(ctx context.Context, proxy *url.URL, addr string) (net.Conn, error) {
	conn, err := e.dial.DialContext(ctx, "tcp", proxyHostPort(proxy))
	if err != nil {
		return nil, fmt.Errorf("dial egress proxy %s: %w", proxy.Host, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: http.Header{},
	}
	if u := proxy.User; u != nil {
		pass, _ := u.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+basicAuth(u.Username(), pass))
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write CONNECT to %s: %w", proxy.Host, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read CONNECT response from %s: %w", proxy.Host, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("egress proxy %s refused CONNECT to %s: %s",
			proxy.Host, addr, resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		// The proxy sent bytes past the response header; they belong to the
		// tunnel and must not be dropped.
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

// bufferedConn replays bytes already read into the bufio.Reader.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func proxyHostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return net.JoinHostPort(u.Hostname(), "3128")
}

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}
