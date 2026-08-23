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
	"errors"
	"net/http"
	"strings"
	"time"
)

// ErrRefreshDead marks a permanently rejected refresh token (400/401): the
// credential is gone and retrying cannot help, so the account is disabled
// rather than rotated away from. Lives here because both providers' refresh
// paths raise it and this is the package they share.
var ErrRefreshDead = errors.New("refresh token rejected")

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
	// OwnsUpstream distinguishes a provider that must be reached at its own
	// host from one whose DefaultUpstream merely restates the global
	// `upstream` setting. Without it the caller has to name a provider to
	// decide, which is the branch this registry exists to remove.
	OwnsUpstream bool
	// DefaultModelMap maps incoming model ids to this provider's own, used
	// where the account configures no mapping of its own. Without it a
	// freshly logged-in account of a provider that speaks different ids is
	// unusable: every request is a hard unmapped-model error until someone
	// hand-writes a map. An account's own modelMap always wins.
	DefaultModelMap map[string]string
	// GoverningWindows returns the quota window names that govern a given
	// model (issue #24): Anthropic reports separate buckets per model
	// family ("5h", "7d", "7d-fable"), and a bucket being spent for one
	// family must not read as the whole account being done for every other.
	//
	// nil means the provider has no family-scoped buckets at all — Kimi's
	// windows are just whatever /usages reports, with nothing to narrow by
	// model — and callers must fall back to treating every recorded window
	// as governing, which is the behaviour from before this field existed.
	//
	// A non-nil implementation must still resolve an unrecognised model: it
	// has to return the provider's general windows rather than nothing, and
	// must never guess a narrower family (e.g. fable) for a model it does
	// not actually recognise as belonging to it.
	GoverningWindows func(model string) []string
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

// specs is the index. Each provider's entry lives in its own file —
// claude.go, kimi.go — so "where does this provider live" has one answer.
//
// A map of data rather than an interface, deliberately. The obvious design is
// a Provider interface, and it cannot be built here: the values it would
// carry are needed in three directions at once — capabilities by pool, error
// classification by proxy, refresh by accounts — so an interface naming their
// types makes this package import all three, and all three already import it.
// A struct of plain funcs and values imports nothing of ours and cycles with
// nothing.
var specs = map[string]Spec{
	"claude-oauth": claudeSpec,
	"kimi-oauth":   kimiSpec,
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
