package proxy

// Issue #177: every non-CONNECT request went to route(), and both
// downstream builders — buildRequest (pooled) and rawRequest (passthrough)
// — rewrote scheme and host to the configured upstream unconditionally. A
// plain-HTTP request the client addressed to some OTHER host was therefore
// delivered to api.anthropic.com, with a pooled credential injected on the
// pooled path. Found live: 3,991 AWS IMDS probes (http://169.254.169.254/
// latest/api/token) answered 404 by Anthropic.
//
// These tests drive the real handler through a real http.Client configured
// with a real Proxy func, so the request arrives in proxy form exactly as an
// exported HTTP_PROXY would deliver it. The pool is HEALTHY here, unlike
// identityRig's exhausted one: that makes the defect's signature directly
// observable — with the gate removed the request reaches the upstream
// carrying "Bearer pool-token", which is the credential injection the issue
// is about.

import (
	"context"
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
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// seenRequest is what a recording server observed.
type seenRequest struct {
	host   string
	path   string
	header http.Header
}

// recorder starts a server that reports every request it receives on the
// returned channel and answers with body.
func recorder(t *testing.T, body string) (*httptest.Server, chan seenRequest) {
	t.Helper()
	ch := make(chan seenRequest, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- seenRequest{host: r.Host, path: r.URL.Path, header: r.Header.Clone()}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

// forwardRig builds the three-server world these tests need:
//
//   - upstream: the configured Anthropic stand-in. Its host (127.0.0.1) is
//     therefore the ONLY loopback host spillway owns.
//   - foreign: a server spillway has never heard of. Addressed as
//     "localhost:<port>" so its hostname differs from the upstream's — same
//     listener either way, but a different name, which is exactly the
//     distinction hostAllowed draws and the one the bug ignored.
//   - front: the proxy listener itself.
//
// pooledToken is injected by buildRequest on the pooled path, so seeing it
// anywhere proves a pooled credential was attached.
type forwardRig struct {
	front        *httptest.Server
	client       *http.Client
	upstreamSeen chan seenRequest
	foreignSeen  chan seenRequest
	foreignHost  string // localhost:<port> — never an owned host
	frontHost    string // localhost:<port> of the proxy listener itself
	upstreamURL  string // the configured upstream, an owned host
	rl           *reqlog.Log
}

const pooledToken = "pool-token"

func newForwardRig(t *testing.T) *forwardRig {
	t.Helper()
	upstream, upstreamSeen := recorder(t, `{"upstream":true}`)
	foreign, foreignSeen := recorder(t, `{"foreign":true}`)

	rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rl.Close() })

	cfg := config.Defaults()
	cfg.Upstream = upstream.URL
	cfg.Pool.ExhaustedMode = "fail"
	acct := pool.NewAccount("pooled", pool.SourceYAML, pooledToken, "", 0, "")
	h, err := NewHandler(&cfg, testLogger(), pool.New([]*pool.Account{acct}, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	h.SetHooks(Hooks{Log: rl})
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)

	frontURL, err := url.Parse(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Guard the premise: if the upstream and the "foreign" server shared a
	// hostname, every assertion below would be vacuous.
	if urlHost(upstream.URL) == "localhost" {
		t.Fatalf("upstream %q must not be addressed as localhost — the foreign host is", upstream.URL)
	}
	rig := &forwardRig{
		front:        front,
		upstreamSeen: upstreamSeen,
		foreignSeen:  foreignSeen,
		foreignHost:  "localhost:" + portOf(t, foreign.URL),
		upstreamURL:  upstream.URL,
		frontHost:    "localhost:" + portOf(t, front.URL),
		rl:           rl,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				Proxy: func(*http.Request) (*url.URL, error) { return frontURL, nil },
			},
		},
	}
	// And guard the other half of the premise: spillway must consider the
	// upstream's host its own and the foreign name not its own. Without
	// this, a future change to wantedHosts could make every test below pass
	// for the wrong reason.
	if !h.hostAllowed(urlHost(upstream.URL)) {
		t.Fatalf("upstream host %q is not in allowedHosts — rig is wrong", urlHost(upstream.URL))
	}
	if h.hostAllowed("localhost") {
		t.Fatal("localhost is in allowedHosts — the 'foreign' host is not foreign, rig is wrong")
	}
	return rig
}

func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func urlHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// lastSeen reports what a recorder saw, or nil if it saw nothing.
func lastSeen(ch chan seenRequest) *seenRequest {
	select {
	case s := <-ch:
		return &s
	default:
		return nil
	}
}

// TestForeignHostReachesTheClientsOwnHostNotTheUpstream is the end-to-end
// proof for issue #177, across BOTH request builders.
//
// Each path is chosen for the builder it lands on with the #177 gate
// removed, so a single planted defect (deleting the gate) must turn every
// subtest red — one builder's coverage cannot stand in for the other's.
func TestForeignHostReachesTheClientsOwnHostNotTheUpstream(t *testing.T) {
	cases := []struct {
		name, method, path, builder string
	}{
		// The live case from the issue: unclassified, so route() sends it
		// through pool selection and buildRequest.
		{"imds probe", http.MethodGet, "/latest/api/token", "buildRequest (pooled)"},
		// Inference path — buildRequest with a buffered body.
		{"inference path", http.MethodPost, "/v1/messages", "buildRequest (pooled)"},
		// Identity-bound — passThrough, so rawRequest.
		{"identity path", http.MethodPost, "/v1/sessions", "rawRequest (passthrough)"},
		// Confirmed non-quota — also passThrough, so also rawRequest.
		{"non-quota path", http.MethodGet, "/api/claude_code/settings", "rawRequest (passthrough)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rig := newForwardRig(t)

			req, err := http.NewRequest(c.method, "http://"+rig.foreignHost+c.path, strings.NewReader(testBody))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer client-token")
			resp, err := rig.client.Do(req)
			if err != nil {
				t.Fatalf("request to the client's own host failed: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if up := lastSeen(rig.upstreamSeen); up != nil {
				t.Errorf("a request for %s reached the configured UPSTREAM (%s %s, Authorization %q) — %s rewrote the destination",
					rig.foreignHost, up.host, up.path, up.header.Get("Authorization"), c.builder)
			}
			fg := lastSeen(rig.foreignSeen)
			if fg == nil {
				t.Fatalf("the client's own host never received the request (status %d, body %q)", resp.StatusCode, body)
			}
			if fg.path != c.path {
				t.Errorf("foreign host saw path %q, want %q", fg.path, c.path)
			}
			if fg.host != rig.foreignHost {
				t.Errorf("foreign host saw Host %q, want %q", fg.host, rig.foreignHost)
			}
			// The hard requirement of #177: nothing spillway issues may be
			// attached to a request aimed somewhere else.
			if got := fg.header.Get("Authorization"); got != "Bearer client-token" {
				t.Errorf("Authorization at the foreign host = %q, want the client's own header verbatim", got)
			}
			if strings.Contains(fg.header.Get("Authorization"), pooledToken) {
				t.Errorf("a POOLED credential was injected into a request aimed at %s", rig.foreignHost)
			}
			if string(body) != `{"foreign":true}` {
				t.Errorf("client got body %q, want the foreign host's own answer", body)
			}

			e := waitForEntry(t, rig.rl)
			if e.Account != "(forward)" {
				t.Errorf("request log account = %q, want %q — a forwarded request must not look like pooled traffic", e.Account, "(forward)")
			}
			if e.Event != reqlog.EventPassthrough {
				t.Errorf("request log event = %q, want %q", e.Event, reqlog.EventPassthrough)
			}
		})
	}
}

// TestForeignHostWithNoClientCredentialGetsNone is the IMDS shape exactly as
// it arrives: an AWS SDK sends no Authorization at all. The forwarded
// request must still carry none — "no injection" has to hold when there is
// nothing of the client's to hide behind.
func TestForeignHostWithNoClientCredentialGetsNone(t *testing.T) {
	rig := newForwardRig(t)

	req, err := http.NewRequest(http.MethodPut, "http://"+rig.foreignHost+"/latest/api/token", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "21600")
	resp, err := rig.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if up := lastSeen(rig.upstreamSeen); up != nil {
		t.Errorf("an IMDS probe reached the configured upstream (%s %s)", up.host, up.path)
	}
	fg := lastSeen(rig.foreignSeen)
	if fg == nil {
		t.Fatal("the IMDS probe never reached the host it was addressed to")
	}
	if got := fg.header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want absent — spillway injected a credential into a foreign request", got)
	}
	if got := fg.header.Get("X-Api-Key"); got != "" {
		t.Errorf("X-Api-Key = %q, want absent", got)
	}
	if got := fg.header.Get("X-aws-ec2-metadata-token-ttl-seconds"); got != "21600" {
		t.Errorf("the client's own header was mangled: %q", got)
	}
}

// TestOwnedHostInProxyFormStillPoolRoutes is the over-match guard: a
// plain-HTTP proxy request naming the CONFIGURED upstream is spillway's to
// claim, and must still go through pool selection with the pooled
// credential injected. The #177 gate must narrow what spillway claims, not
// what it serves.
func TestOwnedHostInProxyFormStillPoolRoutes(t *testing.T) {
	rig := newForwardRig(t)

	req, err := http.NewRequest(http.MethodPost, rig.upstreamURL+"/v1/messages", strings.NewReader(testBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := rig.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if fg := lastSeen(rig.foreignSeen); fg != nil {
		t.Errorf("the request went to the foreign host (%s %s) — the gate over-matched", fg.host, fg.path)
	}
	up := lastSeen(rig.upstreamSeen)
	if up == nil {
		t.Fatalf("the configured upstream was never reached (status %d)", resp.StatusCode)
	}
	if got := up.header.Get("Authorization"); got != "Bearer "+pooledToken {
		t.Errorf("Authorization at the upstream = %q, want the pooled credential — pool routing broke", got)
	}
}

// TestBaseURLModeStillPoolRoutes is the one thing that must never break:
// origin-form POST /v1/messages straight at the listener, which is base-URL
// mode and also exactly what a MITM-terminated CONNECT replays. No host in
// the request line, so the #177 gate must not see one.
func TestBaseURLModeStillPoolRoutes(t *testing.T) {
	rig := newForwardRig(t)

	resp, err := http.Post(rig.front.URL+"/v1/messages", "application/json", strings.NewReader(testBody))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != `{"upstream":true}` {
		t.Errorf("body = %q, want the upstream's answer", body)
	}
	up := lastSeen(rig.upstreamSeen)
	if up == nil {
		t.Fatal("base-URL mode no longer reaches the upstream")
	}
	if got := up.header.Get("Authorization"); got != "Bearer "+pooledToken {
		t.Errorf("Authorization = %q, want the pooled credential injected", got)
	}
	if up.path != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", up.path)
	}
}

// TestBaseURLModeIdentityPathStillPassesThrough is the same control for the
// other builder: rawRequest must still rewrite an origin-form request to the
// upstream, carrying the client's own credential.
func TestBaseURLModeIdentityPathStillPassesThrough(t *testing.T) {
	rig := newForwardRig(t)

	req, err := http.NewRequest(http.MethodPost, rig.front.URL+"/v1/sessions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer client-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	up := lastSeen(rig.upstreamSeen)
	if up == nil {
		t.Fatal("an identity path in base-URL mode no longer reaches the upstream")
	}
	if got := up.header.Get("Authorization"); got != "Bearer client-token" {
		t.Errorf("Authorization = %q, want the client's own credential verbatim", got)
	}
}

// TestProxyFormNamingSpillwayItselfIsNotForwarded pins the self-recognition
// guard: a client with HTTP_PROXY *and* ANTHROPIC_BASE_URL both pointed at
// spillway sends an absolute URI naming spillway's own listener. That host
// is not an upstream, so without addressedToSelf it is "forwarded" to
// spillway — which is where it already was.
//
// That does not hang (forwardDirect dials the listener directly, so the
// second arrival is origin-form and routes normally), which is precisely
// why the request LOG is what this asserts on: the symptom is one client
// request producing two entries, the first of them labelled "(forward)" and
// naming a hop that went nowhere. A status-code assertion would pass
// straight through the defect.
func TestProxyFormNamingSpillwayItselfIsNotForwarded(t *testing.T) {
	rig := newForwardRig(t)

	req, err := http.NewRequest(http.MethodPost, "http://"+rig.frontHost+"/v1/messages", strings.NewReader(testBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := rig.client.Do(req)
	if err != nil {
		t.Fatalf("a request naming spillway's own listener did not complete (%v) — it was forwarded back to spillway", err)
	}
	resp.Body.Close()

	up := lastSeen(rig.upstreamSeen)
	if up == nil {
		t.Fatalf("a request naming spillway's own listener never reached the upstream (status %d)", resp.StatusCode)
	}
	if got := up.header.Get("Authorization"); got != "Bearer "+pooledToken {
		t.Errorf("Authorization = %q, want the pooled credential — it was not routed as spillway's own traffic", got)
	}
	// One client request, one entry, and it names the account that served
	// it. A second entry — or a "(forward)" label — means spillway routed
	// its own listener as if it were somebody else's host.
	e := waitForEntry(t, rig.rl)
	if e.Account != "pooled" {
		t.Errorf("request log account = %q, want %q", e.Account, "pooled")
	}
}

// fakeAddr is a net.Addr whose String() is whatever the test wants the
// connection's local address to be.
type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

// TestAddressedToSelf pins the self-recognition predicate directly,
// including the two ways it must say no: a different port on the same
// machine, and a host that is not this machine at all. The IMDS address is
// in the table on purpose — it is the live case from issue #177, and
// misreading it as "us" would put the bug straight back.
func TestAddressedToSelf(t *testing.T) {
	cases := []struct {
		name, target, local string
		want                bool
	}{
		{"localhost names the 127.0.0.1 listener", "http://localhost:7654/v1/messages", "127.0.0.1:7654", true},
		{"same literal address", "http://127.0.0.1:7654/v1/messages", "127.0.0.1:7654", true},
		{"ipv6 loopback", "http://[::1]:7654/v1/messages", "127.0.0.1:7654", true},
		{"default port fills in from scheme", "http://localhost/v1/messages", "127.0.0.1:80", true},
		{"non-loopback bind naming itself", "http://192.168.1.5:7654/v1/messages", "192.168.1.5:7654", true},
		{"different port on this machine", "http://localhost:9999/v1/messages", "127.0.0.1:7654", false},
		{"the IMDS address", "http://169.254.169.254/latest/api/token", "127.0.0.1:80", false},
		{"a real vendor host", "http://api.anthropic.com/v1/messages", "127.0.0.1:80", false},
		{"a name that merely starts with localhost", "http://localhost.example.com:7654/v1/messages", "127.0.0.1:7654", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.target, nil)
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, fakeAddr(c.local)))
			if got := addressedToSelf(req); got != c.want {
				t.Errorf("addressedToSelf(%q, local %q) = %v, want %v", c.target, c.local, got, c.want)
			}
		})
	}
}

// TestAddressedToSelfWithoutALocalAddr covers the path where the server did
// not put a local address in the context at all: nothing is known, so
// nothing may be assumed to be us.
func TestAddressedToSelfWithoutALocalAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://localhost:7654/v1/messages", nil)
	req = req.WithContext(context.Background())
	if addressedToSelf(req) {
		t.Error("addressedToSelf = true with no local address in the context, want false")
	}
}

// TestSplitHostPortScheme pins the default-port fill-in the loop guard uses
// when the client writes an authority with no port.
func TestSplitHostPortScheme(t *testing.T) {
	cases := []struct{ authority, scheme, host, port string }{
		{"localhost:7654", "http", "localhost", "7654"},
		{"localhost", "http", "localhost", "80"},
		{"localhost", "https", "localhost", "443"},
		{"169.254.169.254", "http", "169.254.169.254", "80"},
	}
	for _, c := range cases {
		host, port := splitHostPortScheme(c.authority, c.scheme)
		if host != c.host || port != c.port {
			t.Errorf("splitHostPortScheme(%q, %q) = (%q, %q), want (%q, %q)",
				c.authority, c.scheme, host, port, c.host, c.port)
		}
	}
}
