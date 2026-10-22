package main

// Issue #259 review round: `spillway run` must not touch the CA until the
// daemon has finished initialising, and must tolerate a daemon that has not
// bound its port yet.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/mitm"
)

// adminStatusProbe reads /api/state from a fake admin server through the
// real admin client code path (adminAPI.get).
func adminStatusProbe(base string, listening func() error) readyProbe {
	api := &adminAPI{client: &http.Client{Timeout: 2 * time.Second}, base: base}
	return readyProbe{
		listening: listening,
		status: func() (string, error) {
			var st struct {
				Status string `json:"status"`
			}
			if err := api.get("/api/state", &st); err != nil {
				return "", err
			}
			return st.Status, nil
		},
	}
}

func TestRunWaitsForReadyBeforeTouchingTheCA(t *testing.T) {
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")
	var polls atomic.Int32
	var readyServed atomic.Bool
	var premature atomic.Bool
	var daemonPEM []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := polls.Add(1)
		status := "starting"
		if n >= 4 {
			status = "ready"
			if !readyServed.Swap(true) {
				// The daemon's last init step: it mints the REAL CA.
				ca, err := mitm.EnsureCA(pemPath, []string{"api.anthropic.com"}, nil)
				if err != nil {
					t.Errorf("fake daemon CA: %v", err)
				} else {
					daemonPEM = ca.CertPEM()
				}
			}
		} else if _, err := os.Stat(pemPath); err == nil {
			premature.Store(true) // run touched the CA while still "starting"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
	}))
	defer srv.Close()

	var errOut bytes.Buffer
	ca, err := prepareCA(context.Background(),
		adminStatusProbe(srv.URL, func() error { return nil }), pemPath, &errOut, 5*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if premature.Load() {
		t.Error("the CA pem existed while the daemon still reported starting: run touched it before ready")
	}
	if !readyServed.Load() {
		t.Fatal("run never waited for /api/state to report ready")
	}
	if !bytes.Equal(ca.CertPEM(), daemonPEM) {
		t.Error("run returned a CA that is not the one the daemon minted at ready (stale anchor, #66)")
	}
	if got := strings.Count(errOut.String(), "waiting for spillway to finish starting"); got != 1 {
		t.Errorf("stderr %q: want the waiting line exactly once, got %d", errOut.String(), got)
	}
}

func TestRunWaitsForAPortThatIsNotBoundYet(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // free port, nothing listening yet

	cfg := config.Defaults()
	cfg.Proxy.Host = "127.0.0.1"
	cfg.Proxy.Port = port

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":"ready"}`)
	}))
	defer srv.Close()

	// The daemon binds 250ms after run starts waiting.
	bound := make(chan net.Listener, 1)
	go func() {
		time.Sleep(250 * time.Millisecond)
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			bound <- nil
			return
		}
		bound <- l
	}()

	p := adminStatusProbe(srv.URL, func() error { return probeListening(&cfg) })
	var errOut bytes.Buffer
	start := time.Now()
	if err := waitReady(context.Background(), p, &errOut, 5*time.Second, 20*time.Millisecond); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	if l := <-bound; l == nil {
		t.Skip("could not rebind the test port")
	} else {
		l.Close()
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Error("returned before the port was bound")
	}
	if !strings.Contains(errOut.String(), "waiting for spillway to start") {
		t.Errorf("stderr = %q, want a waiting line", errOut.String())
	}
}

func TestRunWaitGivesUpWithTheClearError(t *testing.T) {
	cfg := config.Defaults()
	cfg.Proxy.Port = 1
	p := adminStatusProbe("http://127.0.0.1:1", func() error { return probeListening(&cfg) })
	err := waitReady(context.Background(), p, &bytes.Buffer{}, 150*time.Millisecond, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not listening") {
		t.Errorf("err = %v, want the existing 'not listening' refusal", err)
	}
}

func TestRunWaitHonoursCtrlC(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := readyProbe{
		listening: func() error { return errors.New("down") },
		status:    func() (string, error) { return "", nil },
	}
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	err := waitReady(ctx, p, &bytes.Buffer{}, time.Minute, 10*time.Millisecond)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Errorf("err = %v after %v, want prompt cancellation", err, time.Since(start))
	}
}

func TestRunWaitFailsFastOnARealAdminError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	start := time.Now()
	err := waitReady(context.Background(),
		adminStatusProbe(srv.URL, func() error { return nil }), &bytes.Buffer{}, time.Minute, 10*time.Millisecond)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("err = %v after %v, want an immediate error", err, time.Since(start))
	}
}
