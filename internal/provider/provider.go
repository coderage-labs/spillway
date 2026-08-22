// Package provider is the one place a provider's behaviour is described.
//
// This is design doc §3's Provider, arriving late (§6.20). Before it, adding
// a provider meant finding eleven `switch acct.Type` sites across seven
// files — request classification, model-id errors, quota source, default
// upstream, refresh endpoint, probe model, usage polling, capabilities and
// config validation. Missing one produced silent wrong-provider behaviour
// rather than a compile error.
//
// It is a data-driven registry rather than an interface with method
// receivers, for one reason: this package must import nothing else of ours.
// Capabilities are needed by pool, classification by proxy, refresh by
// accounts; an interface carrying their types would make a cycle. Everything
// here speaks primitives and its own small types, and callers convert.
package provider

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Kind is a provider family. It is derived from an account's configured type,
// never stored separately.
type Kind string

const (
	Claude Kind = "claude"
	Kimi   Kind = "kimi"
)

// Capabilities describes what a provider's models accept. Every field is a
// measured behaviour (§6.19), never a documented one.
type Capabilities struct {
	// ThinkingDefaultOn: the provider reasons unless told not to. Anthropic
	// is opt-in; Kimi k3 is opt-out.
	ThinkingDefaultOn bool
	// ForcedToolChoiceWithThinking: tool_choice:{type:"tool"} is accepted
	// while thinking is active. False on Kimi, where the two are mutually
	// exclusive — so forcing a tool fails there by default.
	ForcedToolChoiceWithThinking bool
	// PromptCaching: cache_control is honoured and reported. Kimi caches
	// implicitly and ignores the directive.
	PromptCaching bool
}

// ErrKind classifies an upstream failure. Quota takes the account out of
// rotation; Rate retries it; ModelID and None are neither.
type ErrKind int

const (
	// ErrNone is a failure that is not the pool's business — pass it through.
	ErrNone ErrKind = iota
	// ErrQuota means this account is spent until its window resets.
	ErrQuota
	// ErrRate is a transient throttle: same account, after a backoff.
	ErrRate
	// ErrModelID is an unknown model id wearing an auth status code. The
	// credential is fine; retrying or disabling the account would be wrong.
	ErrModelID
)

// Window is one quota reading. Converted to pool.QuotaWindow by the caller,
// so this package stays dependency-free.
type Window struct {
	Name    string
	Limit   float64
	Used    float64
	ResetAt time.Time
	Source  string
}

// Spec is everything that varies between providers.
type Spec struct {
	Kind Kind
	// AccountType is the value used in config's accounts[].type.
	AccountType string
	// DefaultUpstream when an account does not name one.
	DefaultUpstream string
	// ProbeModel is the cheapest model to send a quota probe to. modelMap is
	// consulted first so a configured mapping wins.
	ProbeModel func(modelMap map[string]string) string
	// Capabilities as measured.
	Capabilities Capabilities
	// Classify decides what an upstream failure means for the pool.
	Classify func(status int, header http.Header, body []byte) ErrKind
	// ClassifiableStatuses are the response codes worth reading a body for.
	// Kimi overloads 401 and 403; Anthropic does not, and reading bodies it
	// never classifies would buffer responses for nothing.
	ClassifiableStatuses []int
	// WindowsFromHeaders reads quota out of a response. Empty for providers
	// that report usage out-of-band.
	WindowsFromHeaders func(header http.Header, now time.Time) []Window
	// PollsUsage: quota comes from an endpoint rather than response headers.
	PollsUsage bool
	// OverageFromHeaders reads whether the account may keep serving past its
	// subscription quota, and be billed for it. Nil for providers with no
	// such concept — which is not the same as "not allowed", so callers must
	// check Known before acting.
	OverageFromHeaders func(header http.Header) Overage
	// ResetHint bounds how long an exhausted account sits out. fallback is
	// the account's own last-known reset, if it has one.
	ResetHint func(header http.Header, now time.Time, fallback time.Time) time.Time
	// RefreshFlavour names the credential-refresh shape. The implementation
	// itself stays with the Manager, which owns the base URLs and HTTP
	// client a package-level registry could not hold per-instance. A new
	// provider reusing an existing flavour needs no code outside this file.
	RefreshFlavour Flavour
}

// Flavour is a credential-refresh shape.
type Flavour string

const (
	// AnthropicOAuth: POST the token endpoint with grant_type=refresh_token.
	AnthropicOAuth Flavour = "anthropic-oauth"
	// KimiDevice: the device-code flow's refresh exchange.
	KimiDevice Flavour = "kimi-device"
)

var specs = map[string]Spec{
	"claude-oauth": {
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
	},

	"kimi-oauth": {
		Kind:            Kimi,
		AccountType:     "kimi-oauth",
		DefaultUpstream: "https://api.kimi.com/coding",
		ProbeModel: func(m map[string]string) string {
			if v, ok := m["claude-haiku-4-5-20251001"]; ok {
				return v
			}
			return "k3"
		},
		Capabilities: Capabilities{
			ThinkingDefaultOn:            true,
			ForcedToolChoiceWithThinking: false,
			PromptCaching:                false,
		},
		// Kimi speaks neither unified-status headers nor retry-after for a
		// quota 429, and overloads 401/403 — so classification is body-based.
		Classify: func(status int, _ http.Header, body []byte) ErrKind {
			lower := strings.ToLower(string(body))
			if status == http.StatusUnauthorized && strings.Contains(lower, "model id") {
				// Measured 2026-08-21: an unknown model id returns 401. The
				// credential is fine; treating this as auth failure would
				// disable a healthy account.
				return ErrModelID
			}
			if status == http.StatusForbidden {
				if containsAny(lower, "usage limit", "quota", "billing") {
					return ErrQuota
				}
				return ErrNone
			}
			if status == http.StatusTooManyRequests {
				if strings.Contains(lower, "usage limit") {
					return ErrQuota
				}
				return ErrRate // plain 429: concurrency throttle
			}
			return ErrNone
		},
		PollsUsage:     true,
		RefreshFlavour: KimiDevice,
		// 403 carries quota/billing here, and 401 can be a model-id error.
		ClassifiableStatuses: []int{
			http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden},
		ResetHint: func(_ http.Header, now, fallback time.Time) time.Time {
			if fallback.After(now) {
				return fallback
			}
			return now.Add(time.Hour)
		},
	},
}

// For returns the spec for a configured account type. An unknown or empty
// type gets Claude's, which is the API every account here claims to speak.
func For(accountType string) Spec {
	if s, ok := specs[accountType]; ok {
		return s
	}
	return specs["claude-oauth"]
}

// Classifiable reports whether a status is worth reading a body for.
func (s Spec) Classifiable(status int) bool {
	for _, c := range s.ClassifiableStatuses {
		if c == status {
			return true
		}
	}
	return false
}

// Known reports whether an account type is configured. Config validation
// reads the registry rather than repeating the list.
func Known(accountType string) bool {
	_, ok := specs[accountType]
	return ok
}

// Types lists the valid account types, for error messages.
func Types() []string {
	out := make([]string, 0, len(specs))
	for k := range specs {
		out = append(out, k)
	}
	// Stable order for deterministic messages.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Overage is what a provider says about serving past the subscription quota
// at cost. Known distinguishes "the provider told us" from "we have no idea",
// because spending the user's money on a guess is not acceptable.
type Overage struct {
	Known     bool
	Available bool
	// InUse reports that THIS response was itself served on extra usage —
	// the provider's own word rather than our prediction. Ground truth beats
	// inference here: the first live test billed a request that spillway
	// recorded as ordinary traffic, because the prediction was wrong.
	InUse bool
	// Utilization is the fraction of the extra-usage allowance consumed
	// (0-1), where 1 means the next request is refused. -1 when unreported.
	Utilization float64
	// ResetAt is when the extra-usage allowance refills. Zero when unknown;
	// unlike a quota window this is typically a billing period, so it can be
	// months away.
	ResetAt time.Time
	// Reason is the provider's explanation when it is not available, e.g.
	// "member_zero_credit_limit". Worth surfacing: most of these are a
	// setting the user can change.
	Reason string
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
