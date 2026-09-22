package admin

// POST /api/accounts/probe — issue #192: force one quota probe of a named
// account right now, bypassing the two schedules that would otherwise
// suppress it (#90's re-probe backoff and #152's billed-probe rationing).
//
// Shaped like /api/pin rather than like /api/settings. It is a live
// instruction, not a setting: it touches no config file, survives no
// restart, and is available whether or not settings editing is enabled. The
// only thing it needs wiring for is the probe itself (EnableProbe), because
// sending one means acting as a client — which is internal/accounts' job,
// handed over as a closure for the same reason SetCAWarning and SetUnpooled
// are closures: this package does not import the packages that do the work.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/coderage-labs/spillway/internal/pool"
)

type accountProbeRequest struct {
	Name string `json:"name"`
	// Force acknowledges a charge. Only meaningful on an account whose
	// probe would be billed; everywhere else it changes nothing, because
	// nothing else on this path refuses.
	Force bool `json:"force"`
}

type accountProbeResponse struct {
	Account string `json:"account"`
	// Billed says this probe cost money — true only when Force was given
	// and the account's quota was gone with extra usage permitted. Reported
	// rather than inferred by the caller: the dashboard and `spillway
	// probe` both say so out loud, and the alternative is a surface
	// re-deriving the money rule from account JSON and getting it wrong.
	Billed bool `json:"billed,omitempty"`
	// Windows is the reading the probe just took, so the caller can show the
	// answer without a second round trip to /api/accounts.
	Windows []pool.QuotaWindow `json:"quotaWindows,omitempty"`
}

// EnableProbe wires the forced-probe action. f sends one probe to the named
// account and reports whether it was billed; a refusal it expects the caller
// to handle is wrapped around pool.ErrProbeWouldBill. Left nil in tests that
// only exercise the read-only API, and in any build with no probe client to
// hand — which answers 404 rather than pretending to have probed.
func (s *Server) EnableProbe(f func(ctx context.Context, name string, force bool) (bool, error)) {
	s.probe = f
}

func (s *Server) handleAccountProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.probe == nil {
		http.Error(w, "probing is not enabled", http.StatusNotFound)
		return
	}
	var req accountProbeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "malformed body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	// Resolved against the live pool before anything is sent, so an unknown
	// name is a 404 and never a probe. Deliberately exact-match, like
	// /api/pin: the CLI resolves a label or prefix to a real name at the
	// edge, and a second, differently-shaped matcher in here is how two
	// surfaces come to disagree about which account a user meant.
	var acct *pool.Account
	for _, a := range s.pool.Accounts() {
		if a.Name == req.Name {
			acct = a
			break
		}
	}
	if acct == nil {
		http.Error(w, "no account named "+req.Name, http.StatusNotFound)
		return
	}
	// 400, not 409, and the distinction is load-bearing: this endpoint keeps
	// /api/pin's rule that 409 means "force answers this and 400 does not",
	// because both surfaces render a 409 as a retry offer. A "probe anyway"
	// button on a parked account would be a button that cannot work. Checked
	// here rather than only inside the probe because this is the layer that
	// can see the account's state and say which of the two it was.
	if acct.Parked() {
		http.Error(w, "account "+req.Name+" is parked", http.StatusBadRequest)
		return
	}
	if acct.State() == pool.StateDisabled {
		http.Error(w, "account "+req.Name+" is disabled", http.StatusBadRequest)
		return
	}

	billed, err := s.probe(r.Context(), req.Name, req.Force)
	if err != nil {
		// 409 for the money refusal, exactly as /api/pin maps
		// ErrPinWouldBill: the request is well formed and the answer may be
		// different if the caller means it, so a surface can offer the
		// choice. A 400 invites it to give up, and a 500 makes a deliberate
		// refusal look like a fault.
		//
		// Anything else is a probe that was actually attempted and did not
		// complete — an upstream timeout, a dead credential, a provider
		// error — which is 502, not the caller's fault. No "spillway: "
		// prefix: the CLI relays this text verbatim under its own.
		code := http.StatusBadGateway
		if errors.Is(err, pool.ErrProbeWouldBill) {
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
		return
	}
	s.writeJSON(w, accountProbeResponse{
		Account: req.Name,
		Billed:  billed,
		Windows: acct.QuotaWindows(),
	})
}
