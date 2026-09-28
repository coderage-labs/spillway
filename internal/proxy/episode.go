package proxy

// Exhaustion/recovery notification episodes (issue #230).
//
// Before this, every hold called Notify(EventHeld, "pool-held", …) and
// every final refusal called Notify(EventExhausted, "pool-exhausted-all",
// …) — two keys, each coalesced independently by notify.go's flat
// 10-minute window. A genuine multi-hour exhaustion re-opened each key
// every 10 minutes for as long as it lasted (2026-09-28: on the order of
// 10-20 pings saying the same thing), and the message named "the pool" even
// when only the fable family was actually out — Sonnet/Opus kept serving
// the whole time — which told the owner to stop work that was in fact
// still running.
//
// An episode's boundary is pool STATE — pool.FamilyHasCapacity, the exact
// tier-1/2 predicate SelectExcept itself uses — never a timer: a
// timer-bounded "episode" is exactly the original bug under a different
// name, since a long exhaustion would just reopen a "new" one every
// coalesce interval.
//
// Two kinds of episode, tracked in episodeTracker below:
//   - a family episode (e.g. fable's "7d-fable"): open whenever that
//     family lacks capacity while the general windows do not — "Fable
//     held, other models running".
//   - the general ("all models") episode: open whenever the general
//     windows lack capacity. By construction (pool.FamilyHasCapacity's own
//     comment) a family can never have capacity while the general windows
//     don't, so the general episode opening while a family episode is
//     already open IS the escalation the issue asks for, and needs no
//     separate flag — reportExhaustion below simply checks the general
//     predicate first and returns before ever touching the family one.
//
// Recovery is the one half of this that must NOT depend on a request
// arriving at all (issue #230: "recovery fires on capacity returning …
// even with zero held requests outstanding" — a served-only trigger would
// never announce anything to an owner who stopped retrying). That's
// checkRecovery, driven by WatchRecovery's background loop rather than the
// request path — see that function's own comment for why a capacity signal
// plus each open episode's own reset time, not a poll, is what wakes it.
import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/coderage-labs/spillway/internal/notify"
)

// etaSlipThreshold is how much later a re-computed ETA has to land, versus
// the one last notified for this episode, before the owner is told again
// (issue #230's "re-notify once" rule). Moving earlier, or later by less
// than this, is not worth a second ping on its own — see
// shouldNotifyLocked's holdMax check for the other trigger.
const etaSlipThreshold = 15 * time.Minute

// episodeState is one episode's notification memory: whether it is
// currently open, and the ETA it was last notified with (etaKnown false
// when that ETA is itself unknown — "every blocking account is disabled"
// territory, same as EarliestReset's own ok=false).
type episodeState struct {
	open     bool
	eta      time.Time
	etaKnown bool
}

// episodeTracker holds every open episode's notification state (issue
// #230): one for the general ("all models") episode, one per family that
// has ever been reported down. Guarded by its own mutex because it is
// touched from the request path (reportExhaustion, on every hold/refusal)
// and from the background recovery watcher (checkRecovery) concurrently.
type episodeTracker struct {
	mu       sync.Mutex
	general  episodeState
	families map[string]episodeState
}

func newEpisodeTracker() *episodeTracker {
	return &episodeTracker{families: map[string]episodeState{}}
}

// shouldNotifyLocked decides whether s's episode needs a notification for
// eta/etaKnown, and updates s to match when it does. Shared by the general
// and every family episode — one rule, not two copies that could drift.
//
//   - Not open yet: opening is always worth a notification (issue #230's
//     "episode start"), whatever the ETA is (even unknown).
//   - Already open: only a MATERIAL slip re-notifies — more than
//     etaSlipThreshold later than what was last notified, or now past
//     holdMax (the point at which a freshly-starting hold would fail fast
//     rather than wait, per hold.go's own reset-vs-deadline check — a
//     slip that crosses that line is a materially different situation
//     even when it's under the flat threshold). An unknown ETA never
//     counts as a slip in either direction — there's nothing to compare —
//     and neither does one moving EARLIER: that's good news the recovery
//     message covers, not a reason to ping again.
//
// eta is stored only on a transition this function reports true for, never
// on a decrease: the next slip must be measured against what the owner was
// actually told, not silently re-baselined by an improvement nobody heard
// about.
func shouldNotifyLocked(holdMax time.Duration, s *episodeState, eta time.Time, etaKnown bool) bool {
	if !s.open {
		s.open, s.eta, s.etaKnown = true, eta, etaKnown
		return true
	}
	if !etaKnown || !s.etaKnown {
		return false
	}
	if eta.Sub(s.eta) > etaSlipThreshold || eta.After(time.Now().Add(holdMax)) {
		s.eta = eta
		return true
	}
	return false
}

// reportExhaustion is called from the request path — hold.go's
// waitForReset right before it actually parks, and proxy.go's final
// refusal — whenever a request finds that its own family currently has no
// capacity. eta/etaKnown is the SAME reset that request is already
// waiting on (or already failing on), computed once by the caller via the
// existing #140/#229 logic, never re-derived here: the notification must
// never disagree with what the request is actually doing.
//
// family is the request's own FamilyKey. The general predicate is
// re-checked fresh on every call rather than trusted from the caller,
// which protects against one specific staleness: SelectExcept's `skip`
// (already-tried) set can make a single request fail even while the pool
// as a whole still has capacity for its family, and re-deriving here
// (ignoring skip, exactly as FamilyHasCapacity does) means a retry
// artifact can never manufacture a false "held" notification.
//
// No-op unless this is a genuine transition for the relevant episode —
// see shouldNotifyLocked. Any number of requests holding, retrying, or
// waking and re-holding inside the same episode call this repeatedly and
// get exactly one notification for it.
func (h *Handler) reportExhaustion(family string, eta time.Time, etaKnown bool) {
	if h.notifier == nil {
		return
	}
	t := h.episodes
	t.mu.Lock()
	defer t.mu.Unlock()

	if !h.pool.FamilyHasCapacity("") {
		if shouldNotifyLocked(h.holdMax, &t.general, eta, etaKnown) {
			body := allHeldBody(eta, etaKnown)
			// Carried over from the pre-#230 exhaustedMessage: extra usage
			// being refused is a separate, actionable fact from the hold
			// itself (issue #151), and the "all models" episode is exactly
			// the scope that fact applies to — never named on a
			// single-family message, which says nothing about the whole
			// pool's overage state.
			if note := overageRefusalNote(h.pool); note != "" {
				body += "; " + note
			}
			h.notifier.NotifyEpisode(notify.EventExhausted, "spillway: all models held", body)
		}
		// Track this family as affected even though its OWN message is
		// suppressed while the general episode covers it — checkRecovery
		// needs to know to name it if it is still down once the general
		// windows recover (issue #230's "other models back — fable still
		// held"), and this is the only place that ever learns a family was
		// actually affected without a dedicated watcher-side discovery
		// pass (see checkRecovery's own comment for why it deliberately
		// has none).
		if family != "" {
			fs := t.families[family]
			fs.open = true
			t.families[family] = fs
		}
		return
	}
	if family == "" || h.pool.FamilyHasCapacity(family) {
		return // general windows are fine and nothing further governs this request
	}
	fs := t.families[family]
	if shouldNotifyLocked(h.holdMax, &fs, eta, etaKnown) {
		h.notifier.NotifyEpisode(notify.EventHeld,
			"spillway: "+capitalize(familyDisplayName(family))+" held", familyHeldBody(family, eta))
	}
	t.families[family] = fs
}

// checkRecovery evaluates every episode this run has ever opened against
// the pool's CURRENT capacity and sends the recovery message for any that
// has actually recovered (issue #230). Called only by WatchRecovery's
// background loop, deliberately never by the request path: the whole
// point is that recovery must be announced even with zero held requests
// outstanding, and gating it behind a request would silently reintroduce
// exactly that gap.
//
// Deliberately does not go looking for a family that was never reported
// open (no pool.FamilyNames()-style discovery pass): a family this run has
// never seen reportExhaustion for has never had a "held" message sent
// about it either, and announcing "X back" with no matching "X held"
// first would be a non-sequitur, not a recovery.
func (h *Handler) checkRecovery() {
	if h.notifier == nil {
		return
	}
	t := h.episodes
	t.mu.Lock()
	defer t.mu.Unlock()

	if !h.pool.FamilyHasCapacity("") {
		return // general windows still down — nothing can have recovered (see pool.FamilyHasCapacity)
	}
	wasGeneralOpen := t.general.open
	t.general = episodeState{}

	var stillDown []string
	for family, fs := range t.families {
		if !fs.open {
			continue
		}
		if h.pool.FamilyHasCapacity(family) {
			t.families[family] = episodeState{}
			if !wasGeneralOpen {
				// Standalone family recovery: the general episode was
				// never open for this, so this family's own "back" is the
				// whole story.
				h.notifier.NotifyEpisode(notify.EventRecovered,
					"spillway: "+capitalize(familyDisplayName(family))+" back",
					familyBackBody(family))
			}
			continue
		}
		stillDown = append(stillDown, family)
	}

	if !wasGeneralOpen {
		return
	}
	if len(stillDown) == 0 {
		h.notifier.NotifyEpisode(notify.EventRecovered, "spillway: all models back", "All models back")
		return
	}
	for _, family := range stillDown {
		eta, ok := h.pool.EarliestFamilyReset(family)
		h.notifier.NotifyEpisode(notify.EventRecovered,
			"spillway: other models back", otherBackBody(family, eta, ok))
	}
}

// nextEpisodeDeadline reports the soonest reset among every currently-open
// episode, recomputed fresh from the pool each call rather than trusted
// from stored state, so WatchRecovery's timer can never fire on a stale
// ETA. ok=false when nothing open has a scheduled reset to wait for at all
// (ordinarily impossible while any episode is open, but disabled accounts
// give no scheduled reset either — see EarliestReset/EarliestFamilyReset).
func (h *Handler) nextEpisodeDeadline() (time.Time, bool) {
	t := h.episodes
	t.mu.Lock()
	generalOpen := t.general.open
	openFamilies := make([]string, 0, len(t.families))
	for family, fs := range t.families {
		if fs.open {
			openFamilies = append(openFamilies, family)
		}
	}
	t.mu.Unlock()

	var earliest time.Time
	ok := false
	consider := func(x time.Time, xok bool) {
		if xok && (!ok || x.Before(earliest)) {
			earliest, ok = x, true
		}
	}
	if generalOpen {
		consider(h.pool.EarliestReset())
	}
	for _, family := range openFamilies {
		consider(h.pool.EarliestFamilyReset(family))
	}
	return earliest, ok
}

// WatchRecovery runs the episode recovery watcher until ctx is done (issue
// #230). Recovery has to be announced even with zero held requests
// outstanding (see checkRecovery's comment), and nothing on the request
// path fires at all when nobody is retrying — so this runs independently,
// woken by the pool's capacity-changed signal (issue #105's
// CapacitySignal, already the exact broadcast a probe finding a window
// refilled or an account being un-parked/re-authenticated raises) and by
// each open episode's own reset time passing, rather than polling on a
// tight loop. A capacity signal covers every OTHER way capacity returns
// (ClearRecoveredWindowRejections, ClearExhausted, Apply, EnsureFresh/
// Recover); the timer is only for the case nothing else ever signals —a
// window's reset simply arriving with no probe and no request to notice it
// lazily flipping an account's own State() back to StateOK.
func (h *Handler) WatchRecovery(ctx context.Context) {
	for {
		wake := h.pool.CapacitySignal()
		var timer *time.Timer
		var timerC <-chan time.Time
		if until, ok := h.nextEpisodeDeadline(); ok {
			d := time.Until(until)
			if d < 0 {
				d = 0
			}
			timer = time.NewTimer(d + resetSlack)
			timerC = timer.C
		}
		select {
		case <-wake.Ch():
		case <-timerC:
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		}
		if timer != nil {
			timer.Stop()
		}
		h.checkRecovery()
	}
}

// etaText renders a reset for a notification body: "15:04 (1h36m)", or ""
// when the reset itself is unknown — callers decide the fallback wording,
// since "held" and "still held" read differently with nothing appended.
func etaText(t time.Time, ok bool) string {
	if !ok {
		return ""
	}
	wait := time.Until(t).Round(time.Minute)
	if wait < 0 {
		wait = 0
	}
	return t.Local().Format("15:04") + " (" + wait.String() + ")"
}

// familyDisplayName renders a pool.FamilyKey value for a notification.
// Spillway's window-naming convention is "<duration>-<family>" — "7d-fable"
// is the only one any provider reports today — so this strips the leading
// duration segment. A key that doesn't fit the convention (multiple names
// joined by "+", a future family with no duration prefix, or "") is
// rendered as-is rather than guessed at.
func familyDisplayName(family string) string {
	if family == "" || strings.Contains(family, "+") {
		return family
	}
	if _, rest, ok := strings.Cut(family, "-"); ok && rest != "" {
		return rest
	}
	return family
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func allHeldBody(eta time.Time, ok bool) string {
	if !ok {
		return "All models held"
	}
	return "All models held until " + etaText(eta, ok)
}

func familyHeldBody(family string, eta time.Time) string {
	return capitalize(familyDisplayName(family)) + " held until " + etaText(eta, true) + " — other models running"
}

func familyBackBody(family string) string {
	return capitalize(familyDisplayName(family)) + " back"
}

func otherBackBody(family string, eta time.Time, ok bool) string {
	msg := "Other models back — " + familyDisplayName(family) + " still held"
	if ok {
		msg += " until " + etaText(eta, ok)
	}
	return msg
}
