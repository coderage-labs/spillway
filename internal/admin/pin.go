package admin

// POST/DELETE /api/pin — direct the pool at one account, or release it (#11).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/coderage-labs/spillway/internal/pool"
)

type pinRequest struct {
	Account string `json:"account"`
	Force   bool   `json:"force"`
}

type pinResponse struct {
	Pinned string `json:"pinned"`
	// Warning carries what the caller should know but was not stopped by:
	// switching costs the prompt cache, which is the reason sticky selection
	// exists at all.
	Warning string `json:"warning,omitempty"`
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		s.pool.Unpin()
		s.writeJSON(w, pinResponse{})
	case http.MethodPost:
		var req pinRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "malformed body", http.StatusBadRequest)
			return
		}
		if req.Account == "" {
			s.pool.Unpin()
			s.writeJSON(w, pinResponse{})
			return
		}
		if err := s.pool.Pin(req.Account, req.Force); err != nil {
			// 409 rather than 400 for the two that force overrides: the
			// request is well formed and the answer may be different if the
			// caller means it. A dashboard can offer the choice; a 400
			// invites it to give up.
			code := http.StatusBadRequest
			if errors.Is(err, pool.ErrPinWouldBill) || errors.Is(err, pool.ErrPinCrossProvider) {
				code = http.StatusConflict
			}
			// No "spillway: " prefix: this text is relayed verbatim by the
			// CLI, which is itself printed by main under that prefix, and
			// two of them read as a stutter.
			http.Error(w, err.Error(), code)
			return
		}
		s.writeJSON(w, pinResponse{
			Pinned:  s.pool.Pinned(),
			Warning: "prompt cache is per account, so the next request will miss it",
		})
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
