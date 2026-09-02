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
	"time"
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
// Money, asked twice. An account whose only remaining path is paid extra
// usage is exactly what the three-tier selector reaches for last and only
// with permission; naming it directly must not be the cheap way around that
// (p.wouldBill). And separately, an account the PROVIDER will bill even
// though spillway would not have chosen to — see providerWouldBill, and
// issue #139 for how the first check alone let that through.
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
	if !force && providerWouldBill(a) {
		// Names the provider-side cause on purpose. "would spend money" on
		// an account whose overage spillway has switched off reads as a bug
		// unless the message says whose decision it actually is, and a
		// refusal the reader cannot explain is a support ticket. The
		// --force offer is added by the surfaces that have one: the CLI
		// appends the hint to a 409, the dashboard renders this text with a
		// "pin anyway" button beside it.
		return fmt.Errorf("%w: %q has no quota left and %s has extra usage enabled on it, so the provider bills a pinned request even with spillway's own overage setting off",
			ErrPinWouldBill, name, ProviderOf(a.Type))
	}
	// p.crossProvider, not the CrossProvider() accessor: Pin already holds
	// p.mu and the accessor takes it again. SelectExcept reads the field the
	// same way for the same reason (#13 unexported these).
	if !force && !p.crossProvider {
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

// providerWouldBill reports that the PROVIDER would charge for the next
// request on a. It is the question p.wouldBill does not ask, and the reason
// a pin needs both (issue #139).
//
// wouldBill answers "would spillway CHOOSE to bill here?" — its first line
// is CanOverage, which is false whenever spillway has not been given
// permission to spend. That is the right question for selection, where
// spillway is the one choosing: tier 3 of SelectExcept is the only charged
// tier and it is entered only with permission, so no permission means no
// charge. It is the wrong question for a pin. A pin bypasses selection
// entirely — pinnedChoice ignores the threshold and every tier — so once
// the account is named, whether the request bills is the provider's
// decision, not spillway's. With `overage-status: allowed` upstream the next
// pinned request is served on extra usage and charged while allowOverage
// sits off, and spillway finds out afterwards from the overage-in-use
// header. Takes no *Pool because it must not consult p.allowOverage: that
// field is exactly the thing that was answering for the provider.
//
// Deliberately NOT the rotate-away threshold, which issue #139 proposed and
// which wouldBill uses. A pin exists to override that threshold
// (pinnedChoice; TestPinIgnoresTheRotateAwayThreshold), and an account
// merely over it still has free quota left — the next request is served from
// the subscription and costs nothing. switchThreshold is also a user
// setting: at 0.5 a threshold-based rule would start refusing pins on
// accounts sitting at 60% with hours of headroom, which is over-refusing on
// the money path and teaches --force as a reflex. The money question is
// narrower: is the quota gone. So the fraction is 1 — a window at or past
// its own limit, or a confirmed quota rejection (StateExhausted).
//
// Any window, not just the account-wide ones. A spent 7d-fable beside an
// idle 5h is exactly the account issue #135 measured, and a fable request on
// that pin bills; a pin is set before any model is known, so narrowing to
// the account-wide windows would under-refuse the case that was reported.
//
// A window upstream has already REJECTED is the one exclusion, and it is the
// same reasoning as Overage.Available being false: a rejection means the
// next request is refused, not served, so there is no money at stake. It
// needs excluding rather than ignoring because MarkWindowRejected forges a
// spent row (utilization 1.0) for the dashboard's benefit, and because
// SelectExcept's usable() already drops the pinned account for the family
// that window governs (issue #54) — so a fable request never reaches it to
// be billed while Sonnet, governed only by the account-wide windows, keeps
// serving free from the same pin. The asymmetry is deliberate: a spent
// HEADER reading refuses, a rejected window does not.
//
// A spent window whose own reset has passed no longer counts (issue #135,
// QuotaWindow.currentAt): a reading that has expired is not evidence that
// money is at stake, and refusing on a number nobody has re-measured would
// mirror the bug #135 fixed.
//
// An UNKNOWN overage reading (Overage().Known false) does not refuse. Three
// reasons, the first decisive:
//
//   - The refusal would be unactionable. The only way to make the reading
//     known is to send a request on the account, and the only way to send
//     one on a refused pin is --force. A guard whose sole remedy is the
//     override it exists to gate teaches --force as a habit and stops
//     meaning anything on the accounts where the risk is real.
//   - It is the rule the rest of the money path already follows.
//     Account.Overage's own contract is that an account which has never been
//     used "tells us nothing, and must not be assumed billable", and
//     CanOverage requires a confirmed header before honouring a pool-wide
//     yes.
//   - `overage` reads null on plenty of real accounts — it does on the
//     accounts issue #139 was reported from — so refusing on unknown would
//     refuse the common case instead of the dangerous one.
//
// Fail-closed on money here means "spillway never chooses to spend", not
// "spillway refuses everything it cannot see". The residual risk is real and
// bounded: a pinned request on a spent account with no overage reading may
// be billed once, after which the header makes the state known and every
// later pin is refused.
func providerWouldBill(a *Account) bool {
	ov := a.Overage()
	if !ov.Known || !ov.Available {
		return false
	}
	return a.State() == StateExhausted || a.spentBeyondRejections(time.Now())
}

// spentBeyondRejections reports a current quota reading at or past its own
// limit for a window upstream has NOT rejected — quota the provider will
// serve on extra usage rather than refuse. See providerWouldBill for why
// each of the three skips is there. The clock is injected for the same
// reason overThresholdAt's is; like it, this takes a.mu itself.
func (a *Account) spentBeyondRejections(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, w := range a.windows {
		if !w.currentAt(now) {
			continue
		}
		// until.After(now), matching WindowRejectedFor's deadline exactly:
		// a rejection that has lapsed is not a reason to trust the forged
		// row it left behind.
		if until, ok := a.windowRejected[w.Name]; ok && until.After(now) {
			continue
		}
		if w.Limit > 0 && w.Used/w.Limit >= 1 {
			return true
		}
	}
	return false
}
