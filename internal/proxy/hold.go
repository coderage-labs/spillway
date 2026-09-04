package proxy

// Pool-exhaustion hold (design doc §6.11): when every account is spent, the
// worst outcome is a hang the user can't diagnose — so holding is explicit,
// bounded per request, and loud in notify mode.
//
// Issue #55: holdMax bounds how long a single request will park, but that
// bound alone isn't enough. A reset well past the deadline (a spent weekly
// window with days left on it, say) used to still park for the full holdMax
// only to reach the exact 429 it could have returned immediately.
// waitForReset now fails fast whenever the soonest KNOWN reset would land
// after the deadline — same answer, without the wait.
//
// Issue #105: park used to wait on exactly the reset timer and the client
// giving up — a moment in time, never a fact about the pool. An account
// added, re-probed back to health, un-parked, or re-authenticated while a
// request was parked was invisible to it: the good account sat idle and the
// request slept until the ORIGINAL reset regardless. park now also selects
// on the pool's capacity-changed signal (internal/pool/capacity.go), waking
// early to re-try selection — but a wake only means "something changed, go
// look again", not "this request can now be served". The caller
// (waitForReset via proxy.go's loop) re-selects and, if that still fails,
// comes back through waitForReset against the SAME original deadline —
// never a fresh budget.
import (
	"net/http"
	"time"

	"github.com/coderage-labs/spillway/internal/notify"
)

// wakeStagger bounds how long park delays after a capacity wake before
// re-selecting, scaled by the caller's rank among everyone this same signal
// woke (issue #105's anti-thunder measure). Every held request reads the
// same broadcast channel, so without this every one of them would call
// SelectExcept in the same instant a single account became usable — the
// exact herd issue #91 was filed about (51 held requests), just retriggered
// on a faster clock. Selection and inFlight accounting already spread load
// across MULTIPLE usable accounts; this spreads the RATE of re-selection
// when there is only one. Small enough that the reported bug (a good
// account sitting idle for what could be hours) is still fixed in
// low-single-digit seconds even for a large herd; capped by maxWakeStagger
// below so a pathologically large herd can't push an individual wait past
// its own deadline.
const wakeStagger = 30 * time.Millisecond

// maxWakeStagger caps the total stagger delay regardless of rank, so an
// unusually large herd degrades to "many requests re-select at roughly the
// same capped delay" rather than "the delay grows without bound".
const maxWakeStagger = 2 * time.Second

// resetSlack absorbs two unrelated small sources of clock skew, rather than
// building a cliff that is sensitive to either:
//   - the reset-vs-deadline comparison below: a reset landing a few hundred
//     milliseconds past the deadline is, for any practical purpose, "at"
//     the deadline, not "hours away" — ordinary scheduling jitter between
//     computing the deadline and re-checking it shouldn't flip fail-fast vs.
//     hold.
//   - the wait itself: firing exactly at the reset risks re-selecting a
//     moment before the upstream's own clock agrees the window is over.
const resetSlack = 250 * time.Millisecond

// waitForReset parks the request until the soonest reset that could let
// THIS request through, bounded by deadline (holdMax from first
// exhaustion). Returns true when selection should be re-tried (the reset
// arrived, or an unknown-reset budget elapsed), false to fail now (mode
// disabled, a known reset beyond the deadline, an unknown-reset budget
// already spent, or the client went away).
//
// body is the buffered request body — the same bytes SelectExcept was just
// handed. It is what makes the wait request-specific (issue #140): the
// model in it decides which quota windows govern this request, and so which
// per-window rejection deadlines are the ones it is actually waiting on.
func (h *Handler) waitForReset(r *http.Request, body []byte, deadline time.Time) bool {
	if h.holdMax <= 0 || h.exhaustedMode == "fail" {
		return false
	}
	now := time.Now()
	reset, ok := h.pool.EarliestReset()
	// Issue #140: a family-scoped 429 never exhausts the account (#54 leaves
	// it StateOK so the other families keep serving), so a request parked
	// because every account is window-rejected for its model is invisible to
	// EarliestReset — it reported ok=false and the request slept the whole
	// holdMax with its real deadline minutes away. The wake time is the
	// soonest of BOTH: a mixed pool (some accounts exhausted account-wide,
	// others only window-rejected) can be unblocked by whichever lands
	// first, and waking early only costs one re-selection.
	if wreset, wok := h.pool.EarliestWindowReset(body); wok && (!ok || wreset.Before(reset)) {
		reset, ok = wreset, true
	}
	if !ok {
		// Neither an exhaustion nor a window rejection gives a reset we can
		// reason about (e.g. every blocking account is disabled — there's no
		// scheduled notion of "how long"). Issue #55's fail-fast only fires
		// when we KNOW waiting is pointless; here we don't know that, and
		// treating "unknown" as "far away" would turn a possibly-transient
		// blip into an instant, unretryable error. Hold for the same bounded budget
		// this request already committed to — today's pre-#55 behaviour —
		// and let the deadline, not a guess, decide when to give up.
		remaining := deadline.Sub(now)
		if remaining <= 0 {
			return false
		}
		return h.park(r, remaining, deadline)
	}

	wait := reset.Sub(now)
	if wait <= 0 {
		return true // already past — re-select immediately
	}
	if reset.After(deadline.Add(resetSlack)) {
		// The reset this request would eventually get served on is beyond
		// what it's willing to hold for. Parking anyway would only spend
		// the whole budget to reach the identical 429 (#55) — say why and
		// let the caller have it now.
		log := h.logger.Info
		if h.exhaustedMode == "notify" {
			log = h.logger.Warn
		}
		log("pool exhausted — soonest reset is beyond the hold budget, failing fast",
			"path", r.URL.Path,
			"reset", reset.UTC().Format(time.RFC3339),
			"holdMax", h.holdMax,
		)
		return false
	}

	log := h.logger.Info
	if h.exhaustedMode == "notify" {
		log = h.logger.Warn
		// §6.11: the whole point of notify mode. The request is about to be
		// parked for possibly hours, and a log line does not reach someone
		// who has walked away.
		if h.notifier != nil {
			h.notifier.Notify(notify.EventHeld, "pool-held", "spillway: pool exhausted",
				"Holding requests until "+reset.Local().Format("15:04")+
					" ("+wait.Round(time.Minute).String()+")")
		}
	}
	log("pool exhausted — holding request until reset",
		"path", r.URL.Path,
		"reset", reset.UTC().Format(time.RFC3339),
		"wait", wait.Round(time.Second),
	)
	return h.park(r, wait, reset)
}

// park blocks for wait (or until the pool signals new capacity — issue
// #105), registering the hold — for the status line and dashboard, so a
// parked request is distinguishable from a hung one (§6.11) — as ending at
// until. Returns true once wait has elapsed OR the pool gained
// potentially-usable capacity, false immediately if the client disconnects
// first. A true return is a hint to re-try selection, not a promise it will
// succeed — the caller's loop already treats every true return this way.
func (h *Handler) park(r *http.Request, wait time.Duration, until time.Time) bool {
	release := h.pool.BeginHold(until)
	defer release()

	timer := time.NewTimer(wait + resetSlack) // slack past the reset
	defer timer.Stop()

	wake := h.pool.CapacitySignal()
	select {
	case <-timer.C:
		return true
	case <-wake.Ch():
		// Anti-thunder: spread re-selection across the requests this same
		// signal woke instead of every one of them calling SelectExcept in
		// the same instant (see wakeStagger's comment). The backstop timer
		// and a client disconnect still both pre-empt the stagger.
		stagger := time.Duration(wake.Rank()) * wakeStagger
		if stagger > maxWakeStagger {
			stagger = maxWakeStagger
		}
		if stagger > 0 {
			staggerTimer := time.NewTimer(stagger)
			defer staggerTimer.Stop()
			select {
			case <-staggerTimer.C:
			case <-timer.C:
			case <-r.Context().Done():
				return false
			}
		}
		return true
	case <-r.Context().Done():
		return false // client disconnect cancels the hold
	}
}
