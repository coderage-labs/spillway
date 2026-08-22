package admin

// /api/state is the pool-level view: the things that are true of the whole
// pool rather than of one account.
//
// It exists for the status line. Per-account data was already available, but
// the two signals that matter most when something is wrong — "a request is
// parked" and "how much of the pool is still usable" — are not properties of
// any single account, and deriving them client-side would let the CLI and the
// dashboard disagree about the same daemon.

import (
	"net/http"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

// holdJSON describes requests parked waiting for a reset.
type holdJSON struct {
	Count int       `json:"count"`
	Until time.Time `json:"until"`
}

// stateJSON is the /api/state response.
type stateJSON struct {
	// Holding is present only while something is actually parked.
	Holding *holdJSON `json:"holding,omitempty"`
	// Usable is the number of accounts the selector will pick from
	// preferentially; Total counts every configured account.
	Usable int `json:"usable"`
	Total  int `json:"total"`
	// Reserve is eligible but over the rotate-away threshold: its quota
	// headers say it is finished, so it is used only when nothing better
	// exists. Counting these as usable made an account spillway was actively
	// avoiding render as healthy.
	Reserve int `json:"reserve"`
	// Exhausted, Parked and Disabled break down the rest. Disabled is the
	// one that needs a human: the credential is gone.
	Exhausted int `json:"exhausted"`
	Parked    int `json:"parked"`
	Disabled  int `json:"disabled"`
	// InFlight is the total across the pool.
	InFlight int `json:"inFlight"`
	// Overage counts exhausted accounts that can still serve, at cost,
	// because extra usage is available and permitted. They are not usable
	// and not dry: they are the tier reached instead of holding.
	Overage int `json:"overage"`
	// Threshold is the used-fraction the selector rotates away at. Served so
	// the dashboard draws its spill line where the pool actually spills,
	// rather than at a constant that silently diverges once the setting is
	// changed.
	Threshold float64 `json:"threshold"`
	// NextReset is the soonest quota reset in the pool, present only when
	// nothing at all is left — that is when it becomes the answer to "when
	// can I work again". A reserve account still serves, so it does not
	// count as dry.
	NextReset *time.Time `json:"nextReset,omitempty"`
}

func (s *Server) state() stateJSON {
	var st stateJSON
	for _, a := range s.pool.Accounts() {
		st.Total++
		st.InFlight += a.InFlight()
		switch {
		case a.Parked():
			st.Parked++
		case a.State() == pool.StateDisabled:
			st.Disabled++
		// Billing beats both of the categories below it. An account can
		// reach the paid tier from either — 429'd, or spent by its own
		// headers without ever being rejected — and which one it was matters
		// far less than the fact that using it costs money. Deriving this
		// from StateExhausted alone missed the second and commoner case.
		case s.pool.WouldBill(a):
			st.Overage++
		case a.State() == pool.StateExhausted:
			st.Exhausted++
		case a.OverThreshold(s.pool.Threshold()):
			st.Reserve++
		default:
			st.Usable++
		}
	}
	st.Threshold = s.pool.Threshold()
	if n, until := s.pool.Holds(); n > 0 {
		st.Holding = &holdJSON{Count: n, Until: until}
	}
	if st.Usable == 0 && st.Reserve == 0 && st.Overage == 0 {
		if reset, ok := s.pool.EarliestReset(); ok {
			st.NextReset = &reset
		}
	}
	return st
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, s.state())
}
