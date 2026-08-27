package admin

// POST /api/accounts/remove — issue #83: take an account out of the running
// pool immediately, so a credential the CLI has just deleted from the
// secret store can never be selected for a request. Always available (like
// /api/pin), not gated behind EnableSettings: it never touches the config
// file — the CLI already did that — it only tells the live pool to forget
// the name.

import (
	"encoding/json"
	"io"
	"net/http"
)

type accountRemoveRequest struct {
	Name string `json:"name"`
}

type accountRemoveResponse struct {
	// Removed is true when the name was actually present in the pool.
	// false is not an error: the CLI calls this once per `accounts remove`
	// regardless of whether a daemon is running or already agrees, and "it
	// was already gone" is a fine outcome — see handleAccountRemove.
	Removed bool `json:"removed"`
}

// handleAccountRemove is deliberately idempotent and never reports a missing
// name as an error: the caller's goal is "this name must not be selectable",
// which already holds whether Remove found it or not. The only error
// response is a malformed request — a name spillway cannot even make sense
// of, as opposed to one it dutifully forgot only for it not to exist.
func (s *Server) handleAccountRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req accountRemoveRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "malformed body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	removed := s.pool.Remove(req.Name)
	s.writeJSON(w, accountRemoveResponse{Removed: removed})
}
