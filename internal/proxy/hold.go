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

import (
	"net/http"
	"time"
)

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

// waitForReset parks the request until the soonest account reset, bounded by
// deadline (holdMax from first exhaustion). Returns true when selection
// should be re-tried (the reset arrived, or an unknown-reset budget
// elapsed), false to fail now (mode disabled, a known reset beyond the
// deadline, an unknown-reset budget already spent, or the client went
// away).
func (h *Handler) waitForReset(r *http.Request, deadline time.Time) bool {
	if h.holdMax <= 0 || h.exhaustedMode == "fail" {
		return false
	}
	now := time.Now()
	reset, ok := h.pool.EarliestReset()
	if !ok {
		// Nothing is exhausted with a reset we can reason about (e.g. every
		// blocking account is disabled — there's no scheduled notion of
		// "how long"). Issue #55's fail-fast only fires when we KNOW
		// waiting is pointless; here we don't know that, and treating
		// "unknown" as "far away" would turn a possibly-transient blip into
		// an instant, unretryable error. Hold for the same bounded budget
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
			h.notifier.Notify("pool-exhausted", "spillway: pool exhausted",
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

// park blocks for wait, registering the hold — for the status line and
// dashboard, so a parked request is distinguishable from a hung one (§6.11)
// — as ending at until. Returns true once wait has elapsed, false
// immediately if the client disconnects first.
func (h *Handler) park(r *http.Request, wait time.Duration, until time.Time) bool {
	release := h.pool.BeginHold(until)
	defer release()

	timer := time.NewTimer(wait + resetSlack) // slack past the reset
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-r.Context().Done():
		return false // client disconnect cancels the hold
	}
}
