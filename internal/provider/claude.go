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

// claudeProbeModelID is the cheapest, account-wide-only model ProbeModel
// sends a quota probe to. Named so claudeFamilyProbeModel below can defer
// to the identical id for the two families ("5h", "7d") this plain probe
// already measures, rather than repeating the literal.
const claudeProbeModelID = "claude-haiku-4-5-20251001"

// claudeFableProbeModelID is the LAST-RESORT fable-governed model for
// issue #229's family probe — used only when accounts.familyProbeModel
// cannot find any model this account, or any other account in the pool,
// has actually been served on (Account.LastModel) that claudeGoverningWindows
// recognises as fable-governed. Preferring an observed model over this
// constant is deliberate: an earlier version of this fix hard-coded a
// GUESSED id ("claude-haiku-4-5-fable") that nothing had ever confirmed
// against a live account, and — worse — a guessed-but-syntactically-valid
// id still returns ok=true here, so the "nothing to probe with" safety
// branch never engaged; the probe just 400/404'd every time, measured
// nothing, and stranded a non-billable account until its real reset,
// days out.
//
// "claude-fable-5-1": read from model_served in the live request log on
// 2026-09-28 (16,071 requests over the prior 7 days) — a real id this pool
// actually serves fable traffic on, not a guess. It will age as Anthropic
// ships new model ids; that is exactly why it is the FALLBACK, not the
// first choice.
const claudeFableProbeModelID = "claude-fable-5-1"

// claudeFamilyProbeModel implements Spec.FamilyProbeModel (issue #229).
// "5h" and "7d" are account-wide — every request governs them, including
// claudeProbeModelID's own — so they just defer to the plain probe model.
// "7d-fable" needs a model claudeGoverningWindows itself would recognise as
// fable-governed, or the "probe" would ask the wrong question and learn
// nothing about the window it was sent to test.
func claudeFamilyProbeModel(window string, modelMap map[string]string) (string, bool) {
	switch window {
	case "5h", "7d":
		if v, ok := modelMap[claudeProbeModelID]; ok {
			return v, true
		}
		return claudeProbeModelID, true
	case "7d-fable":
		if v, ok := modelMap[claudeFableProbeModelID]; ok {
			return v, true
		}
		return claudeFableProbeModelID, true
	}
	return "", false
}

var claudeSpec = Spec{
	Kind:            Claude,
	AccountType:     "claude-oauth",
	DefaultUpstream: "https://api.anthropic.com",
	ProbeModel: func(m map[string]string) string {
		if v, ok := m[claudeProbeModelID]; ok {
			return v
		}
		return claudeProbeModelID
	},
	FamilyProbeModel: claudeFamilyProbeModel,
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

// anthropicAllowedStatuses is the set of `-status` values that mean "this
// still serves" — shared by every place this package reads one of
// Anthropic's unified-ratelimit status headers (the overage status below,
// and the per-window statuses anthropicRejectedWindows reads), so the two
// can't drift back apart the way #234 found them already had.
//
// "allowed_warning" is not a near-miss for "allowed": it means allowed AND
// past the warning threshold. Testing for equality with "allowed" read a
// live account with working extra usage as having none — so spillway
// served a billed request and recorded it as ordinary traffic (the overage
// path, fixed first). Issue #234 found the identical mistake one level up:
// anthropicRejectedWindows was still testing per-window status with
// `!= "allowed"`, so a window merely past its warning threshold (7d at
// 79%, still allowed) was counted as rejected alongside whichever window
// actually caused the 429 (fable's 7d_oi) — and ScopeRejection then saw 7d
// in the rejected set and benched the account for every model until the
// weekly reset, days out. Membership, not equality, in both places now;
// anything unrecognised still fails closed.
var anthropicAllowedStatuses = map[string]bool{
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
	inUse := h.Get(OverageInUseHeader) == "true"
	if v == "" && !inUse {
		return Overage{Utilization: -1}
	}
	ov := Overage{
		Known: true,
		// In-use implies available whatever the status says: the provider
		// just served a billed request, which settles the question.
		Available:   anthropicAllowedStatuses[v] || inUse,
		InUse:       inUse,
		Utilization: -1,
		Reason:      h.Get(OverageDisabledReasonHeader),
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
// Checked as membership in anthropicAllowedStatuses rather than equality
// with "rejected", and NOT as "!= allowed" either (issue #234, the same
// correction anthropicOverage's comment above already went through once):
// "allowed_warning" means allowed and past the warning threshold, not
// rejected. Treating it as rejected dragged every window past its warning
// threshold into the rejected set on any 429 — live 2026-09-28, a fable
// 429 (7d_oi rejected) pulled 7d (allowed_warning, 79% used) in with it,
// ScopeRejection saw 7d in that set and benched the account for every
// model, not just fable, until the 7d reset days out.
//
// Anything not in anthropicAllowedStatuses still fails closed, same
// reasoning as before this fix: the measured vocabulary is not exhaustively
// known, and the two ways to be wrong are not equally bad. Treating an
// unrecognised status as a rejection costs one account an undeserved
// rotation — every other account in the pool is still tried. Treating it as
// transient instead means retrying the same spent account three times with
// backoff while a healthy one sits idle, which is the exact failure #25
// reports; an unknown future status must not be able to reproduce that.
// Fail toward rotation, not toward retry.
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
		if !anthropicAllowedStatuses[v] {
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

// The three headers below are exported for the proxy's hideOverageFromClient
// strip (issue #103): they are the response signals Claude Code's usage-credit
// gate latches on, and the proxy must delete exactly the spellings this
// package reads, not a second hand-typed copy that could drift.
const (
	// OverageInUseHeader says this response was served on paid extra usage.
	// Read by anthropicOverage below; on the client it is the 200-path latch
	// input for the credit gate (issue #103).
	OverageInUseHeader = "anthropic-ratelimit-unified-overage-in-use"
	// OverageDisabledReasonHeader carries why extra usage will not serve.
	// Read by anthropicOverage below; the client caches it and it feeds the
	// same gate (issue #103).
	OverageDisabledReasonHeader = "anthropic-ratelimit-unified-overage-disabled-reason"
	// RepresentativeClaimHeader is anthropicRepresentativeClaimHeader's
	// exported name — one spelling, shared with the proxy.
	RepresentativeClaimHeader = anthropicRepresentativeClaimHeader
)

// anthropicRepresentativeClaimHeader carries Anthropic's own answer to
// "which window governed this response" (issue #53) — as opposed to
// claudeGoverningWindows above, which only guesses which windows COULD
// govern a request, from the model name, before any response exists.
const anthropicRepresentativeClaimHeader = "anthropic-ratelimit-unified-representative-claim"

// representativeClaimWindows translates the header's snake_case vocabulary
// into spillway's own window names (issue #53).
//
// All three entries are measured, not guessed — issue #234 pinned the two
// added after #53 shipped by reading live logs from 2026-09-15 through
// 2026-09-28: "seven_day" recurs across every model family (opus-5,
// opus-5-5, sonnet-5, haiku-4-5, fable-5-1), while "seven_day_overage_included"
// appears only on claude-fable-5-1 (and one probe with no model). That
// split matches claudeWindows' own "7d_oi" ("overage included") header
// prefix for what spillway names "7d-fable" — fable's extra weekly bucket
// on top of the account-wide 7d — so seven_day_overage_included maps to
// "7d-fable", not to a second "7d". Anything still outside this map is
// genuinely unmeasured, not merely unguessed: AnthropicRepresentativeClaim
// below reports it as unrecognised rather than pretending to translate it,
// and callers must log that as "unknown", never as a mismatch — a mismatch
// claims to know what the value should have been, and for anything not
// listed here we don't.
var representativeClaimWindows = map[string]string{
	"five_hour":                  "5h",       // measured live, issue #53
	"seven_day":                  "7d",       // measured live, issue #234
	"seven_day_overage_included": "7d-fable", // measured live, issue #234 — fable-5-1 only
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
// reset header of only the named windows (issue #54), and — within that
// already-narrowed set — taking the SOONEST of their resets, not the
// longest (issue #90).
//
// #54's scoping to only the windows that actually fired is unchanged and
// still fixes its own bug on its own: a 5h-only rejection (5h rejected, 7d
// allowed) never sees 7d's header at all here, because `windows` doesn't
// name it — see TestResetHintScopesToRejectedWindowOnly.
//
// What issue #90 fixes is the aggregation WITHIN that set when more than
// one named window actually fired together. Before this, a combined
// rejection (e.g. 5h AND 7d both rejected in the same response) took the
// MAX of their resets, so the far-off weekly window won and benched the
// account for its full three days even though the 5h window — the one
// that actually clears first — reset an hour later and was healthy.
// Measured live (2026-08-27 11:41:20, account=metawin): sentenced to
// 2026-08-30T07:00:00Z (the 7d reset) when the 5h reset, an hour out, was
// the binding constraint. The account becomes usable again as soon as its
// SOONEST rejected window clears; if a longer one is still in force the
// worst case is one more 429 costing a rotation, not days of a missing
// account.
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
		v := h.Get("anthropic-ratelimit-unified-" + w.prefix + "-reset")
		if v == "" {
			continue
		}
		sec, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		t := time.Unix(int64(sec), 0)
		if !t.After(now) {
			// Already passed — not a real future bound to weigh against
			// the others, and never the reason to fall back to a stale
			// zero value here (retryAfter/1h below are for "no reset
			// header at all", not "the header we found was in the past").
			continue
		}
		if reset.IsZero() || t.Before(reset) {
			reset = t
		}
	}
	if !reset.IsZero() {
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
