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
	// Reserve is eligible but over the rotate-away threshold for the general
	// windows (5h/7d — issue #24): its quota headers say it is finished for
	// ordinary Sonnet/Opus/Haiku traffic, so it is used only when nothing
	// better exists. Counting these as usable made an account spillway was
	// actively avoiding render as healthy. An account spent only on its
	// fable bucket is NOT counted here — it is fully usable for the traffic
	// this aggregate, model-agnostic view is about.
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
	// Pinned names the account selection has been directed at, if any. In
	// state rather than only in the pin endpoint's own response: a reader
	// that does not know about the pin would otherwise report a pool
	// "choosing" an account it was told to use (#11).
	Pinned string `json:"pinned,omitempty"`
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
	// StaleCA is true once a genuine MITM CA regeneration this run
	// performed has left at least one client looking stuck trusting the
	// old anchor (issue #66) — see internal/proxy's stranded-client
	// detector, wired in via SetCAWarning. Always false when nothing
	// wired it up (e.g. base-URL-only setups, or tests that don't
	// exercise MITM), and it decays back to false on its own once the
	// symptom stops recurring — it is not meant to latch forever.
	StaleCA bool `json:"staleCA,omitempty"`
	// Unpooled reports traffic spillway passed through WITHOUT a pooled
	// credential because it did not recognise the path as inference (issue
	// #176). Present only once something has actually been passed through
	// unpooled, like Holding above.
	//
	// This is the visible half of inverting the routing default. spillway
	// now pools only what it recognises, which means a new inference
	// endpoint would be forwarded on the user's own credential and the pool
	// would quietly go unused for it. The daemon warns about that in the
	// log; this is the same fact somewhere a reader looks without tailing a
	// log file. Silence is what let both of the bugs that motivated the
	// inversion run for thousands of requests.
	Unpooled *unpooledJSON `json:"unpooled,omitempty"`
}

// unpooledJSON is the /api/state view of issue #176's unrecognised-path
// counter. Declared here rather than reusing internal/proxy's type because
// admin does not import proxy — main wires the two together, exactly as it
// does for the stale-CA warning.
type unpooledJSON struct {
	// Requests is every request passed through on the client's own
	// credential because spillway did not recognise the path. Identity and
	// confirmed-non-quota paths are excluded: those are recognised.
	Requests int `json:"requests"`
	// Paths is how many distinct path templates those fell on. Templates,
	// not paths: identifiers are redacted before anything is recorded.
	Paths int `json:"paths"`
	// InferenceShaped names the templates that took a POST with a body and
	// therefore raised the warning — the list to check against
	// proxy.inferencePaths.
	InferenceShaped []string `json:"inferenceShaped,omitempty"`
}

func (s *Server) state() stateJSON {
	var st stateJSON
	st.Pinned = s.pool.Pinned()
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
		case a.OverThresholdFor("", s.pool.Threshold()):
			st.Reserve++
		default:
			st.Usable++
		}
	}
	st.Threshold = s.pool.Threshold()
	if n, until := s.pool.Holds(); n > 0 {
		st.Holding = &holdJSON{Count: n, Until: until}
	}
	if s.caWarning != nil {
		st.StaleCA = s.caWarning()
	}
	// Read fresh on every request, never cached, for the same reason
	// caWarning is: the whole value of this figure is that it is current.
	if s.unpooled != nil {
		if u := s.unpooled(); u.Requests > 0 {
			st.Unpooled = &u
		}
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
