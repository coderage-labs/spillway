package proxy

// End-to-end coverage for issue #66's stale-CA warning: a genuine MITM CA
// regeneration, wired the way main.go wires it (NotifyCARegenerated after
// mitm.EnsureCA reports Regenerated), followed by a real client's TLS
// handshake actually failing against the terminated CONNECT path — not just
// the mitmFailLogger unit exercised directly in mitmfail_test.go.

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/mitm"
	"github.com/coderage-labs/spillway/internal/pool"
)

// failingHandshake dials front and CONNECTs to upstreamHost, but never
// trusts the MITM leaf — the same "client that doesn't trust our CA" shape
// as TestConnectMITMRejectsUntrustingClient — so the handshake fails and
// connect.go's slogWriter routes exactly one failure into h.mitmFails.
func failingHandshake(t *testing.T, frontAddr, upstreamHost string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", frontAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("CONNECT " + upstreamHost + ":443 HTTP/1.1\r\nHost: " + upstreamHost + "\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	buf := make([]byte, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("no CONNECT response: %v", err)
	}
	// An empty RootCAs pool: the client will not trust the served leaf, so
	// the handshake itself fails — this is the ordinary "client doesn't
	// trust the leaf" shape, never a bug in the leaf.
	tlsConn := tls.Client(conn, &tls.Config{ServerName: upstreamHost})
	_ = tlsConn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := tlsConn.Handshake(); err == nil {
		t.Fatal("expected the handshake to fail against an untrusted CA")
	}
}

func newStaleCARig(t *testing.T) (h *Handler, frontAddr, upstreamHost string) {
	t.Helper()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	u := mustHostname(t, upstream.URL)

	ca, err := mitm.EnsureCA(filepath.Join(t.TempDir(), "ca.pem"), []string{u}, testLogger())
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	cfg := config.Defaults()
	cfg.Upstream = upstream.URL
	p := pool.New([]*pool.Account{pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, "")}, time.Now())
	h, err = NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	h.SetMITM(ca)

	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	return h, front.Listener.Addr().String(), u
}

// TestStaleCAWarningNeedsBothRegenerationAndRecurrence: neither ingredient
// alone is enough — a regeneration with no subsequent failures must stay
// false (nothing was actually stranded), and a single real handshake
// failure right after a regeneration must also stay false (that alone is
// the ordinary "client walked away mid-handshake" case). Only a
// regeneration followed by the SAME failure recurring flips it.
func TestStaleCAWarningNeedsBothRegenerationAndRecurrence(t *testing.T) {
	h, frontAddr, upstreamHost := newStaleCARig(t)

	// No regeneration declared at all: real handshake failures must not
	// warn (mirrors TestStrandedNeverArmedByHandshakeFailuresAlone, but
	// through the real CONNECT path).
	failingHandshake(t, frontAddr, upstreamHost)
	if h.StaleCAWarning() {
		t.Fatal("StaleCAWarning() = true before any regeneration was ever declared")
	}

	// Declare the regeneration main.go would have detected.
	h.NotifyCARegenerated()
	if h.StaleCAWarning() {
		t.Error("StaleCAWarning() = true immediately after regeneration with no failures yet")
	}

	// One more real failure: still just one occurrence since arming.
	failingHandshake(t, frontAddr, upstreamHost)
	if h.StaleCAWarning() {
		t.Error("StaleCAWarning() = true after only one post-regeneration handshake failure")
	}

	// The same failure recurring is the actual symptom.
	failingHandshake(t, frontAddr, upstreamHost)
	if !h.StaleCAWarning() {
		t.Error("StaleCAWarning() = false after the same MITM handshake failure recurred following a genuine regeneration")
	}
}
