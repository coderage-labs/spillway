package pool

// Capacity-changed signal (issue #105).
//
// A held request (internal/proxy/hold.go's park) used to wait on exactly
// one thing: a timer set to fire at the pool's earliest known reset. That
// answers "has a moment in time arrived", never "does capacity exist now" —
// so a pool gaining a usable account mid-hold (one added live, one
// re-probed back to health, one un-parked, one re-authenticated) was
// invisible to every request already parked, even though selection would
// now succeed. This file is the broadcast half of the fix; park is the
// wait half.
//
// Signalling is deliberately narrow: SignalCapacity is called ONLY on a
// transition INTO potentially-usable capacity — Add, ClearExhausted
// actually clearing an exhaustion, Apply actually un-parking an account,
// and EnsureFresh/Recover actually reviving a credential from
// StateDisabled. A signal firing on every quota-header write (RecordQuota,
// MarkExhausted, MarkWindowRejected) would wake every held request
// constantly for nothing — that busy-wake is avoided by construction:
// nothing on those paths calls into this file.

import "sync/atomic"

// CapacityWake is one generation of the capacity-changed broadcast. Ch()
// closes exactly once, the instant this generation's transition fires; a
// new CapacityWake (with a fresh, open channel) replaces it atomically
// under the pool's lock, so a waiter that reads Ch() once and blocks on it
// never misses a wake and never sees the same one twice.
type CapacityWake struct {
	ch chan struct{}
	// seq hands out sequential positions to Rank callers for THIS
	// generation only — a fresh CapacityWake starts back at zero — so many
	// requests woken by the same close can spread their re-select over a
	// short window instead of firing in lockstep (issue #91's herd,
	// retriggered on a faster clock, is exactly the failure this avoids).
	seq atomic.Int64
}

func newCapacityWake() *CapacityWake {
	return &CapacityWake{ch: make(chan struct{})}
}

// Ch returns the channel that closes when this generation wakes.
func (w *CapacityWake) Ch() <-chan struct{} { return w.ch }

// Rank returns this caller's 0-based position among everyone who has called
// Rank on this same generation so far (first caller: 0, second: 1, ...).
// Safe for concurrent callers — that is the point: it is how many
// simultaneously-parked requests agree on a stagger order with no
// additional coordination.
func (w *CapacityWake) Rank() int64 {
	return w.seq.Add(1) - 1
}

// CapacitySignal returns the pool's current capacity-changed generation. A
// caller selects on Ch() alongside its own timer; once Ch() closes, the
// generation is spent — call CapacitySignal again to get the next one
// before waiting again, rather than reusing this one, which will never
// close a second time.
func (p *Pool) CapacitySignal() *CapacityWake {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capGen
}

// SignalCapacity closes the current generation's channel — waking every
// request parked on it — and installs a fresh generation for the next
// round. Call ONLY on a transition INTO potentially-usable capacity; see
// the package-level comment above for why that boundary matters and where
// it is already drawn (Add, ClearExhausted, Apply, EnsureFresh, Recover).
func (p *Pool) SignalCapacity() {
	p.mu.Lock()
	p.signalCapacityLocked()
	p.mu.Unlock()
}

// signalCapacityLocked is SignalCapacity for callers that already hold mu
// (Add appends under mu and wants to signal before releasing it).
func (p *Pool) signalCapacityLocked() {
	close(p.capGen.ch)
	p.capGen = newCapacityWake()
}
