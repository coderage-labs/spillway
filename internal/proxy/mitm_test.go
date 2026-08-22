package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/mitm"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/secrets"
)

// mitmRig is a TLS upstream + CA + MITM-enabled proxy front server.
type mitmRig struct {
	front    *httptest.Server
	upstream *httptest.Server
	ca       *mitm.CA
}

func newMITMRig(t *testing.T, upstreamHandler http.HandlerFunc) *mitmRig {
	t.Helper()
	upstream := httptest.NewTLSServer(upstreamHandler)
	t.Cleanup(upstream.Close)

	ca, err := mitm.EnsureCA(secrets.NewFake(), filepath.Join(t.TempDir(), "ca.pem"))
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}

	cfg := config.Defaults()
	cfg.Upstream = upstream.URL
	p := pool.New([]*pool.Account{pool.NewAccount("a", pool.SourceYAML, "pool-tok", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	// Our upstream leg must validate the httptest TLS cert.
	h.Transport.TLSClientConfig = &tls.Config{RootCAs: x509Pool(t, upstream)}
	h.SetMITM(ca)

	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	return &mitmRig{front: front, upstream: upstream, ca: ca}
}

func x509Pool(t *testing.T, srv *httptest.Server) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}

// proxyClient builds a client that CONNECTs through the front server and
// validates the MITM leaf against the rig's CA.
func (rig *mitmRig) proxyClient(t *testing.T) *http.Client {
	t.Helper()
	proxyURL, _ := url.Parse(rig.front.URL)
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(rig.ca.CertPEM())
	return &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: caPool},
			// Exercise the h2 path: ALPN must negotiate h2 and the
			// terminated conn must multiplex.
			ForceAttemptHTTP2: true,
		},
		Timeout: 10 * time.Second,
	}
}

// End-to-end CONNECT → MITM: the client validates our leaf against the CA
// (handshake would fail otherwise), we validate the upstream's real cert
// (handshake would fail otherwise), and the request flows through the pool
// path with injection.
func TestConnectMITMEndToEnd(t *testing.T) {
	rig := newMITMRig(t, func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "Bearer pool-tok" {
			t.Errorf("injected Authorization = %q", a)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"via":"mitm"}`)
	})

	client := rig.proxyClient(t)
	resp, err := client.Get(rig.upstream.URL + "/v1/models")
	if err != nil {
		t.Fatalf("request through MITM: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != `{"via":"mitm"}` {
		t.Errorf("status = %d body = %q", resp.StatusCode, body)
	}
}

// A client that does NOT trust our CA must fail the handshake — the leaf is
// not some public-cert shortcut.
func TestConnectMITMRejectsUntrustingClient(t *testing.T) {
	rig := newMITMRig(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached upstream despite untrusted CA")
	})
	proxyURL, _ := url.Parse(rig.front.URL)
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   5 * time.Second,
	}
	if _, err := client.Get(rig.upstream.URL + "/"); err == nil {
		t.Fatal("expected TLS verification failure for untrusting client")
	}
}

// We validate the upstream's cert: with a wrong-root transport the request
// must fail rather than relay.
func TestConnectMITMValidatesUpstream(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached upstream with invalid cert chain")
	}))
	defer upstream.Close()

	ca, err := mitm.EnsureCA(secrets.NewFake(), filepath.Join(t.TempDir(), "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Upstream = upstream.URL
	p := pool.New([]*pool.Account{pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	// Empty root pool: the upstream cert must NOT validate.
	h.Transport.TLSClientConfig = &tls.Config{RootCAs: x509.NewCertPool()}
	h.SetMITM(ca)
	front := httptest.NewServer(h)
	defer front.Close()

	proxyURL, _ := url.Parse(front.URL)
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(ca.CertPEM())
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: caPool}},
		Timeout:   5 * time.Second,
	}
	resp, err := client.Get(upstream.URL + "/v1/models")
	if err != nil {
		t.Fatalf("request through MITM: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (upstream cert rejected)", resp.StatusCode)
	}
}

// CONNECT to a non-allowlisted host blind-tunnels with byte fidelity.
func TestConnectBlindTunnel(t *testing.T) {
	// Echo server as the arbitrary non-allowlisted target.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go io.Copy(c, c)
		}
	}()
	echoAddr := echo.Addr().String()

	cfg := config.Defaults()
	p := pool.New([]*pool.Account{pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()
	frontAddr := strings.TrimPrefix(front.URL, "http://")

	conn, err := net.DialTimeout("tcp", frontAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "200 Connection Established") {
		t.Fatalf("CONNECT response = %q", buf[:n])
	}

	msg := []byte("blind-tunnel-byte-fidelity\x00\x01\x02")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != string(msg) {
		t.Errorf("echo = %q, want %q", got, msg)
	}
}

// Identity paths keep the client's own credential verbatim through both
// modes; /v1/messages still gets pool injection.
func TestIdentityPassThrough(t *testing.T) {
	got := make(chan http.Header, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := config.Defaults()
	cfg.Upstream = upstream.URL
	p := pool.New([]*pool.Account{pool.NewAccount("a", pool.SourceYAML, "pool-tok", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	for _, path := range []string{
		"/v1/oauth/token", "/v1/code/execute", "/api/oauth/files/abc", "/api/oauth/file_upload",
		// RC server-mode paths observed live 2026-08-21.
		"/v1/environments/env-1/work/poll", "/v1/sessions/sess-1/archive",
	} {
		req, _ := http.NewRequest(http.MethodPost, front.URL+path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer client-own-token")
		req.Header.Set("X-Api-Key", "client-api-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		hdr := <-got
		if a := hdr.Get("Authorization"); a != "Bearer client-own-token" {
			t.Errorf("%s: Authorization = %q, want client credential verbatim", path, a)
		}
		if k := hdr.Get("X-Api-Key"); k != "client-api-key" {
			t.Errorf("%s: x-api-key = %q, want client credential verbatim", path, k)
		}
	}

	// Inference paths must never be swallowed by the identity list:
	// /v1/messages stays pool-routed (injected), even though /v1/sessions/
	// is a pass-through prefix.
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{"metadata":{"user_id":"s"},"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	hdr := <-got
	if a := hdr.Get("Authorization"); a != "Bearer pool-tok" {
		t.Errorf("/v1/messages: Authorization = %q, want pool injection", a)
	}
}

// A WebSocket-style Upgrade relays the handshake with client headers and
// then the raw byte stream both ways.
func TestUpgradeRelay(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "not an upgrade", http.StatusBadRequest)
			return
		}
		if a := r.Header.Get("Authorization"); a != "Bearer client-ws-token" {
			t.Errorf("upgrade Authorization = %q, want client credential verbatim", a)
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("upstream hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		io.Copy(rw, rw) // echo frames back
	}))
	defer upstream.Close()

	cfg := config.Defaults()
	cfg.Upstream = upstream.URL
	p := pool.New([]*pool.Account{pool.NewAccount("a", pool.SourceYAML, "pool-tok", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(front.URL, "http://"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /v1/session_ingress/ws/abc HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nAuthorization: Bearer client-ws-token\r\n\r\n", "api.anthropic.com")

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("upgrade response = %q", status)
	}
	// Drain headers.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	// Bidirectional stream: echo check.
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	pong := make([]byte, 4)
	if _, err := io.ReadFull(br, pong); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(pong) != "ping" {
		t.Errorf("echo = %q", pong)
	}
}
