package proxy

// Hiding one account's credit state from the client (issue #103).
//
// Claude Code carries a usage-credit gate for its top model family: a
// process-local latch, set from response signals, that silently swaps the
// session's model down (Fable -> Opus) and clears only on CLI restart or an
// explicit /model — never when a quota window resets. Its inputs, read off
// the 2.1.252 binary rather than guessed: a 200 to a fable request whose
// headers say overage-in-use, a 429 whose representative-claim names the
// fable weekly bucket, and a 429 body whose error details carry
// error_code "credits_required". The utilization headers play no part —
// they feed the /usage display only.
//
// Behind a pool those signals describe one account, but the client reads
// them as the world. Measured live (2026-08-31 20:04): the overage tier
// served a fable request from the one billable account at a moment the
// whole pool was legitimately fable-dry, the response's overage-in-use
// latched the client, and when another account's fable window reset hours
// later the session kept asking for Opus indefinitely — rotation never got
// a fable request to act on, which is issue #103's gap.
//
// So, opt-in (pool.hideOverageFromClient), the proxy deletes exactly those
// latch inputs from pooled Claude responses. Spillway's own bookkeeping is
// untouched: RecordQuota, the overage warning/notification, and EventOverage
// all read the response before route() writes anything to the client, so the
// strip blinds only the client — "spending is never silent" still holds.
// What changes hands is the consent authority: the CLI's per-account credit
// dialog cannot make a sane decision for a pool, and with this flag on it is
// pool.allowOverage (off by default, fail-closed, per-account overridable)
// that decides whether spillway ever serves a billed request.
//
// Passthrough traffic (identity-bound and non-quota paths) never comes
// through route(), so it is never stripped — those responses describe the
// client's own account and remain byte-faithful.

import (
	"encoding/json"
	"net/http"

	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
)

// fableCreditClaim is the representative-claim value naming the fable weekly
// bucket ("seven_day_overage_included" — the CLI's own translation table maps
// it to the 7d_oi header family). A claim naming any other window is kept:
// it is information, and only this value participates in the credit latch.
const fableCreditClaim = "seven_day_overage_included"

// creditsRequiredCode is the 429 body marker for "buy credits to continue".
// On the client it arms the same latch the headers do, and it does so even
// when the representative-claim header is absent — which is why headers and
// body must be neutralized together, never just one of them.
const creditsRequiredCode = "credits_required"

// hidingCreditSignals reports whether this response's credit markers are to
// be hidden: the flag is on, and the account is a Claude one — the markers
// are Anthropic's vocabulary, and Anthropic semantics must never be applied
// to a body another provider produced.
//
// Asked per response, not snapshotted at handler construction, so a
// dashboard settings change applies to the next request like every other
// pool setting (issue #13's Apply is the only writer).
func (h *Handler) hidingCreditSignals(acct *pool.Account) bool {
	return h.pool.HideOverageFromClient() &&
		provider.For(acct.Type).Kind == provider.Claude
}

// stripCreditSignals deletes the client-latch inputs from a pooled response's
// headers. Deliberately NOT stripped: overage-status, overage-utilization,
// and every window utilization/reset header — none of them is a latch input,
// and the mutation stays as small as the mechanism requires.
func stripCreditSignals(hd http.Header) {
	hd.Del(provider.OverageInUseHeader)
	hd.Del(provider.OverageDisabledReasonHeader)
	if hd.Get(provider.RepresentativeClaimHeader) == fableCreditClaim {
		hd.Del(provider.RepresentativeClaimHeader)
	}
}

// neutralizeCreditsRequired removes the "credits_required" error_code from a
// relayed 429 body, checking both nestings observed in the wild
// (error.details and error.error.details). Deleting the key is the least
// invention that works: the client tests the code for equality, and absence
// never latches. Key order changing on re-marshal is acceptable — the
// byte-fidelity invariant (§4, replay_test.go) guards what spillway SENDS,
// where Anthropic fingerprints the request; no such contract exists for an
// error body the client merely parses.
//
// Any body this function cannot confidently edit — not JSON, no error
// object, no matching code — comes back unchanged. A malformed upstream
// error must reach the client as-is, never be replaced by a proxy-made one.
func neutralizeCreditsRequired(body []byte) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	e, ok := m["error"].(map[string]any)
	if !ok {
		return body
	}
	changed := dropCreditsCode(e)
	if inner, ok := e["error"].(map[string]any); ok {
		changed = dropCreditsCode(inner) || changed
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func dropCreditsCode(e map[string]any) bool {
	d, ok := e["details"].(map[string]any)
	if !ok || d["error_code"] != creditsRequiredCode {
		return false
	}
	delete(d, "error_code")
	return true
}
