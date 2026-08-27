package proxy

// Issue #87 coverage: (1) the MITM host set now includes every registered
// provider's DefaultUpstream up front, not just the hosts of accounts
// configured that day, so a live-added account for a not-yet-configured
// provider is already covered; (2) that inclusion must not cause a restart
// with an unchanged provider set to regenerate the CA (#70's guarantee must
// survive); (3) a custom, provider-unknown host must never be silently
// promoted into CONNECT termination without a leaf to back it; (4)
// allowedHosts is now mutated at runtime (RefreshAllowedHosts) and must be
// race-clean against in-flight CONNECTs.

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/mitm"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
)

// slogLoggerTo is testLogger's buffer-capturing sibling (mitm's ca_test.go
// has its own copy of the same helper; not shared across packages).
func slogLoggerTo(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

func containsHost(hosts []string, want string) bool {
	for _, h := range hosts {
		if h == want {
			return true
		}
	}
	return false
}

// TestAllowedHostsIncludesEveryProviderDefault: a pool with only a
// claude-oauth account must still report kimi's default upstream host —
// the whole point of pre-minting every provider's leaf up front (#87),
// rather than only the hosts of accounts configured today.
func TestAllowedHostsIncludesEveryProviderDefault(t *testing.T) {
	cfg := config.Defaults()
	p := pool.New([]*pool.Account{pool.NewAccount("claude-1", pool.SourceYAML, "tok", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	hosts := h.AllowedHosts()
	for _, want := range []string{"api.anthropic.com", "api.kimi.com"} {
		if !containsHost(hosts, want) {
			t.Errorf("AllowedHosts() = %v, missing provider default host %q", hosts, want)
		}
	}
}

// TestRestartWithAddedAccountForKnownProviderDoesNotRegenerateCA is the
// explicit no-regeneration-on-restart assertion #87's issue text calls for:
// the covering test in mitm.EnsureCA must compare against the PROVIDER set,
// not the configured-accounts set. Adding a kimi account to a previously
// claude-only pool must not change the wanted host set at all, so a
// restart afterwards must not regenerate the CA — #70's guarantee has to
// survive this feature.
func TestRestartWithAddedAccountForKnownProviderDoesNotRegenerateCA(t *testing.T) {
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")
	cfg := config.Defaults()

	// "Start 1": claude-only pool.
	p1 := pool.New([]*pool.Account{pool.NewAccount("claude-1", pool.SourceYAML, "tok", "", 0, "")}, time.Now())
	h1, err := NewHandler(&cfg, testLogger(), p1)
	if err != nil {
		t.Fatal(err)
	}
	ca1, err := mitm.EnsureCA(pemPath, h1.AllowedHosts(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if ca1.Regenerated {
		t.Fatal("first-ever install must not report Regenerated — nothing existed to strand")
	}
	if _, err := ca1.Leaf("api.kimi.com"); err != nil {
		t.Fatalf("kimi's default upstream must already have a leaf even with zero kimi accounts configured: %v", err)
	}

	// "Start 2": a kimi account has now been added to the config (still via
	// a restart, per #87's scope — only the LIVE-add path is issue #87's
	// new behaviour). The provider set is unchanged, so the wanted host set
	// must be unchanged too.
	p2 := pool.New([]*pool.Account{
		pool.NewAccount("claude-1", pool.SourceYAML, "tok", "", 0, ""),
		pool.NewAccount("kimi-1", pool.SourceYAML, "tok2", "", 0, provider.KimiUpstream),
	}, time.Now())
	h2, err := NewHandler(&cfg, testLogger(), p2)
	if err != nil {
		t.Fatal(err)
	}
	hosts2 := h2.AllowedHosts()

	var logbuf bytes.Buffer
	ca2, err := mitm.EnsureCA(pemPath, hosts2, slogLoggerTo(&logbuf))
	if err != nil {
		t.Fatal(err)
	}
	if ca2.Regenerated {
		t.Fatalf("restart after adding an account for an ALREADY-provider-covered host regenerated the CA — "+
			"this strands every already-running proxied CLI for no reason; log: %s", logbuf.String())
	}
	if !bytes.Equal(ca1.CertPEM(), ca2.CertPEM()) {
		t.Error("CA cert rotated across a restart that added no new provider")
	}
}

// TestRefreshAllowedHostsDoesNotPromoteUncoveredCustomHost: a pool.Add of an
// account whose upstream is a genuinely custom host (not any provider's
// default) must not make that host CONNECT-terminated — there is no
// pre-minted leaf for it, so allowing termination would turn a working
// blind tunnel into a hard "leaf mint failed" failure. This is #87's "still
// restart-only" case.
func TestRefreshAllowedHostsDoesNotPromoteUncoveredCustomHost(t *testing.T) {
	cfg := config.Defaults()
	p := pool.New([]*pool.Account{pool.NewAccount("claude-1", pool.SourceYAML, "tok", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := mitm.EnsureCA(filepath.Join(t.TempDir(), "spillway-ca.pem"), h.AllowedHosts(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	h.SetMITM(ca)

	if h.MITMCovers("custom.example.com") {
		t.Fatal("a custom host must not already be covered before it's ever configured")
	}

	custom := pool.NewAccount("custom-1", pool.SourceYAML, "tok3", "", 0, "https://custom.example.com/v1")
	if !p.Add(custom) {
		t.Fatal("Add reported false for a brand new name")
	}
	h.RefreshAllowedHosts()

	if h.hostAllowed("custom.example.com") {
		t.Fatal("RefreshAllowedHosts must not promote a host with no pre-minted leaf into CONNECT termination")
	}
	if h.MITMCovers("custom.example.com") {
		t.Fatal("MITMCovers must stay false for a host EnsureCA was never told about")
	}
}

// TestRefreshAllowedHostsIsNoOpForKnownProviderAccount: adding a kimi
// account to a claude-only pool live must not need RefreshAllowedHosts to
// do anything — the host was already allowed from the very first SetMITM
// call, because it's a provider default (#87 point 2).
func TestRefreshAllowedHostsIsNoOpForKnownProviderAccount(t *testing.T) {
	cfg := config.Defaults()
	p := pool.New([]*pool.Account{pool.NewAccount("claude-1", pool.SourceYAML, "tok", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := mitm.EnsureCA(filepath.Join(t.TempDir(), "spillway-ca.pem"), h.AllowedHosts(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	h.SetMITM(ca)

	if !h.hostAllowed("api.kimi.com") {
		t.Fatal("api.kimi.com must already be allowed before any kimi account exists")
	}

	kimi := pool.NewAccount("kimi-1", pool.SourceYAML, "tok2", "", 0, provider.KimiUpstream)
	if !p.Add(kimi) {
		t.Fatal("Add reported false for a brand new name")
	}
	// No restart, no chain regeneration — RefreshAllowedHosts merely
	// confirms what was already true.
	h.RefreshAllowedHosts()
	if !h.hostAllowed("api.kimi.com") {
		t.Fatal("api.kimi.com must still be allowed after the kimi account is added")
	}
	if !h.MITMCovers("api.kimi.com") {
		t.Fatal("the kimi leaf minted at startup must still be there — no regeneration needed")
	}
}

// TestConcurrentHostSetChangeAgainstInFlightConnects is the race-detector
// regression #87 calls for explicitly: RefreshAllowedHosts now mutates
// allowedHosts at runtime, concurrently with real CONNECTs landing on
// connect.go's hostAllowed read. A sequential test proves nothing here —
// this drives real network CONNECTs against a live front server while a
// second goroutine repeatedly recomputes the host set. Run with -race.
func TestConcurrentHostSetChangeAgainstInFlightConnects(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamHost := mustHostname(t, upstream.URL)

	cfg := config.Defaults()
	cfg.Upstream = upstream.URL
	p := pool.New([]*pool.Account{pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	h.Transport.TLSClientConfig = nil // handshake failure doesn't matter; only the race does

	ca, err := mitm.EnsureCA(filepath.Join(t.TempDir(), "spillway-ca.pem"), h.AllowedHosts(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	h.SetMITM(ca)

	front := httptest.NewServer(h)
	defer front.Close()
	frontAddr := strings.TrimPrefix(front.URL, "http://")

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: repeatedly recomputes and swaps in the host set — the same
	// call an admin account-add handler makes after pool.Add.
	go func() {
		defer wg.Done()
		for i := 0; i < 1500; i++ {
			extra := pool.NewAccount(fmt.Sprintf("extra-%d", i), pool.SourceYAML, "tok", "", 0, "")
			p.Add(extra)
			h.RefreshAllowedHosts()
		}
		stop.Store(true)
	}()

	// Reader: drives real CONNECTs against the live front server, exactly
	// the request path that reads allowedHosts on every call.
	go func() {
		defer wg.Done()
		for !stop.Load() {
			conn, err := net.DialTimeout("tcp", frontAddr, 2*time.Second)
			if err != nil {
				continue
			}
			fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", upstreamHost, upstreamHost)
			buf := make([]byte, 256)
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _ = conn.Read(buf)
			conn.Close()
		}
	}()

	wg.Wait()
}
