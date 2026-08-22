package egress

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProxy accepts CONNECT and, on success, joins the client to a real
// backend so the test can prove bytes flow end to end.
func fakeProxy(t *testing.T, backend string, accept bool, wantAuth string) (addr string, seen *atomic.Int32) {
	t.Helper()
	seen = &atomic.Int32{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					c.Close()
					return
				}
				if req.Method != http.MethodConnect {
					io.WriteString(c, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
					c.Close()
					return
				}
				if wantAuth != "" && req.Header.Get("Proxy-Authorization") != wantAuth {
					io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
					c.Close()
					return
				}
				seen.Add(1)
				if !accept {
					io.WriteString(c, "HTTP/1.1 403 Forbidden\r\n\r\n")
					c.Close()
					return
				}
				up, err := net.Dial("tcp", backend)
				if err != nil {
					c.Close()
					return
				}
				io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
				go func() { io.Copy(up, br); up.Close() }()
				go func() { io.Copy(c, up); c.Close() }()
			}(c)
		}
	}()
	return ln.Addr().String(), seen
}

// echoServer returns the address of a TCP server that echoes a fixed banner.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, "BACKEND-OK")
			c.Close()
		}
	}()
	return ln.Addr().String()
}

func TestDirectDialsUpstreamItself(t *testing.T) {
	backend := echoServer(t)
	e, err := New("", "")
	if err != nil {
		t.Fatal(err)
	}
	if e.Mode() != Direct {
		t.Errorf("empty mode should default to direct, got %q", e.Mode())
	}
	if e.ProxyFunc() != nil {
		t.Error("direct must leave Transport.Proxy nil")
	}
	conn, err := e.DialContext(context.Background(), "tcp", backend)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	assertBanner(t, conn)
}

// The whole point: bytes reach the upstream THROUGH the proxy.
func TestHTTPConnectTunnelsToUpstream(t *testing.T) {
	backend := echoServer(t)
	proxyAddr, seen := fakeProxy(t, backend, true, "")

	e, err := New("http-connect", "http://"+proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := e.DialContext(context.Background(), "tcp", backend)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	assertBanner(t, conn)
	if seen.Load() != 1 {
		t.Errorf("proxy saw %d CONNECTs, want 1", seen.Load())
	}
}

func TestHTTPConnectSendsProxyCredentials(t *testing.T) {
	backend := echoServer(t)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:s3cret"))
	proxyAddr, seen := fakeProxy(t, backend, true, want)

	e, err := New("http-connect", "http://bob:s3cret@"+proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := e.DialContext(context.Background(), "tcp", backend)
	if err != nil {
		t.Fatalf("authenticated CONNECT failed: %v", err)
	}
	defer conn.Close()
	assertBanner(t, conn)
	if seen.Load() != 1 {
		t.Error("proxy did not accept the credentials")
	}
}

// A refusal must name the proxy and the target — "connection failed" sends
// people looking in the wrong place.
func TestHTTPConnectRefusalIsExplained(t *testing.T) {
	backend := echoServer(t)
	proxyAddr, _ := fakeProxy(t, backend, false, "")
	e, _ := New("http-connect", "http://"+proxyAddr)

	_, err := e.DialContext(context.Background(), "tcp", backend)
	if err == nil {
		t.Fatal("expected the refusal to surface")
	}
	for _, want := range []string{"refused CONNECT", proxyAddr, backend} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestConfigErrors(t *testing.T) {
	if _, err := New("http-connect", ""); err == nil {
		t.Error("http-connect without a proxy URL must be rejected at startup")
	}
	if _, err := New("sideways", ""); err == nil {
		t.Error("an unknown mode must be rejected rather than silently direct")
	}
	if _, err := New("http-connect", "://nonsense"); err == nil {
		t.Error("an unparseable proxy URL must be rejected")
	}
}

// Environment mode must be opt-in: `spillway run` points HTTPS_PROXY at
// spillway itself, so a daemon defaulting to the environment would try to
// proxy through itself.
func TestEnvironmentIsNotTheDefault(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	e, _ := New("", "")
	if e.ProxyFunc() != nil {
		t.Error("direct mode must ignore HTTPS_PROXY")
	}
	backend := echoServer(t)
	conn, err := e.DialContext(context.Background(), "tcp", backend)
	if err != nil {
		t.Fatalf("direct dial should ignore the environment proxy: %v", err)
	}
	conn.Close()

	env, _ := New("environment", "")
	if env.ProxyFunc() == nil {
		t.Error("environment mode should supply a proxy function")
	}
}

func assertBanner(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, len("BACKEND-OK"))
	if _, err := io.ReadFull(conn, buf); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read from upstream: %v", err)
	}
	if string(buf) != "BACKEND-OK" {
		t.Errorf("upstream said %q, want BACKEND-OK", buf)
	}
}
