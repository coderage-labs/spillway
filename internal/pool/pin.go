package pool

// Pinning the pool to one account (#11).
//
// Two reasons it exists. Rotation is otherwise only observable by spending an
// account's quota, which makes it awkward to test and impossible to
// demonstrate. And a user may simply want a piece of work kept on one
// subscription, or steered off an account they are about to need elsewhere.
//
// Pool-wide rather than per-session, because the sticky key already is:
// sessionKey hashes metadata.user_id, which is the same for every Claude Code
// session on the machine. A per-session pin would need an identifier no caller
// has, and today would not behave any differently.

import (
	"errors"
	"fmt"
)

// ErrPinWouldBill and ErrPinCrossProvider are the two refusals a caller is
// expected to handle rather than pass on: both have a --force.
var (
	ErrPinWouldBill     = errors.New("pinning there would spend money")
	ErrPinCrossProvider = errors.New("pinning there changes provider mid-session")
)

// Pin directs selection at one account by name until Unpin.
//
// Refused in two cases, each of which force overrides:
//
// Money. An account whose only remaining path is paid extra usage is exactly
// what the three-tier selector reaches for last and only with permission;
// naming it directly must not be the cheap way around that.
//
// Provider. A session is pinned to the provider it started on (§6.18) because
// the client configured its capabilities from the first model it saw, so
// moving a live session from Claude to Kimi hands it a model it did not
// negotiate. Forcing clears that pin — deliberately, and only when asked.
func (p *Pool) Pin(name string, force bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	a := p.find(name)
	if a == nil {
		return fmt.Errorf("no account named %q", name)
	}
	if a.disabledOrParked() {
		if a.Parked() {
			return fmt.Errorf("account %q is parked", name)
		}
		return fmt.Errorf("account %q is disabled", name)
	}
	if !force && p.wouldBill(a) {
		return fmt.Errorf("%w: %q is out of quota and would serve from paid extra usage", ErrPinWouldBill, name)
	}
	if !force && !p.CrossProvider {
		want := ProviderOf(a.Type)
		for _, started := range p.sessionProvider {
			if started != want {
				return fmt.Errorf("%w: sessions started on %s, %q is %s",
					ErrPinCrossProvider, started, name, want)
			}
		}
	}

	p.pinned = name
	if force {
		// The session-provider pin is what would otherwise make this a no-op:
		// selection skips any account of another provider, so a forced
		// cross-provider pin has to drop it or it would sit there doing
		// nothing while the pool carried on as before.
		clear(p.sessionProvider)
	}
	return nil
}

// Unpin returns the pool to ordinary selection.
func (p *Pool) Unpin() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pinned = ""
}

// Pinned is the pinned account's name, or "".
func (p *Pool) Pinned() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pinned
}

// pinnedChoice is the pinned account when it can serve this request, or nil.
// Caller holds p.mu.
//
// Deliberately ignores the rotate-away threshold: naming an account is a
// statement that you want it, and the threshold exists to make an automatic
// choice, not to overrule an explicit one. It does not ignore exhaustion —
// holding every request while healthy accounts sit idle would turn a
// convenience into a way to take yourself offline by accident.
func (p *Pool) pinnedChoice(usable func(*Account) bool) *Account {
	if p.pinned == "" {
		return nil
	}
	a := p.find(p.pinned)
	if a == nil || !a.eligible() || !usable(a) {
		return nil
	}
	return a
}
