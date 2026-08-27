package proxy

// Issue #91: non-inference paths (telemetry, settings, policy limits) must
// never participate in pool selection or the hold path. Covers three
// things: the three CONFIRMED non-quota paths bypass the pool entirely
// (isNonQuotaPath, passthrough.go), an unclassified pooled path never holds
// on exhaustion regardless of classification (the isInferencePath gate in
// route(), proxy.go), and /v1/messages keeps holding exactly as before.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// TestNonQuotaPathsSkipHoldOnExhaustion is issue #91's reported bug,
// reproduced directly: the pool's only account is ALREADY exhausted (as it
// would be after a real /v1/messages quota-429, exactly like production —
// SelectExcept never even gets a chance to hit the upstream for these
// paths since they never reach pool selection at all) with a reset an hour
// out, exhaustedMode=hold and a generous holdMax. Each of the three
// confirmed non-quota paths must still return promptly rather than queuing
// behind that reset — the 51-request, 53-minute pile-up issue #91 reports.
func TestNonQuotaPathsSkipHoldOnExhaustion(t *testing.T) {
	for _, path := range []string{
		"/api/event_logging/v2/batch",
		"/api/claude_code/settings",
		"/api/claude_code/policy_limits",
	} {
		t.Run(path, func(t *testing.T) {
			// The bypass still reaches the upstream directly (it only skips
			// pool SELECTION and the hold path, not the network hop) — a
			// plain 200 here is what "returned promptly, unheld" looks like.
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"ok":true}`)
			}))
			t.Cleanup(up.Close)
			cfg := config.Defaults()
			cfg.Upstream = up.URL
			cfg.Pool.ExhaustedMode = "hold"
			cfg.Pool.HoldMax = "2h"
			acct := pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, "")
			p := pool.New([]*pool.Account{acct}, time.Now())
			p.MarkExhausted(acct, time.Now().Add(time.Hour))
			h, err := NewHandler(&cfg, testLogger(), p)
			if err != nil {
				t.Fatal(err)
			}
			front := httptest.NewServer(h)
			t.Cleanup(front.Close)

			client := &http.Client{Timeout: 2 * time.Second}
			start := time.Now()
			resp, err := client.Get(front.URL + path)
			if err != nil {
				t.Fatalf("request did not return promptly: %v", err)
			}
			defer resp.Body.Close()
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Errorf("%s took %v — held instead of bypassing the pool", path, elapsed)
			}
			// Bypassed straight to the upstream (which 200s by default via
			// httptest — anything other than the upstream's own answer
			// would mean this went through pool selection instead).
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200 from the bypassed upstream", resp.StatusCode)
			}
		})
	}
}

// TestNonQuotaPathNotAttributedToPooledAccount: a confirmed non-quota path
// must not consume selection or appear in the request log against a real
// pooled account (issue #91's second complaint, independent of holding).
func TestNonQuotaPathNotAttributedToPooledAccount(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()

	rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "fail"
	p := pool.New([]*pool.Account{pool.NewAccount("realacct", pool.SourceYAML, "tok", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	h.SetHooks(Hooks{Log: rl})
	front := httptest.NewServer(h)
	defer front.Close()

	resp, err := http.Get(front.URL + "/api/event_logging/v2/batch")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	e := waitForEntry(t, rl)
	if e.Account == "realacct" {
		t.Errorf("account = %q, want NOT attributed to the pooled account", e.Account)
	}
	if e.Event != reqlog.EventPassthrough {
		t.Errorf("event = %q, want %q", e.Event, reqlog.EventPassthrough)
	}
}

// TestNonQuotaPathForwardsClientCredentialUnrewritten: bypass means forward
// as-is with the client's own credential — no injection, no rewrite — the
// same invariant the existing identity paths hold to (§4).
func TestNonQuotaPathForwardsClientCredentialUnrewritten(t *testing.T) {
	got := make(chan http.Header, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	p := pool.New([]*pool.Account{pool.NewAccount("a", pool.SourceYAML, "injected-token", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/api/claude_code/policy_limits", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("X-Api-Key", "client-api-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	hdr := <-got
	if a := hdr.Get("Authorization"); a != "Bearer client-token" {
		t.Errorf("Authorization = %q, want the client's own credential verbatim", a)
	}
	if k := hdr.Get("X-Api-Key"); k != "client-api-key" {
		t.Errorf("X-Api-Key = %q, want the client's own credential verbatim", k)
	}
}

// TestUnclassifiedPathStaysPooled: a path NOT confirmed either way (e.g. an
// mcp-registry lookup) must still get a pooled account's credential when
// one is available — issue #91 explicitly asks to err toward leaving an
// unsure path in the pool rather than guessing it can be bypassed.
func TestUnclassifiedPathStaysPooled(t *testing.T) {
	got := make(chan http.Header, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	p := pool.New([]*pool.Account{pool.NewAccount("a", pool.SourceYAML, "pool-tok", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/mcp-registry/v0/servers", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	hdr := <-got
	if a := hdr.Get("Authorization"); a != "Bearer pool-tok" {
		t.Errorf("Authorization = %q, want pool injection (unclassified paths stay pooled)", a)
	}
}

// TestUnclassifiedPathFailsFastNeverHolds: the conservative fallback issue
// #91 asks for when classification is uncertain — an unclassified path
// still participates in pool selection (see above) but must never hold on
// exhaustion, only POST /v1/messages may.
func TestUnclassifiedPathFailsFastNeverHolds(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be hit — the only account is already exhausted")
	}))
	defer up.Close()

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "hold"
	cfg.Pool.HoldMax = "2h"
	acct := pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, "")
	p := pool.New([]*pool.Account{acct}, time.Now())
	p.MarkExhausted(acct, time.Now().Add(time.Hour))
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	start := time.Now()
	resp, err := client.Get(front.URL + "/mcp-registry/v0/servers")
	if err != nil {
		t.Fatalf("request did not return promptly: %v", err)
	}
	defer resp.Body.Close()
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("took %v — an unclassified path held instead of failing fast", elapsed)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
}

// TestInferencePathStillHoldsOnExhaustion: regression guard — the one
// confirmed inference path must still hold under exhaustedMode=hold exactly
// as before issue #91 (full behaviour already covered by hold_test.go; this
// is a direct, narrow check that the new isInferencePath gate is not
// accidentally false for it).
func TestInferencePathStillHoldsOnExhaustion(t *testing.T) {
	var calls atomic.Int32
	front, _ := holdRig(t, "hold", "30s", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			quota429RetryAfter(1)(w, r)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	})

	start := time.Now()
	resp, err := postMessages2(t, front.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after the hold clears", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("returned after %v — /v1/messages must still hold, not fail fast", elapsed)
	}
}

// postMessages2 avoids colliding with failover_test.go's postMessages,
// which calls t.Fatal on error rather than returning it.
func postMessages2(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	return http.Post(url+"/v1/messages", "application/json", strings.NewReader(testBody))
}
