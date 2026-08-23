package pool

// Per-family quota (issue #24).
//
// Anthropic reports separate quota buckets per model family: an account can
// be fully spent for "fable" while Sonnet and Opus still have headroom.
// OverThreshold used to scan every recorded window regardless of what the
// request actually asked for, so a spent fable bucket read as the whole
// account being done — rotating Sonnet/Opus traffic away from an account
// that had plenty of room for it, and doing nothing to stop a fable request
// landing on an account whose fable bucket really was gone.
//
// The fix is the same shape as capability.go's CanServe: look at the request
// body, ask the provider what governs it, and let selection prefer rather
// than refuse.

import (
	"encoding/json"

	"github.com/coderage-labs/spillway/internal/provider"
)

// modelOf reads the top-level "model" field from a request body, or "" when
// absent, malformed, or there is no body at all (Select's callers that pass
// nil). "" is deliberately treated as "unrecognised" by
// provider.GoverningWindows implementations, never as a signal to skip
// family narrowing — an empty model must resolve to the general windows,
// the same as any other model this package cannot identify.
func modelOf(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return probe.Model
}

// OverThresholdFor reports whether the window(s) that govern model are
// at/above frac, per the account's provider (issue #24). A provider with no
// family-scoped buckets (GoverningWindows nil — Kimi today) falls back to
// OverThreshold's old behaviour of scanning every recorded window, because
// there is nothing to narrow by.
func (a *Account) OverThresholdFor(model string, frac float64) bool {
	gw := provider.For(a.Type).GoverningWindows
	if gw == nil {
		return a.OverThreshold(frac)
	}
	governing := gw(model)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, w := range a.windows {
		if w.Limit <= 0 || w.Used/w.Limit < frac {
			continue
		}
		for _, name := range governing {
			if w.Name == name {
				return true
			}
		}
	}
	return false
}

// OverThresholdForWindow reports whether one specific window (by name) is
// at/above frac, regardless of which model it governs. Used to surface a
// named family's status in the dashboard/CLI (e.g. "fable") without
// widening it into the general OverThreshold bit (#24 decision 3).
func (a *Account) OverThresholdForWindow(name string, frac float64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, w := range a.windows {
		if w.Name == name && w.Limit > 0 && w.Used/w.Limit >= frac {
			return true
		}
	}
	return false
}
