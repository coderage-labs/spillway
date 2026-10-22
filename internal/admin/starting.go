package admin

// Startup handler (issue #259): the admin listener binds before the pool,
// request log and secrets exist, so it needs something to serve with until
// the real Server does. Starting answers /api/state with status "starting"
// and every other path with 503, then forwards everything to the real
// handler once Ready installs it.

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
)

// StatusStarting and StatusReady are the values of /api/state's "status".
const (
	StatusStarting = "starting"
	StatusReady    = "ready"
)

// Starting is an http.Handler that stands in for the admin Server until it
// can be built.
type Starting struct {
	next atomic.Pointer[http.Handler]
}

// NewStarting returns a handler in the starting state.
func NewStarting() *Starting { return &Starting{} }

// Ready installs the real handler; every later request goes to it.
func (s *Starting) Ready(h http.Handler) { s.next.Store(&h) }

func (s *Starting) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := s.next.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	// Same DNS-rebinding guard the real Server applies first.
	if !loopbackHost(r.Host) {
		http.Error(w, "forbidden: bad host", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/api/state" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stateJSON{Status: StatusStarting})
		return
	}
	w.Header().Set("Retry-After", "5")
	http.Error(w, "spillway: still starting — retry shortly", http.StatusServiceUnavailable)
}
