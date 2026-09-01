package proxy

// End-to-end for hiding credit signals (issue #103).
//
// The pool tests prove selection; overage_e2e proves spending is recorded.
// These prove the strip: with hideOverageFromClient on, the client-latch
// inputs never leave the proxy, while spillway's own reading of the same
// response — taken before the strip — is intact. The default-off cases are
// as load-bearing as the on cases: TestResponseFidelity's byte-faithfulness
// only survives because this feature does nothing until asked.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
)

// creditHeaders makes an upstream handler that answers 200 with the full set
// of credit markers plus one utilization header the strip must NOT touch.
func creditHeaders(w http.ResponseWriter) {
	w.Header().Set("Anthropic-Ratelimit-Unified-Overage-In-Use", "true")
	w.Header().Set("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason", "out_of_credits")
	w.Header().Set("Anthropic-Ratelimit-Unified-Overage-Status", "allowed_warning")
	w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.5")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","content":[]}`))
}

func hideOverage(p *pool.Pool, on bool) {
	p.Apply(pool.Settings{SwitchThreshold: 0.98, HideOverageFromClient: on})
}

func TestHideOverageStripsCreditSignalsFromClientResponse(t *testing.T) {
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, _ *http.Request) { creditHeaders(w) },
		func(w http.ResponseWriter, _ *http.Request) { creditHeaders(w) },
	})
	hideOverage(rig.pool, true)

	resp := postMessages(t, rig.front.URL, testBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, hd := range []string{
		"Anthropic-Ratelimit-Unified-Overage-In-Use",
		"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason",
	} {
		if v := resp.Header.Get(hd); v != "" {
			t.Errorf("%s = %q reached the client; it is a latch input and must be stripped", hd, v)
		}
	}
	// Not latch inputs — the mutation must stay as small as the mechanism.
	if v := resp.Header.Get("Anthropic-Ratelimit-Unified-7d-Utilization"); v != "0.5" {
		t.Errorf("7d-utilization = %q, want it untouched — display headers are not stripped", v)
	}
	if v := resp.Header.Get("Anthropic-Ratelimit-Unified-Overage-Status"); v != "allowed_warning" {
		t.Errorf("overage-status = %q, want it untouched", v)
	}

	// The strip must blind only the client: spillway's own reading of the
	// SAME response (RecordQuota runs before the strip) still says billed.
	sawInUse := false
	for _, a := range rig.pool.Accounts() {
		if a.Overage().InUse {
			sawInUse = true
		}
	}
	if !sawInUse {
		t.Error("no account recorded Overage().InUse — the strip blinded spillway's own bookkeeping")
	}
}

func TestOverageHeadersReachClientByDefault(t *testing.T) {
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, _ *http.Request) { creditHeaders(w) },
		func(w http.ResponseWriter, _ *http.Request) { creditHeaders(w) },
	})

	resp := postMessages(t, rig.front.URL, testBody)
	defer resp.Body.Close()
	if v := resp.Header.Get("Anthropic-Ratelimit-Unified-Overage-In-Use"); v != "true" {
		t.Errorf("overage-in-use = %q with the flag off, want the verbatim %q — default off is what keeps responses byte-faithful", v, "true")
	}
	if v := resp.Header.Get("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason"); v != "out_of_credits" {
		t.Errorf("overage-disabled-reason = %q, want verbatim %q", v, "out_of_credits")
	}
}

func TestHideOverageLiveReloadFlipsWithoutRestart(t *testing.T) {
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, _ *http.Request) { creditHeaders(w) },
		func(w http.ResponseWriter, _ *http.Request) { creditHeaders(w) },
	})

	resp := postMessages(t, rig.front.URL, testBody)
	resp.Body.Close()
	if resp.Header.Get("Anthropic-Ratelimit-Unified-Overage-In-Use") != "true" {
		t.Fatal("flag off: header should pass through before the flip")
	}

	hideOverage(rig.pool, true) // the settings path, no handler rebuild

	resp = postMessages(t, rig.front.URL, testBody)
	resp.Body.Close()
	if v := resp.Header.Get("Anthropic-Ratelimit-Unified-Overage-In-Use"); v != "" {
		t.Errorf("overage-in-use = %q after Apply flipped the flag; a settings change must reach the next response without a restart", v)
	}
}

// A 429 that carries the fable representative-claim AND the credits_required
// body is the second latch path. Header and body travel together: the claim
// goes, the error_code goes, and everything else in the body survives.
func TestTerminal429CreditMarkersNeutralized(t *testing.T) {
	const errBody = `{"type":"error","error":{"type":"rate_limit_error","message":"credits required",` +
		`"details":{"error_code":"credits_required","disabled_reason":"out_of_credits"}}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven_day_overage_included")
		// No window-status headers: this is a credit refusal, not a window
		// rejection, so it classifies ErrRate and relays after the inline
		// retries — the exact path issue #103's body rewrite exists for.
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(errBody))
	}))
	t.Cleanup(up.Close)

	acct := pool.NewAccount("only", pool.SourceYAML, "tok", "", 0, up.URL)
	p := pool.New([]*pool.Account{acct}, time.Now())
	hideOverage(p, true)
	cfg := config.Defaults()
	cfg.Pool.ExhaustedMode = "fail"
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)

	resp := postMessages(t, front.URL, testBody) // ~3s: three 1s inline retries first
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the relayed 429", resp.StatusCode)
	}
	if v := resp.Header.Get("Anthropic-Ratelimit-Unified-Representative-Claim"); v != "" {
		t.Errorf("representative-claim = %q reached the client; the fable value is a latch input", v)
	}
	body, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("relayed body is not JSON after the rewrite: %v\n%s", err, body)
	}
	details, _ := m["error"].(map[string]any)["details"].(map[string]any)
	if details == nil {
		t.Fatalf("error.details vanished from the body: %s", body)
	}
	if _, there := details["error_code"]; there {
		t.Error("error_code survived; the body latches even without the claim header, so both must go together")
	}
	if details["disabled_reason"] != "out_of_credits" {
		t.Errorf("disabled_reason = %v, want the sibling field untouched", details["disabled_reason"])
	}
}

// A claim naming any window but the fable one is information, not a latch
// input — the strip keeps it.
func TestBenignRepresentativeClaimSurvivesStrip(t *testing.T) {
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","content":[]}`))
		},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","content":[]}`))
		},
	})
	hideOverage(rig.pool, true)

	resp := postMessages(t, rig.front.URL, testBody)
	defer resp.Body.Close()
	if v := resp.Header.Get("Anthropic-Ratelimit-Unified-Representative-Claim"); v != "five_hour" {
		t.Errorf("claim = %q, want the benign five_hour kept", v)
	}
}

// Passthrough traffic describes the client's OWN account — identity and
// non-quota paths must stay byte-faithful even with the flag on.
func TestPassthroughResponsesNeverStripped(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Overage-In-Use", "true")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(up.Close)

	p := pool.New([]*pool.Account{pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, up.URL)}, time.Now())
	hideOverage(p, true)
	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "fail"
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)

	resp, err := http.Get(front.URL + "/api/claude_code/settings") // isNonQuotaPath
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if v := resp.Header.Get("Anthropic-Ratelimit-Unified-Overage-In-Use"); v != "true" {
		t.Errorf("overage-in-use = %q on a passthrough path, want verbatim %q", v, "true")
	}
}

// The gate is provider-scoped: the markers are Anthropic vocabulary, and
// Anthropic semantics must never be applied to another provider's response.
func TestCreditSignalGateSkipsNonClaudeAccounts(t *testing.T) {
	p := pool.New(nil, time.Now())
	hideOverage(p, true)
	cfg := config.Defaults()
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}

	kimi := pool.NewAccount("k", pool.SourceYAML, "tok", "", 0, "")
	kimi.Type = "kimi-oauth"
	if h.hidingCreditSignals(kimi) {
		t.Error("a kimi account passed the gate; the strip is Claude-only")
	}
	claude := pool.NewAccount("c", pool.SourceYAML, "tok", "", 0, "")
	claude.Type = "claude-oauth"
	if !h.hidingCreditSignals(claude) {
		t.Error("a claude account failed the gate with the flag on")
	}
}

// A body the rewrite cannot confidently edit passes through untouched: a
// malformed upstream error must never be replaced by a proxy-made one.
func TestNeutralizeCreditsRequiredPassesMalformedBodyThrough(t *testing.T) {
	for name, body := range map[string]string{
		"not json":        "upstream fell over",
		"empty":           "",
		"no error object": `{"type":"overloaded"}`,
		"no error_code":   `{"error":{"details":{"disabled_reason":"x"}}}`,
		"other code":      `{"error":{"details":{"error_code":"something_else"}}}`,
	} {
		got := neutralizeCreditsRequired([]byte(body))
		if string(got) != body {
			t.Errorf("%s: body changed from %q to %q; anything but a confident edit must pass through", name, body, got)
		}
	}
	// Both observed nestings are handled.
	deep := `{"error":{"error":{"details":{"error_code":"credits_required"}}}}`
	var m map[string]any
	if err := json.Unmarshal(neutralizeCreditsRequired([]byte(deep)), &m); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(neutralizeCreditsRequired([]byte(deep))), "credits_required") {
		t.Error("the error.error.details nesting kept its error_code")
	}
}
