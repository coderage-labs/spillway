package pool

// Hold registry (design doc §6.11).
//
// When the pool is dry the proxy parks the request rather than failing it.
// From the client's side that is indistinguishable from a hang: Claude Code
// shows a spinner and nothing else, for what may be hours. The registry
// exists so the hold is *observable* — the status line and the dashboard can
// say "parked until 14:30" instead of leaving the user to guess.
//
// It lives on the Pool because the Pool is the object the proxy and the admin
// server already share, and because a hold is a statement about the pool: it
// has nothing to give until then.

import "time"

// hold is one parked request.
type hold struct {
	until time.Time
}

// BeginHold records a request parked until `until`, and returns the function
// that ends it. The returned func is idempotent, so a deferred release is
// safe even on paths that release early.
func (p *Pool) BeginHold(until time.Time) func() {
	h := &hold{until: until}
	p.mu.Lock()
	if p.holds == nil {
		p.holds = map[*hold]struct{}{}
	}
	p.holds[h] = struct{}{}
	p.mu.Unlock()

	var done bool
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if done {
			return
		}
		done = true
		delete(p.holds, h)
	}
}

// Holds reports how many requests are parked and when the first of them is
// due to resume. The count is what matters for "is anything stuck"; the time
// is what makes it actionable.
func (p *Pool) Holds() (n int, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for h := range p.holds {
		if until.IsZero() || h.until.Before(until) {
			until = h.until
		}
		n++
	}
	return n, until
}
