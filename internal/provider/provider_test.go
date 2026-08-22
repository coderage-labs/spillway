package provider

import (
	"net/http"
	"testing"
	"time"
)

// An unknown or absent type must behave as Anthropic — that is the API every
// account here claims to speak, and configs predate the type field.
func TestForFallsBackToClaude(t *testing.T) {
	for _, in := range []string{"", "unknown-provider"} {
		if got := For(in).Kind; got != Claude {
			t.Errorf("For(%q).Kind = %q, want %q", in, got, Claude)
		}
		if For(in).RefreshFlavour != AnthropicOAuth {
			t.Errorf("For(%q) should refresh as anthropic", in)
		}
	}
	if Known("") || Known("unknown-provider") {
		t.Error("fallback must not make an unknown type valid for config")
	}
}

// The measured Kimi behaviours (doc §6.19), asserted at the registry so a
// future edit cannot quietly drop one.
func TestKimiClassification(t *testing.T) {
	k := For("kimi-oauth")
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"usage limit 429 is quota", 429, `{"error":{"message":"usage limit reached"}}`, ErrQuota},
		{"plain 429 is a throttle", 429, `{"error":"too many concurrent"}`, ErrRate},
		{"403 billing is quota", 403, `{"error":"billing quota exhausted"}`, ErrQuota},
		{"403 otherwise is not ours", 403, `{"error":"forbidden"}`, ErrNone},
		// Measured: an unknown model id arrives as 401. Treating it as auth
		// failure would disable a healthy account.
		{"401 model id is not auth death", 401,
			`{"error":{"message":"Your model id does not exist, recognized as other:k3[1m]"}}`, ErrModelID},
		{"401 otherwise is not classified here", 401, `{"error":"invalid bearer"}`, ErrNone},
	} {
		if got := k.Classify(tc.status, http.Header{}, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClaudeClassification(t *testing.T) {
	c := For("claude-oauth")
	// Set(), not a literal map: Go canonicalises "5h" with a lowercase h, so
	// a hand-written key silently fails to match Get().
	rejected := http.Header{}
	rejected.Set("anthropic-ratelimit-unified-5h-status", "rejected")
	if got := c.Classify(429, rejected, nil); got != ErrQuota {
		t.Errorf("rejected status = %v, want quota", got)
	}
	if got := c.Classify(429, http.Header{}, nil); got != ErrRate {
		t.Errorf("bare 429 = %v, want rate", got)
	}
	// Anthropic does not overload 401 the way Kimi does.
	if got := c.Classify(401, http.Header{}, []byte("model id")); got != ErrNone {
		t.Errorf("401 = %v, want none", got)
	}
}

// Reading a body costs a buffered response, so only statuses a provider
// actually classifies should trigger it.
func TestClassifiableStatuses(t *testing.T) {
	if For("claude-oauth").Classifiable(403) {
		t.Error("claude does not classify 403; reading its body buffers for nothing")
	}
	if !For("kimi-oauth").Classifiable(403) {
		t.Error("kimi carries quota in 403")
	}
	for _, s := range []int{429, 401} {
		if !For("kimi-oauth").Classifiable(s) || !For("claude-oauth").Classifiable(s) {
			t.Errorf("status %d should be classifiable for both", s)
		}
	}
	if For("claude-oauth").Classifiable(200) {
		t.Error("200 is not a failure")
	}
}

func TestAnthropicWindowsFromHeaders(t *testing.T) {
	now := time.Now()
	reset := now.Add(2 * time.Hour).Unix()
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
	h.Set("anthropic-ratelimit-unified-5h-reset", itoa(reset))
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.10")

	w := For("claude-oauth").WindowsFromHeaders(h, now)
	if len(w) != 2 {
		t.Fatalf("want 2 windows, got %d: %+v", len(w), w)
	}
	if w[0].Name != "5h" || w[0].Used != 0.42 || w[0].Limit != 1 {
		t.Errorf("5h window wrong: %+v", w[0])
	}
	if !w[0].ResetAt.Equal(time.Unix(reset, 0)) {
		t.Errorf("reset not parsed: %v", w[0].ResetAt)
	}
	// A window the provider did not report must be absent, not zero-valued —
	// "no data" and "no headroom" are different things.
	if len(For("kimi-oauth").specWindows(h, now)) != 0 {
		t.Error("kimi reports usage out-of-band, not in headers")
	}
}

// specWindows is nil-safe access for the test above.
func (s Spec) specWindows(h http.Header, now time.Time) []Window {
	if s.WindowsFromHeaders == nil {
		return nil
	}
	return s.WindowsFromHeaders(h, now)
}

func TestResetHintPrefersProviderSignal(t *testing.T) {
	now := time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC)

	// Kimi has no reset headers; it falls back to its polled quota state.
	polled := now.Add(90 * time.Minute)
	if got := For("kimi-oauth").ResetHint(http.Header{}, now, polled); !got.Equal(polled) {
		t.Errorf("kimi should use its polled reset, got %v", got)
	}
	// With nothing to go on, an hour rather than forever.
	if got := For("kimi-oauth").ResetHint(http.Header{}, now, time.Time{}); !got.Equal(now.Add(time.Hour)) {
		t.Errorf("kimi default = %v, want +1h", got)
	}
	// Anthropic reads its own headers and ignores the fallback.
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-reset", itoa(now.Add(30*time.Minute).Unix()))
	if got := For("claude-oauth").ResetHint(h, now, polled); !got.Equal(now.Add(30 * time.Minute)) {
		t.Errorf("claude should use its header, got %v", got)
	}
}

func TestProbeModelPrefersConfiguredMapping(t *testing.T) {
	if got := For("kimi-oauth").ProbeModel(nil); got != "k3" {
		t.Errorf("kimi default probe model = %q", got)
	}
	m := map[string]string{"claude-haiku-4-5-20251001": "kimi-for-coding"}
	if got := For("kimi-oauth").ProbeModel(m); got != "kimi-for-coding" {
		t.Errorf("modelMap should win, got %q", got)
	}
	if got := For("claude-oauth").ProbeModel(nil); got != "claude-haiku-4-5-20251001" {
		t.Errorf("claude default probe model = %q", got)
	}
}

func TestTypesIsStableAndComplete(t *testing.T) {
	got := Types()
	want := []string{"claude-oauth", "kimi-oauth"}
	if len(got) != len(want) {
		t.Fatalf("Types() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Types()[%d] = %q, want %q (order must be stable for messages)", i, got[i], want[i])
		}
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// The exact headers a live account with working extra usage returned
// (2026-08-22). Captured because the previous parser, written from the
// "rejected" shape alone, read this as having no extra usage at all — so
// spillway served a billed request and logged it as ordinary traffic.
func TestAnthropicOverageReadsTheLiveAllowedShape(t *testing.T) {
	live := http.Header{
		"Anthropic-Ratelimit-Unified-Status":                      []string{"rejected"},
		"Anthropic-Ratelimit-Unified-Overage-Status":              []string{"allowed_warning"},
		"Anthropic-Ratelimit-Unified-Overage-In-Use":              []string{"true"},
		"Anthropic-Ratelimit-Unified-Overage-Utilization":         []string{"0.98"},
		"Anthropic-Ratelimit-Unified-Overage-Surpassed-Threshold": []string{"0.95"},
		"Anthropic-Ratelimit-Unified-Overage-Reset":               []string{"1788220800"},
	}
	ov := For("claude-oauth").OverageFromHeaders(live)

	// allowed_warning is not a near-miss for allowed: it means allowed AND
	// past the warning threshold.
	if !ov.Known || !ov.Available {
		t.Fatalf("overage = %+v, want available — this account was serving billed requests", ov)
	}
	if !ov.InUse {
		t.Error("In-Use not read; it is the only after-the-fact proof a request was billed")
	}
	if ov.Utilization != 0.98 {
		t.Errorf("utilization = %v, want 0.98 — this is how close the cap is", ov.Utilization)
	}
	if ov.ResetAt.Unix() != 1788220800 {
		t.Errorf("resetAt = %v, want the billing-period reset", ov.ResetAt)
	}
}

// In-use settles it even if the status is missing or unrecognised: the
// provider has just served a billed request.
func TestOverageInUseImpliesAvailable(t *testing.T) {
	ov := For("claude-oauth").OverageFromHeaders(http.Header{
		"Anthropic-Ratelimit-Unified-Overage-In-Use": []string{"true"},
	})
	if !ov.Known || !ov.Available || !ov.InUse {
		t.Errorf("overage = %+v, want available and in use", ov)
	}
}

// Unrecognised statuses still fail closed — the fix widens the allowed set,
// it does not abandon the default.
func TestUnrecognisedOverageStatusStillFailsClosed(t *testing.T) {
	for _, status := range []string{"pending_review", "throttled", "ALLOWED", "allowed_soon"} {
		ov := For("claude-oauth").OverageFromHeaders(http.Header{
			"Anthropic-Ratelimit-Unified-Overage-Status": []string{status},
		})
		if ov.Available {
			t.Errorf("status %q was read as permission to bill", status)
		}
	}
}

// The rejected shape, unchanged.
func TestAnthropicOverageReadsTheRejectedShape(t *testing.T) {
	ov := For("claude-oauth").OverageFromHeaders(http.Header{
		"Anthropic-Ratelimit-Unified-Overage-Status":          []string{"rejected"},
		"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": []string{"member_zero_credit_limit"},
	})
	if !ov.Known || ov.Available || ov.InUse {
		t.Errorf("overage = %+v, want known and unavailable", ov)
	}
	if ov.Reason != "member_zero_credit_limit" {
		t.Errorf("reason = %q", ov.Reason)
	}
}

// allowed_warning ALONE, with no In-Use to rescue it.
//
// The live-shape test above carries both, so it passes even with
// allowed_warning removed from the allowed set — In-Use implies available.
// This isolates the status set, which is the half that decides whether an
// account is reachable at all before any billed request has happened.
func TestAllowedWarningIsAvailableWithoutInUse(t *testing.T) {
	ov := For("claude-oauth").OverageFromHeaders(http.Header{
		"Anthropic-Ratelimit-Unified-Overage-Status": []string{"allowed_warning"},
	})
	if !ov.Known || !ov.Available {
		t.Errorf("overage = %+v — allowed_warning means allowed and past the "+
			"warning threshold, not a near-miss for allowed", ov)
	}
	if ov.InUse {
		t.Error("InUse set with no In-Use header")
	}
}
