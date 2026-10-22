package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func quietGateLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// waitHeld fails the test (rather than hanging it) if n requests are not
// held within 2s — a neutered gate never holds, and an unbounded poll loop
// here used to turn that failure into a hang.
func waitHeld(t *testing.T, g *Gate, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for g.Held() != n {
		if time.Now().After(deadline) {
			t.Fatalf("held = %d, want %d: the request was never held by the gate", g.Held(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "served") })

// Issue #259: a request that arrives before init is HELD, then served — over a
// real listener, so "accepted before ready" is the TCP accept and not a
// handler call.
func TestGateHoldsThenServes(t *testing.T) {
	g := NewGate(quietGateLogger(), time.Minute)
	srv := httptest.NewServer(g)
	defer srv.Close()

	type res struct {
		body string
		code int
		err  error
	}
	out := make(chan res, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/v1/messages")
		if err != nil {
			out <- res{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		out <- res{string(b), resp.StatusCode, nil}
	}()

	waitHeld(t, g, 1)
	select {
	case r := <-out:
		t.Fatalf("request returned before Ready: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}

	g.Ready(okHandler)
	select {
	case r := <-out:
		if r.err != nil || r.code != 200 || r.body != "served" {
			t.Fatalf("held request = %+v, want 200 served", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("held request not released by Ready")
	}
	if g.Held() != 0 || !g.IsReady() {
		t.Errorf("held=%d ready=%v after release", g.Held(), g.IsReady())
	}
	// Once ready, no hold at all.
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Body.String() != "served" {
		t.Errorf("post-ready request = %q", rec.Body.String())
	}
}

// A held request honours client cancellation, like exhaustion holds do.
func TestGateHeldRequestHonoursClientCancel(t *testing.T) {
	g := NewGate(quietGateLogger(), time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { g.ServeHTTP(rec, req); close(done) }()
	waitHeld(t, g, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled request still held")
	}
	if g.Held() != 0 {
		t.Errorf("held = %d after cancel", g.Held())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("wrote a response to a client that had gone: %q", rec.Body.String())
	}
}

// The hold is bounded: past it the client gets a retryable 503, not a hang.
func TestGateRefusesAfterHoldBudget(t *testing.T) {
	g := NewGate(quietGateLogger(), 50*time.Millisecond)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("got %d (Retry-After %q), want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestGateCloseDropsHeldRequests(t *testing.T) {
	g := NewGate(quietGateLogger(), time.Minute)
	done := make(chan struct{})
	go func() {
		g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		close(done)
	}()
	waitHeld(t, g, 1)
	g.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release the held request")
	}
}

func TestGateZeroHoldMaxUsesDefault(t *testing.T) {
	if g := NewGate(quietGateLogger(), 0); g.holdMax != DefaultStartupHold {
		t.Errorf("holdMax = %v, want %v", g.holdMax, DefaultStartupHold)
	}
}
