package proxy

// Startup gate (issue #259).
//
// The daemon used to bind its listeners only after secrets, the request log
// and the pool were all initialised — about two minutes after login on a
// loaded machine, during which every client got "connection refused". A
// terminal that resumes Claude sessions on launch failed against a closed
// port.
//
// Gate is the handler the proxy listener serves from the instant it binds.
// Until Ready installs the real Handler it HOLDS every request, and releases
// them all when init finishes. It is deliberately independent of the pool:
// the pool's own park machinery (hold.go, pool.SignalCapacity) cannot exist
// before the pool does, and what a request needs here is the same contract —
// wait, bounded, and give up the moment the client does.
//
// Held, not routed, for every request including CONNECT and non-pooled
// hosts: which hosts are MITM'd and which are blind-tunnelled depends on
// the account set, so answering any of them early could send a pooled host's
// traffic out on the client's own credential. The real Handler (and the CA
// it carries) is installed as the last step of init, so passthrough traffic
// is served the moment the CA is ready.

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultStartupHold is how long a request is held when the configured
// pool.holdMax is 0 (disabled). Startup is not pool exhaustion, so a user who
// turned exhaustion-holding off still wants a start-up request to wait rather
// than be refused.
const DefaultStartupHold = 5 * time.Minute

// Gate serves held requests until the real handler is installed.
type Gate struct {
	logger  *slog.Logger
	holdMax time.Duration

	ready     chan struct{}
	closing   chan struct{}
	readyOnce sync.Once
	closeOnce sync.Once
	next      atomic.Pointer[http.Handler]
	held      atomic.Int64
}

// NewGate returns a gate that holds each request for at most holdMax (0 means
// DefaultStartupHold).
func NewGate(logger *slog.Logger, holdMax time.Duration) *Gate {
	if holdMax <= 0 {
		holdMax = DefaultStartupHold
	}
	return &Gate{
		logger: logger, holdMax: holdMax,
		ready: make(chan struct{}), closing: make(chan struct{}),
	}
}

// Ready installs h and releases every held request. Only the first call has
// any effect.
func (g *Gate) Ready(h http.Handler) {
	g.readyOnce.Do(func() {
		g.next.Store(&h)
		if n := g.held.Load(); n > 0 {
			g.logger.Info("startup complete — releasing held requests", "held", n)
		}
		close(g.ready)
	})
}

// Close abandons init: held requests are dropped (used when init fails or the
// daemon is shutting down, so Shutdown is not left waiting on them).
func (g *Gate) Close() { g.closeOnce.Do(func() { close(g.closing) }) }

// Held reports how many requests are waiting right now.
func (g *Gate) Held() int { return int(g.held.Load()) }

// IsReady reports whether the real handler is installed.
func (g *Gate) IsReady() bool { return g.next.Load() != nil }

func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := g.next.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	if !g.hold(r.Context()) {
		if r.Context().Err() == nil {
			g.writeUnavailable(w)
		}
		return
	}
	(*g.next.Load()).ServeHTTP(w, r)
}

// hold parks until ready. It returns false on timeout, client cancellation or
// Close.
func (g *Gate) hold(ctx context.Context) bool {
	g.held.Add(1)
	defer g.held.Add(-1)
	t := time.NewTimer(g.holdMax)
	defer t.Stop()
	select {
	case <-g.ready:
		return true
	case <-ctx.Done():
		return false
	case <-g.closing:
		return false
	case <-t.C:
		g.logger.Warn("request held through the whole startup hold budget and is being refused",
			"holdMax", g.holdMax)
		return false
	}
}

func (g *Gate) writeUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(5))
	http.Error(w, "spillway: still starting — retry shortly", http.StatusServiceUnavailable)
}
