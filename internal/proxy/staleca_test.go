package proxy

// End-to-end coverage for issue #66's stale-CA warning: a genuine MITM CA
// regeneration, wired the way main.go wires it (NotifyCARegenerated after
// mitm.EnsureCA reports Regenerated), followed by a real client's TLS
// handshake actually failing against the terminated CONNECT path — not just
// the mitmFailLogger unit exercised directly in mitmfail_test.go.
//
// Issue #96: this file used to prove the RECURRENCE half of the detector
// (the same failure happening twice) by driving two real handshakes and
// asserting the resulting detail strings compared equal. That tests
// whatever text the OS happens to produce for a torn-down TLS handshake,
// which is platform-specific and — on windows-latest — differs from what
// macOS/Linux produce for the same client behaviour, so the test proved
// nothing about the detector and failed there twice. The single-occurrence
// assertions below don't care what text a real failure produces, so they
// stay end-to-end; the recurrence proof now lives below them, fed
// synthesised details covering every platform shape #96 named, through the
// exact slogWriter.Write → mitmFailLogger.log pipeline connect.go uses.
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

// TestStaleCAWarningSingleFailureIsNotEvidence drives real handshakes
// through the real CONNECT path — this is the one end-to-end test worth
// keeping, since a single occurrence's assertions don't depend on what
// text the OS produces for the failure, only on whether the detector fires
// too eagerly. Neither ingredient alone is enough: a regeneration with no
// subsequent failures must stay false (nothing was actually stranded), and
// a single real handshake failure right after a regeneration must also
// stay false (that alone is the ordinary "client walked away mid-handshake"
// case).
func TestStaleCAWarningSingleFailureIsNotEvidence(t *testing.T) {
	h, frontAddr, upstreamHost := newStaleCARig(t)

	// No regeneration declared at all: a real handshake failure must not
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

	// One more real failure: still just one occurrence since arming. This
	// must stay false regardless of what text this OS gives the failure.
	failingHandshake(t, frontAddr, upstreamHost)
	if h.StaleCAWarning() {
		t.Error("StaleCAWarning() = true after only one post-regeneration handshake failure")
	}
}

// writeHandshakeFailure feeds a raw handshake-error message into h through
// the exact same path connect.go wires up: http.Server's ErrorLog writes
// through slogWriter, which normalizes and hands off to mitmFailLogger.log.
// This exercises the real pipeline end to end from raw platform text
// without needing a real socket to produce that text — which is the whole
// point after #96: the raw text is exactly the part that must NOT be
// load-bearing, so tests should be able to supply any known shape directly.
func writeHandshakeFailure(h *Handler, host, rawMsg string) {
	w := &slogWriter{limiter: h.mitmFails, host: host}
	_, _ = w.Write([]byte(rawMsg))
}

// TestStaleCAWarningRecurrenceAcrossPlatformTextShapes is the recurrence
// proof #96 asks for: the SAME underlying failure recurring must trip the
// warning even when the two occurrences produce completely different raw
// text, because that is exactly what happens across platforms (and even
// within one) for a client stuck retrying against a replaced CA. Each case
// pairs two real Go/TLS wordings for what is, underneath, one failure kind.
func TestStaleCAWarningRecurrenceAcrossPlatformTextShapes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		host       string
		raw1, raw2 string
	}{
		{
			name: "connection reset: unix wording then windows wording",
			host: "reset.example.com",
			raw1: "http: TLS handshake error from 127.0.0.1:61018: read tcp 127.0.0.1:7654->127.0.0.1:61018: read: connection reset by peer",
			raw2: "http: TLS handshake error from 127.0.0.1:61019: wsarecv: An existing connection was forcibly closed by the remote host",
		},
		{
			// The pairing issue #96 calls out explicitly: "remote error:
			// tls: bad certificate" recurring across two different
			// underlying text shapes must still trip the warning.
			name: "tls handshake rejected: client alert then garbled decrypt",
			host: "badcert.example.com",
			raw1: "http: TLS handshake error from 127.0.0.1:61020: remote error: tls: bad certificate",
			raw2: "http: TLS handshake error from 127.0.0.1:61021: local error: tls: bad record MAC",
		},
		{
			// An unrecognised message must still get a stable class and
			// still support recurrence — it must not silently opt out of
			// detection just because classifyHandshakeFailure doesn't know
			// this particular wording yet.
			name: "unrecognised wording, twice, differently phrased",
			host: "unknown.example.com",
			raw1: "http: TLS handshake error from 127.0.0.1:61022: some future golang wording nobody anticipated",
			raw2: "http: TLS handshake error from 127.0.0.1:61023: a completely different unrecognised phrasing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newStaleCARig(t)
			h.NotifyCARegenerated()

			writeHandshakeFailure(h, tc.host, tc.raw1)
			if h.StaleCAWarning() {
				t.Fatal("StaleCAWarning() = true after only one occurrence")
			}

			writeHandshakeFailure(h, tc.host, tc.raw2)
			if !h.StaleCAWarning() {
				t.Errorf("StaleCAWarning() = false after the same kind of failure recurred via two different text shapes: %q then %q", tc.raw1, tc.raw2)
			}
		})
	}
}

// TestStaleCAWarningDifferentFailureKindsAreNotRecurrence is the
// false-positive guard: two genuinely DIFFERENT failure kinds on the same
// host must not be mistaken for one client stuck retrying, however coarse
// the classification gets. A warning that fires on unrelated churn is worse
// than none (issue #66's original ask).
func TestStaleCAWarningDifferentFailureKindsAreNotRecurrence(t *testing.T) {
	h, _, _ := newStaleCARig(t)
	h.NotifyCARegenerated()

	writeHandshakeFailure(h, "churn.example.com", "http: TLS handshake error from 127.0.0.1:61030: remote error: tls: bad certificate")
	writeHandshakeFailure(h, "churn.example.com", "http: TLS handshake error from 127.0.0.1:61031: read tcp 127.0.0.1:7654->127.0.0.1:61031: read: connection reset by peer")

	if h.StaleCAWarning() {
		t.Error("StaleCAWarning() = true for two DIFFERENT failure kinds on the same host — that is unrelated churn, not one client stuck retrying")
	}
}
