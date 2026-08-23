package proxy

// Tests for issue #54: a quota-429 must be scoped to the window that
// actually rejected it, not blanket-applied to the whole account and not
// bounded by the max reset across every window this account has.

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

// accountWideRejection is proxy.go's own scope decision (issue #54): it
// must derive "account-wide" from the provider's general GoverningWindows
// set, and degrade to today's whole-account behaviour when a provider has
// no per-window signal at all.
func TestAccountWideRejectionScoping(t *testing.T) {
	tests := []struct {
		name        string
		acctType    string
		rejected    []string
		wantWide    bool
		wantWideSet []string
	}{
		{"claude 5h only", "claude-oauth", []string{"5h"}, true, []string{"5h"}},
		{"claude 7d only", "claude-oauth", []string{"7d"}, true, []string{"7d"}},
		{"claude fable only", "claude-oauth", []string{"7d-fable"}, false, nil},
		{"claude mixed 5h+fable", "claude-oauth", []string{"5h", "7d-fable"}, true, []string{"5h"}},
		// Kimi has no RejectedWindows/GoverningWindows: no signal to
		// narrow by, must degrade to account-wide (never "never exhaust").
		{"kimi degrades to account-wide", "kimi-oauth", []string{"anything"}, true, []string{"anything"}},
		// Defensive: empty rejected set reaching here at all (should not
		// happen — Classify uses the same RejectedWindows) fails toward
		// the wider, safer scope.
		{"claude empty rejected set", "claude-oauth", nil, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wide, wideNames := accountWideRejection(tt.acctType, tt.rejected)
			if wide != tt.wantWide {
				t.Errorf("wide = %v, want %v", wide, tt.wantWide)
			}
			if len(wideNames) != len(tt.wantWideSet) {
				t.Fatalf("wideNames = %v, want %v", wideNames, tt.wantWideSet)
			}
			for i, w := range tt.wantWideSet {
				if wideNames[i] != w {
					t.Errorf("wideNames = %v, want %v", wideNames, tt.wantWideSet)
				}
			}
		})
	}
}

// fable429 rejects ONLY the fable weekly bucket: 5h and 7d allowed, 7d_oi
// (fable) rejected with its own (far-out) reset. This is exactly the
// scenario issue #54 exists for — before the fix, this blanket-exhausted
// the account for Sonnet/Haiku too.
func fable429(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Status", "rejected")
	w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Reset", fmt.Sprint(time.Now().Add(48*time.Hour).Unix()))
	w.WriteHeader(http.StatusTooManyRequests)
	fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"fable weekly quota exceeded"}}`)
}

const fableReqBody = `{"model":"claude-opus-4-fable-preview","max_tokens":16,"metadata":{"user_id":"fable-session"},"messages":[]}`

func sonnetReqBody(session string) string {
	return `{"model":"claude-sonnet-4-6","max_tokens":16,"metadata":{"user_id":"` + session + `"},"messages":[]}`
}

// TestFableOnly429DoesNotExhaustAccountForSonnet is the #54 regression
// guard this PR exists for, end to end through the real HTTP path (not a
// direct pool call): a fable-only 429 must not exhaust the account, and a
// later Sonnet request — a fresh session, so it can't ride the sticky
// fast path back onto whatever the fable request rotated to — must still
// select the account that was just fable-rejected.
func TestFableOnly429DoesNotExhaustAccountForSonnet(t *testing.T) {
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) { fable429(w) },
		func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true}`) },
	})
	acct0, acct1 := rig.pool.Accounts()[0], rig.pool.Accounts()[1]
	// Deterministic first pick for the fable request.
	acct0.SetPriority(0)
	acct1.SetPriority(1)

	resp := postMessages(t, rig.front.URL, fableReqBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (rotated to the healthy account)", resp.StatusCode)
	}

	if acct0.State() != pool.StateExhausted && acct0.State() != pool.StateOK {
		t.Fatalf("unexpected state %v", acct0.State())
	}
	if acct0.State() == pool.StateExhausted {
		t.Fatalf("account-0 must not be StateExhausted after a fable-only rejection — that is exactly the #54 bug")
	}

	// A fresh session (not the fable request's) asking for Sonnet must
	// still be able to select account-0: its lower priority wins tier 1
	// as long as WindowRejectedFor(sonnet) correctly reads false.
	got := rig.pool.SelectFor("fresh-sonnet-session", []byte(sonnetReqBody("fresh-sonnet-session")))
	if got == nil {
		t.Fatal("SelectFor(sonnet) = nil, want account-0 (unaffected by the fable rejection)")
	}
	if got.Name != acct0.Name {
		t.Fatalf("SelectFor(sonnet) = %q, want %q (its priority should have won, unless it was wrongly excluded)",
			got.Name, acct0.Name)
	}
	rig.pool.Done(got)
}

// mixed429 rejects BOTH the account-wide 5h window (short reset) and the
// fable weekly window (far-out reset) in the same response.
func mixed429(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", fmt.Sprint(time.Now().Add(2*time.Hour).Unix()))
	w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Status", "rejected")
	w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Reset", fmt.Sprint(time.Now().Add(1000*time.Hour).Unix()))
	w.WriteHeader(http.StatusTooManyRequests)
	fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"quota exceeded"}}`)
}

// A mixed rejection (account-wide AND family together) must still
// exhaust the account (5h is account-wide), bounded to the SHORT 5h
// reset — not the fable window's 1000h-out one — and must also leave a
// standalone fable rejection record, so that once the 5h exhaustion
// clears the fable bucket doesn't silently read as healthy again.
func TestMixedRejectionExhaustsScopedAndKeepsFableRecorded(t *testing.T) {
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) { mixed429(w) },
		func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true}`) },
	})
	acct0, acct1 := rig.pool.Accounts()[0], rig.pool.Accounts()[1]
	acct0.SetPriority(0)
	acct1.SetPriority(1)

	resp := postMessages(t, rig.front.URL, fableReqBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if acct0.State() != pool.StateExhausted {
		t.Fatalf("account-0 state = %v, want StateExhausted (5h is account-wide)", acct0.State())
	}
	until := acct0.ExhaustedUntil()
	if until.After(time.Now().Add(3 * time.Hour)) {
		t.Errorf("ExhaustedUntil = %v, want ~2h out, not borrowing fable's 1000h reset", until)
	}
	if !acct0.WindowRejectedFor("claude-opus-4-fable-preview") {
		t.Error("fable window should still be independently recorded rejected, for after the 5h exhaustion clears")
	}
}
