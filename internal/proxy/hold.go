package proxy

// Pool-exhaustion hold (design doc §6.11): when every account is spent, the
// worst outcome is a hang the user can't diagnose — so holding is explicit,
// bounded per request, and loud in notify mode.

import (
	"net/http"
	"time"
)

// waitForReset parks the request until the soonest account reset, bounded by
// deadline (holdMax from first exhaustion). Returns true when selection
// should be re-tried (reset reached), false to fail now (mode disabled, no
// reset known, budget exceeded, or the client went away).
func (h *Handler) waitForReset(r *http.Request, deadline time.Time) bool {
	if h.holdMax <= 0 || h.exhaustedMode == "fail" {
		return false
	}
	reset, ok := h.pool.EarliestReset()
	if !ok {
		return false // all disabled — waiting won't help
	}
	wait := time.Until(reset)
	if wait <= 0 {
		return true // already past — re-select immediately
	}
	if time.Now().Add(wait).After(deadline) {
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
	// Register the hold so the status line and dashboard can say what is
	// happening. Without this a parked request is indistinguishable from a
	// hung one from the outside.
	release := h.pool.BeginHold(reset)
	defer release()

	timer := time.NewTimer(wait + 250*time.Millisecond) // slack past the reset
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-r.Context().Done():
		return false // client disconnect cancels the hold
	}
}
