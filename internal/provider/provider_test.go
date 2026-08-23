package provider

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// TestClaudeClassification covers anthropicRejected (issue #25): a fable
// weekly rejection (header prefix "7d_oi") must be recognised as a quota
// rejection exactly like 5h/7d, an absent family header must never be read
// as a rejection, and an unrecognised status value must fail toward quota
// (rotate) rather than toward transient (retry a possibly-spent account).
// Header names and values are the live-measured set from #25's comment.
func TestClaudeClassification(t *testing.T) {
	c := For("claude-oauth")

	tests := []struct {
		name string
		set  map[string]string
		want ErrKind
	}{
		{
			name: "5h rejected",
			set:  map[string]string{"anthropic-ratelimit-unified-5h-status": "rejected"},
			want: ErrQuota,
		},
		{
			name: "7d rejected",
			set:  map[string]string{"anthropic-ratelimit-unified-7d-status": "rejected"},
			want: ErrQuota,
		},
		{
			// The bug #25 reports: fable's own weekly bucket, distinct from
			// the account-wide 5h/7d windows anthropicRejected used to check.
			name: "7d_oi (fable) rejected",
			set:  map[string]string{"anthropic-ratelimit-unified-7d_oi-status": "rejected"},
			want: ErrQuota,
		},
		{
			name: "all allowed",
			set: map[string]string{
				"anthropic-ratelimit-unified-5h-status":    "allowed",
				"anthropic-ratelimit-unified-7d-status":    "allowed",
				"anthropic-ratelimit-unified-7d_oi-status": "allowed",
			},
			want: ErrRate,
		},
		{
			// Haiku, measured live (#25): no 7d_oi-* headers at all because
			// the fable family was never engaged on that request. Absent
			// must mean "this family wasn't involved", never "rejected".
			name: "7d_oi header absent entirely (haiku case)",
			set: map[string]string{
				"anthropic-ratelimit-unified-5h-status": "allowed",
				"anthropic-ratelimit-unified-7d-status": "allowed",
			},
			want: ErrRate,
		},
		{
			// The status vocabulary isn't exhaustively known (only "allowed"
			// and "rejected" are measured so far). An unrecognised value
			// must fail toward quota — one account rotated undeservedly —
			// never toward transient, which would retry a possibly-spent
			// account 3x with backoff, the exact failure #25 reports.
			name: "unknown status value",
			set:  map[string]string{"anthropic-ratelimit-unified-7d_oi-status": "some_future_status"},
			want: ErrQuota,
		},
		{
			name: "no rate-limit headers at all",
			set:  map[string]string{},
			want: ErrRate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			// Set(), not a literal map: Go canonicalises "5h" with a
			// lowercase h, so a hand-written key silently fails Get().
			for k, v := range tt.set {
				h.Set(k, v)
			}
			if got := c.Classify(429, h, nil); got != tt.want {
				t.Errorf("Classify = %v, want %v", got, tt.want)
			}
		})
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

// TestAnthropicWindowsFromHeadersIncludesFable pins the "7d_oi" header
// prefix to spillway's "7d-fable" window name (issue #25), and confirms an
// absent 7d_oi-* (the measured Haiku case) yields no fable window at all
// rather than a zero-valued one.
func TestAnthropicWindowsFromHeadersIncludesFable(t *testing.T) {
	now := time.Now()
	reset := now.Add(48 * time.Hour).Unix()
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.01")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.02")
	h.Set("anthropic-ratelimit-unified-7d_oi-utilization", "0.02")
	h.Set("anthropic-ratelimit-unified-7d_oi-reset", itoa(reset))

	w := For("claude-oauth").WindowsFromHeaders(h, now)
	if len(w) != 3 {
		t.Fatalf("want 3 windows, got %d: %+v", len(w), w)
	}
	if w[2].Name != "7d-fable" || w[2].Used != 0.02 || w[2].Limit != 1 {
		t.Errorf("7d-fable window wrong: %+v", w[2])
	}
	if !w[2].ResetAt.Equal(time.Unix(reset, 0)) {
		t.Errorf("7d-fable reset not parsed: %v", w[2].ResetAt)
	}

	// The measured Haiku case: no 7d_oi-* headers at all.
	haiku := http.Header{}
	haiku.Set("anthropic-ratelimit-unified-5h-utilization", "0.05")
	haiku.Set("anthropic-ratelimit-unified-7d-utilization", "0.05")
	wh := For("claude-oauth").WindowsFromHeaders(haiku, now)
	if len(wh) != 2 {
		t.Fatalf("haiku request must not synthesize a fable window, got %d: %+v", len(wh), wh)
	}
}

func TestResetHintPrefersProviderSignal(t *testing.T) {
	now := time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC)

	// Kimi has no reset headers; it falls back to its polled quota state.
	// windows is always empty for Kimi (no header-scoped families) — passed
	// as nil here the same as a real caller would.
	polled := now.Add(90 * time.Minute)
	if got := For("kimi-oauth").ResetHint(http.Header{}, nil, now, polled); !got.Equal(polled) {
		t.Errorf("kimi should use its polled reset, got %v", got)
	}
	// With nothing to go on, an hour rather than forever.
	if got := For("kimi-oauth").ResetHint(http.Header{}, nil, now, time.Time{}); !got.Equal(now.Add(time.Hour)) {
		t.Errorf("kimi default = %v, want +1h", got)
	}
	// Anthropic reads its own headers and ignores the fallback, restricted
	// to the windows named — here just "5h", the one that was rejected.
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-reset", itoa(now.Add(30*time.Minute).Unix()))
	if got := For("claude-oauth").ResetHint(h, []string{"5h"}, now, polled); !got.Equal(now.Add(30 * time.Minute)) {
		t.Errorf("claude should use its header, got %v", got)
	}
}

// TestResetHintScopesToRejectedWindowOnly is issue #54's third defect,
// reproduced directly: a 5h-only rejection (5h rejected, 7d allowed and
// far out) must bound the deadline to 5h's own reset, not the max across
// every window this account has. Before the fix, anthropicReset always
// scanned both 5h and 7d regardless of which was actually rejected, so a
// 5h-only rejection silently borrowed 7d's ~144h-out reset — measured live
// on main (2026-08-22): the account sat out 144h instead of ~2h.
func TestResetHintScopesToRejectedWindowOnly(t *testing.T) {
	now := time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-status", "rejected")
	h.Set("anthropic-ratelimit-unified-5h-reset", itoa(now.Add(2*time.Hour).Unix()))
	h.Set("anthropic-ratelimit-unified-7d-status", "allowed")
	h.Set("anthropic-ratelimit-unified-7d-reset", itoa(now.Add(144*time.Hour).Unix()))

	rejected := For("claude-oauth").RejectedWindows(h)
	if len(rejected) != 1 || rejected[0] != "5h" {
		t.Fatalf("RejectedWindows = %v, want exactly [5h]", rejected)
	}

	got := For("claude-oauth").ResetHint(h, rejected, now, time.Time{})
	want := now.Add(2 * time.Hour)
	if !got.Equal(want) {
		t.Errorf("ResetHint(rejected=%v) = %v, want %v (5h's own reset, not 7d's 144h)", rejected, got, want)
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

// Every provider lives in the registry, and nothing outside it should have to
// name one. This guards the shape rather than any single behaviour: the last
// two branches were `a.Type != "claude-oauth"` in resolve.go and a refresh
// dispatch, and the first came back after being removed once.
func TestNoProviderNamedOutsideTheRegistry(t *testing.T) {
	root := ".."
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// The registry is allowed to name providers; that is its job.
			if info.Name() == "provider" || info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, src, 0) // comments excluded
		if perr != nil {
			return nil // not our business here
		}
		ast.Inspect(f, func(n ast.Node) bool {
			bin, ok := n.(*ast.BinaryExpr)
			if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
				return true
			}
			for _, side := range []ast.Expr{bin.X, bin.Y} {
				lit, ok := side.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if v := strings.Trim(lit.Value, `"`); v == "claude-oauth" || v == "kimi-oauth" {
					offenders = append(offenders,
						fmt.Sprintf("%s:%d compares against %q", path, fset.Position(lit.Pos()).Line, v))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offenders {
		t.Errorf("%s — put the decision in the registry instead", o)
	}
}

// Issue #24: Claude reports separate quota buckets per model family, and the
// map from model to governing window(s) is provider knowledge, tested at the
// registry rather than through the pool's selection behaviour alone.
func TestClaudeGoverningWindows(t *testing.T) {
	gw := claudeSpec.GoverningWindows
	if gw == nil {
		t.Fatal("claudeSpec.GoverningWindows is nil")
	}

	has := func(windows []string, name string) bool {
		for _, w := range windows {
			if w == name {
				return true
			}
		}
		return false
	}

	for _, tc := range []struct {
		model     string
		wantFable bool
	}{
		{"claude-sonnet-4-6", false},
		{"claude-opus-5", false},
		{"claude-haiku-4-5-20251001", false},
		// Unrecognised entirely — must resolve to the general windows, never
		// be silently treated as fable.
		{"some-future-model-nobody-has-heard-of", false},
		{"", false},
		{"claude-opus-4-fable-preview", true},
		{"FABLE", true}, // case-insensitive
	} {
		windows := gw(tc.model)
		if !has(windows, "5h") || !has(windows, "7d") {
			t.Errorf("GoverningWindows(%q) = %v, want 5h and 7d present", tc.model, windows)
		}
		if got := has(windows, "7d-fable"); got != tc.wantFable {
			t.Errorf("GoverningWindows(%q) 7d-fable presence = %v, want %v", tc.model, got, tc.wantFable)
		}
	}
}

// Kimi has no family-scoped buckets at all — the map must say so (nil)
// rather than pretend to know, so callers fall back to checking every
// recorded window instead of guessing which one governs.
func TestKimiHasNoGoverningWindows(t *testing.T) {
	if kimiSpec.GoverningWindows != nil {
		t.Error("kimiSpec.GoverningWindows should be nil: Kimi reports no per-family buckets")
	}
}

// Kimi's classification is body-based (kimiSpec.Classify); it has no
// per-window header signal to name a rejection by, so RejectedWindows must
// be nil too (issue #54) — a caller checking for nil is how it knows to
// degrade to account-wide behaviour instead of trying to narrow with
// nothing to narrow by.
func TestKimiHasNoRejectedWindows(t *testing.T) {
	if kimiSpec.RejectedWindows != nil {
		t.Error("kimiSpec.RejectedWindows should be nil: Kimi has no header-scoped windows")
	}
}

// TestClaudeRejectedWindowsNamesExactlyWhatFired is issue #54's widening of
// #25's bool to actual names, asserted directly against RejectedWindows
// (Classify's ErrQuota/ErrRate split already covers the bool behaviour in
// TestClaudeClassification above).
func TestClaudeRejectedWindowsNamesExactlyWhatFired(t *testing.T) {
	rw := claudeSpec.RejectedWindows
	if rw == nil {
		t.Fatal("claudeSpec.RejectedWindows is nil")
	}

	eq := func(t *testing.T, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}

	t.Run("5h only", func(t *testing.T) {
		h := http.Header{}
		h.Set("anthropic-ratelimit-unified-5h-status", "rejected")
		h.Set("anthropic-ratelimit-unified-7d-status", "allowed")
		eq(t, rw(h), []string{"5h"})
	})

	t.Run("5h and fable both rejected, 7d allowed", func(t *testing.T) {
		h := http.Header{}
		h.Set("anthropic-ratelimit-unified-5h-status", "rejected")
		h.Set("anthropic-ratelimit-unified-7d-status", "allowed")
		h.Set("anthropic-ratelimit-unified-7d_oi-status", "rejected")
		eq(t, rw(h), []string{"5h", "7d-fable"})
	})

	t.Run("fable only", func(t *testing.T) {
		h := http.Header{}
		h.Set("anthropic-ratelimit-unified-5h-status", "allowed")
		h.Set("anthropic-ratelimit-unified-7d-status", "allowed")
		h.Set("anthropic-ratelimit-unified-7d_oi-status", "rejected")
		eq(t, rw(h), []string{"7d-fable"})
	})

	// The measured Haiku case (#25): no 7d_oi-* headers at all must never
	// be read as a rejected name, only as "not engaged".
	t.Run("haiku: fable header absent entirely", func(t *testing.T) {
		h := http.Header{}
		h.Set("anthropic-ratelimit-unified-5h-status", "allowed")
		h.Set("anthropic-ratelimit-unified-7d-status", "allowed")
		got := rw(h)
		if len(got) != 0 {
			t.Errorf("got %v, want no rejected windows", got)
		}
	})
}
