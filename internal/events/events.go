// Package events is a tiny pub/sub broker for pool/proxy events, consumed
// by the admin SSE stream.
package events

import (
	"sync"
	"time"
)

// Event is something the user should see: rotations, holds, exhaustion,
// credential recovery.
type Event struct {
	Ts      time.Time `json:"ts"`
	Type    string    `json:"type"`    // rotated-quota, rotated-conn, rate-retry, held, exhausted, recovered
	Account string    `json:"account"` // account involved ("" when pool-wide)
	Detail  string    `json:"detail"`  // human one-liner, no secrets
}

// Broker fans events out to subscribers.
type Broker struct {
	mu   sync.Mutex
	subs map[chan Event]bool
}

// New returns an empty Broker.
func New() *Broker { return &Broker{subs: map[chan Event]bool{}} }

// Publish sends an event to all subscribers (dropped for slow ones).
func (b *Broker) Publish(e Event) {
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Subscribe returns a buffered channel of future events.
func (b *Broker) Subscribe() chan Event {
	ch := make(chan Event, 32)
	b.mu.Lock()
	b.subs[ch] = true
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes and closes a subscription channel.
func (b *Broker) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	if b.subs[ch] {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}
