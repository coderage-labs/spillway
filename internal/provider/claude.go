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
		if len(anthropicRejectedWindows(h)) > 0 {
			return ErrQuota
		}
		return ErrRate
	},
	RejectedWindows:      anthropicRejectedWindows,
	WindowsFromHeaders:   anthropicWindows,
	GoverningWindows:     claudeGoverningWindows,
	OverageFromHeaders:   anthropicOverage,
	RefreshFlavour:       AnthropicOAuth,
	ClassifiableStatuses: []int{http.StatusTooManyRequests, http.StatusUnauthorized},
	ResetHint: func(h http.Header, windows []string, now, _ time.Time) time.Time {
		return anthropicReset(h, windows, now)
	},
}

// Unparsed headers (issue #53), for the next person before they re-derive
// this from scratch. Measured live together on the same real response
// (2026-08-22):
//
//	Anthropic-Ratelimit-Unified-Status=allowed
//	Anthropic-Ratelimit-Unified-Reset=<unix seconds>
//	Anthropic-Ratelimit-Unified-Fallback-Percentage=0.5
//
// Status and Reset read like a pool-wide summary of the per-window values
// anthropicWindows already parses below (one "allowed"/one reset standing
// in for 5h + 7d + 7d-fable combined) — plausible, but nobody has forced a
// case where they disagree with the per-window headers to actually confirm
// it. Nothing reads them; do not treat this paragraph as having verified
// the theory.
//
// Fallback-Percentage=0.5 is flatly unexplained. It is not obviously the
// same "fallback" model routing means elsewhere in the Anthropic API, and
// 0.5 was observed on a request that had nothing evidently 50% about it.
// Do not let anything depend on it, guess a meaning for it, or wire it into
// selection until someone actually determines what it tracks.
//
// Anthropic-Ratelimit-Unified-Overage-Status and
// -Overage-Disabled-Reason are deliberately NOT in the unparsed set above —
// unlike the three headers named there, they are already read, by
// anthropicOverage immediately below.
//
// anthropicRepresentativeClaimHeader (issue #53, below) is a fourth header
// this package now reads: Anthropic-Ratelimit-Unified-Representative-Claim,
// which is the closest thing to a direct answer for which window governed
// a given response — see claudeGoverningWindows' own comment for how it
// differs from that static guess.

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

// claudeWindows is the single source of truth for every Anthropic
// rate-limit window family this package understands: spillway's name for
// the window, and the header prefix Anthropic uses for it
// (anthropic-ratelimit-unified-<prefix>-{status,utilization,reset}).
//
// anthropicRejectedWindows and anthropicWindows both range over this list
// instead of naming header strings themselves, so a fourth family (issue
// #25 found "7d_oi" for fable the hard way, by a live 429 going
// unrecognised) is covered everywhere by adding one line here, not by
// remembering to update two functions in lockstep.
var claudeWindows = []struct{ name, prefix string }{
	{"5h", "5h"},
	{"7d", "7d"},
	{"7d-fable", "7d_oi"},
}

// anthropicRejectedWindows names which window families' status header say
// this request was refused for quota, not throttled (issue #25, widened by
// #54 from a bool to the actual names: knowing a request was quota-rejected
// doesn't say how far to rotate — the proxy needs to know which window
// fired so it can scope exhaustion to what that window actually governs,
// instead of the whole account for every model).
//
// Checked as "!= allowed" rather than "== rejected": the measured
// vocabulary is only {allowed, rejected} today (issue #25's comment), but
// it is not exhaustively known, and the two ways to be wrong are not
// equally bad. Treating an unrecognised status as a rejection costs one
// account an undeserved rotation — every other account in the pool is
// still tried. Treating it as transient instead means retrying the same
// spent account three times with backoff while a healthy one sits idle,
// which is the exact failure #25 reports; an unknown future status must
// not be able to reproduce that. Fail toward rotation, not toward retry.
//
// An absent status header is neither: it means this request never engaged
// that family (a Haiku request carries no 7d_oi-* at all, per #25), so it
// is skipped rather than compared — never returned as a rejected name.
func anthropicRejectedWindows(h http.Header) []string {
	var out []string
	for _, w := range claudeWindows {
		v := h.Get("anthropic-ratelimit-unified-" + w.prefix + "-status")
		if v == "" {
			continue
		}
		if v != "allowed" {
			out = append(out, w.name)
		}
	}
	return out
}

func anthropicWindows(h http.Header, now time.Time) []Window {
	var out []Window
	for _, w := range claudeWindows {
		v := h.Get("anthropic-ratelimit-unified-" + w.prefix + "-utilization")
		if v == "" {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		win := Window{Name: w.name, Limit: 1, Used: f, Source: "headers"}
		if rs := h.Get("anthropic-ratelimit-unified-" + w.prefix + "-reset"); rs != "" {
			if sec, err := strconv.ParseFloat(rs, 64); err == nil {
				win.ResetAt = time.Unix(int64(sec), 0)
			}
		}
		out = append(out, win)
	}
	return out
}

// claudeGoverningWindows implements Spec.GoverningWindows for Claude
// (issue #24). "5h" and "7d" are account-wide — every request draws on
// them, fable included. "7d-fable" is the extra weekly bucket fable models
// draw on top of that; a model outside the fable family never touches it,
// so its own bucket being spent must not make a Sonnet or Opus request look
// like it is talking to a done account — that is what this function's
// narrowing is for.
//
// anthropicRejectedWindows (issue #25), by contrast, checks all three
// families: it only decides whether a 429 the account actually received is
// quota or throttle, before any model is known. This function is also the
// answer to "which of the rejected names is account-wide" — the proxy
// (issue #54) calls claudeGoverningWindows("") to get exactly this
// account-wide set, and only widens exhaustion to the whole account when a
// rejected window is in it; a fable-only rejection instead marks just that
// window, never pool.MarkExhausted.
//
// An unrecognised model — including the empty string modelOf returns for a
// malformed or absent body — resolves to the general windows, never to
// fable: guessing narrower for a model this package cannot actually identify
// would silently gate traffic on a bucket that has nothing to do with it.
func claudeGoverningWindows(model string) []string {
	windows := []string{"5h", "7d"}
	if strings.Contains(strings.ToLower(model), "fable") {
		windows = append(windows, "7d-fable")
	}
	return windows
}

// anthropicRepresentativeClaimHeader carries Anthropic's own answer to
// "which window governed this response" (issue #53) — as opposed to
// claudeGoverningWindows above, which only guesses which windows COULD
// govern a request, from the model name, before any response exists.
const anthropicRepresentativeClaimHeader = "anthropic-ratelimit-unified-representative-claim"

// representativeClaimWindows translates the header's snake_case vocabulary
// into spillway's own window names (issue #53).
//
// "five_hour" is the only entry here, and it is the only one that should
// be: it is the only value anyone has actually read off a live response
// (a fable request, where it named the 5h bucket rather than the weekly
// fable one #24's static map would have predicted — see
// claudeGoverningWindows). The other families almost certainly have their
// own spellings ("seven_day"? "seven_day_opus_income"? — nobody knows),
// but guessing them here would silently launder a guess as a measurement:
// AnthropicRepresentativeClaim below reports anything not in this map as
// unrecognised rather than pretending to translate it, and callers must
// log that as "unknown", never as a mismatch — a mismatch claims to know
// what the value should have been, and for everything but five_hour we
// don't.
var representativeClaimWindows = map[string]string{
	"five_hour": "5h", // measured live, issue #53 — the only confirmed entry
}

// AnthropicRepresentativeClaim reads the representative-claim header and
// translates it to spillway's window naming (issue #53).
//
// raw == "" (ok=false) means the header was absent, which is normal and not
// a finding: not every response carries it, the same way a Haiku request
// carries no 7d_oi-* headers at all (issue #25). Callers must treat that as
// a no-op, never log it.
//
// window == "" with recognised == false but raw != "" means the header was
// present with a value this package doesn't yet have a translation for —
// log it as unknown, not as evidence of a mismatch.
func AnthropicRepresentativeClaim(h http.Header) (raw, window string, recognised bool) {
	raw = h.Get(anthropicRepresentativeClaimHeader)
	if raw == "" {
		return "", "", false
	}
	window, recognised = representativeClaimWindows[raw]
	return raw, window, recognised
}

// anthropicReset bounds how long an exhausted window sits out, reading the
// reset header of only the named windows (issue #54).
//
// Before this, the reset was always the max of 5h and 7d's own headers,
// regardless of which window actually rejected the request: a 5h-only
// rejection (5h rejected, 7d allowed) borrowed 7d's far-off reset and
// sidelined the account for the whole week instead of ~2 hours — measured
// live on main, 144h instead of 2. Restricting the scan to `windows` (the
// caller passes only the window(s) that actually fired) fixes that same
// bug for every family at once, fable's 7d_oi included, rather than adding
// a fourth special case here.
//
// windows empty (a provider with no per-window signal, or a defensive call
// with nothing to narrow by) finds no reset header to read and falls
// straight to the fallbacks below — never to a wider scan across every
// window this package knows about.
func anthropicReset(h http.Header, windows []string, now time.Time) time.Time {
	var reset time.Time
	for _, w := range claudeWindows {
		if !containsName(windows, w.name) {
			continue
		}
		if v := h.Get("anthropic-ratelimit-unified-" + w.prefix + "-reset"); v != "" {
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

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func retryAfter(h http.Header) int {
	n, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil {
		return 0
	}
	return n
}
