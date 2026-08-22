package provider

// Everything spillway knows about Anthropic: what it can do, how it reports
// quota and extra usage, and how to read a refusal. The helpers below are
// only used from here.

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

var claudeSpec = Spec{
	Kind:            Claude,
	AccountType:     "claude-oauth",
	DefaultUpstream: "https://api.anthropic.com",
	ProbeModel: func(m map[string]string) string {
		if v, ok := m["claude-haiku-4-5-20251001"]; ok {
			return v
		}
		return "claude-haiku-4-5-20251001"
	},
	Capabilities: Capabilities{
		ThinkingDefaultOn:            false,
		ForcedToolChoiceWithThinking: true,
		PromptCaching:                true,
	},
	Classify: func(status int, h http.Header, _ []byte) ErrKind {
		if status != http.StatusTooManyRequests {
			return ErrNone
		}
		if anthropicRejected(h) {
			return ErrQuota
		}
		return ErrRate
	},
	WindowsFromHeaders:   anthropicWindows,
	OverageFromHeaders:   anthropicOverage,
	RefreshFlavour:       AnthropicOAuth,
	ClassifiableStatuses: []int{http.StatusTooManyRequests, http.StatusUnauthorized},
	ResetHint: func(h http.Header, now, _ time.Time) time.Time {
		return anthropicReset(h, now)
	},
}

// overageAllowed is the set of statuses that mean extra usage will serve.
//
// "allowed_warning" is not a near-miss for "allowed": it means allowed AND
// past the warning threshold. Testing for equality with "allowed" read a live
// account with working extra usage as having none — so spillway served a
// billed request and recorded it as ordinary traffic. Membership, not
// equality, and anything unrecognised still fails closed.
var overageAllowed = map[string]bool{
	"allowed":         true,
	"allowed_warning": true,
}

// anthropicOverage reads the extra-usage headers Anthropic sends on every
// response. Observed live (2026-08-22), both shapes:
//
//	Overage-Status: rejected
//	Overage-Disabled-Reason: member_zero_credit_limit
//
//	Overage-Status: allowed_warning
//	Overage-In-Use: true
//	Overage-Utilization: 0.98
//	Overage-Surpassed-Threshold: 0.95
//	Overage-Reset: 1788220800
func anthropicOverage(h http.Header) Overage {
	v := h.Get("anthropic-ratelimit-unified-overage-status")
	inUse := h.Get("anthropic-ratelimit-unified-overage-in-use") == "true"
	if v == "" && !inUse {
		return Overage{Utilization: -1}
	}
	ov := Overage{
		Known: true,
		// In-use implies available whatever the status says: the provider
		// just served a billed request, which settles the question.
		Available:   overageAllowed[v] || inUse,
		InUse:       inUse,
		Utilization: -1,
		Reason:      h.Get("anthropic-ratelimit-unified-overage-disabled-reason"),
	}
	if u := h.Get("anthropic-ratelimit-unified-overage-utilization"); u != "" {
		if f, err := strconv.ParseFloat(u, 64); err == nil {
			ov.Utilization = f
		}
	}
	if r := h.Get("anthropic-ratelimit-unified-overage-reset"); r != "" {
		if sec, err := strconv.ParseFloat(r, 64); err == nil {
			ov.ResetAt = time.Unix(int64(sec), 0)
		}
	}
	return ov
}

func anthropicRejected(h http.Header) bool {
	return h.Get("anthropic-ratelimit-unified-5h-status") == "rejected" ||
		h.Get("anthropic-ratelimit-unified-7d-status") == "rejected"
}

func anthropicWindows(h http.Header, now time.Time) []Window {
	var out []Window
	for _, w := range []struct{ name, util, reset string }{
		{"5h", "anthropic-ratelimit-unified-5h-utilization", "anthropic-ratelimit-unified-5h-reset"},
		{"7d", "anthropic-ratelimit-unified-7d-utilization", "anthropic-ratelimit-unified-7d-reset"},
		{"7d-fable", "anthropic-ratelimit-unified-7d_oi-utilization", "anthropic-ratelimit-unified-7d_oi-reset"},
	} {
		v := h.Get(w.util)
		if v == "" {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		win := Window{Name: w.name, Limit: 1, Used: f, Source: "headers"}
		if rs := h.Get(w.reset); rs != "" {
			if sec, err := strconv.ParseFloat(rs, 64); err == nil {
				win.ResetAt = time.Unix(int64(sec), 0)
			}
		}
		out = append(out, win)
	}
	return out
}

func anthropicReset(h http.Header, now time.Time) time.Time {
	var reset time.Time
	for _, k := range []string{
		"anthropic-ratelimit-unified-5h-reset",
		"anthropic-ratelimit-unified-7d-reset",
	} {
		if v := h.Get(k); v != "" {
			if sec, err := strconv.ParseFloat(v, 64); err == nil {
				if t := time.Unix(int64(sec), 0); t.After(reset) {
					reset = t
				}
			}
		}
	}
	if !reset.IsZero() && reset.After(now) {
		return reset
	}
	if ra := retryAfter(h); ra > 0 {
		return now.Add(time.Duration(ra) * time.Second)
	}
	return now.Add(time.Hour)
}

func retryAfter(h http.Header) int {
	n, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil {
		return 0
	}
	return n
}
