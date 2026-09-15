package proxy

// Issue #166: the identity-path list matched "/v1/sessions/" by PREFIX, so
// the exact collection path /v1/sessions — how a session is created — fell
// through to pool routing and was created under whichever pooled account
// selection happened to pick, while every later /v1/sessions/<id> call on
// that same session went out on the client's own credential.
//
// These tests pin both halves of each identity tree (the collection and its
// items) and, just as importantly, the boundary: /v1/sessionsfoo must not
// match, and /v1/messages must never become identity-bound.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// identityCollections are the collection endpoints that issue #166 found
// falling through to the pool. Only /v1/sessions is confirmed live (8 pooled
// accounts served it); the other two have zero observed requests but are the
// same structural defect.
var identityCollections = []string{"/v1/code", "/v1/environments", "/v1/sessions", "/api/oauth/files"}

// TestIsIdentityPathMatchesCollectionAndItems is the predicate-level table:
// for every identity tree, BOTH the exact base and the trailing-slash form
// (and an item under it) are identity-bound.
func TestIsIdentityPathMatchesCollectionAndItems(t *testing.T) {
	for _, base := range identityCollections {
		for _, path := range []string{base, base + "/", base + "/abc123", base + "/abc123/archive"} {
			t.Run(path, func(t *testing.T) {
				if !isIdentityPath(path) {
					t.Errorf("isIdentityPath(%q) = false, want true — identity-bound", path)
				}
			})
		}
	}
}

// TestIsIdentityPathDoesNotOverMatch is the other half of issue #166: the
// fix widens the rule from "base/" to "base or base/", and must not widen it
// one character further. /v1/messages is called out in passthrough.go as the
// path that must NEVER be identity-bound — pool routing it is the product.
func TestIsIdentityPathDoesNotOverMatch(t *testing.T) {
	for _, path := range []string{
		"/v1/messages",
		"/v1/messages/count_tokens",
		"/v1/sessionsfoo",
		"/v1/sessions_archive",
		"/v1/environmentsx",
		"/v1/codex",
		"/v1/code_interpreter",
		"/api/oauth/filesystem",
		"/v1/complete",
		"/mcp-registry/v0/servers",
		"/",
		"",
	} {
		t.Run(path, func(t *testing.T) {
			if isIdentityPath(path) {
				t.Errorf("isIdentityPath(%q) = true, want false — must stay pool-routed", path)
			}
		})
	}
}

// TestUnderPathBoundary pins the helper directly, including the segment
// boundary that stops a base from swallowing a longer unrelated path.
func TestUnderPathBoundary(t *testing.T) {
	cases := []struct {
		path, base string
		want       bool
	}{
		{"/v1/sessions", "/v1/sessions", true},
		{"/v1/sessions/", "/v1/sessions", true},
		{"/v1/sessions/x", "/v1/sessions", true},
		{"/v1/sessions/x/archive", "/v1/sessions", true},
		{"/v1/sessionsfoo", "/v1/sessions", false},
		{"/v1/session", "/v1/sessions", false},
		{"/v1/messages", "/v1/sessions", false},
		{"", "/v1/sessions", false},
	}
	for _, c := range cases {
		if got := underPath(c.path, c.base); got != c.want {
			t.Errorf("underPath(%q, %q) = %v, want %v", c.path, c.base, got, c.want)
		}
	}
}

// identityRig builds a proxy whose ONLY pooled account is already exhausted
// in fail-fast mode. That makes "did this consume a pool account?" directly
// observable end to end: a pool-routed request can only 429 (it has nothing
// to select), so reaching the upstream at all proves the request bypassed
// pool selection — and the upstream reports which credential it arrived on.
func identityRig(t *testing.T) (front *httptest.Server, seen chan http.Header, rl *reqlog.Log) {
	t.Helper()
	seen = make(chan http.Header, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(up.Close)

	rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rl.Close() })

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "fail"
	acct := pool.NewAccount("pooled", pool.SourceYAML, "pool-token", "", 0, "")
	p := pool.New([]*pool.Account{acct}, time.Now())
	p.MarkExhausted(acct, time.Now().Add(time.Hour))
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	h.SetHooks(Hooks{Log: rl})
	front = httptest.NewServer(h)
	t.Cleanup(front.Close)
	return front, seen, rl
}

// TestIdentityCollectionReachesUpstreamOnClientCredential is the end-to-end
// proof, through the real proxy handler rather than the predicate: for every
// identity tree, BOTH the exact collection path and its trailing-slash form
// reach the upstream carrying the CLIENT's own credential, and neither
// consumes a pool account (the only account is exhausted — a pool-routed
// request would 429 and never reach the upstream at all).
func TestIdentityCollectionReachesUpstreamOnClientCredential(t *testing.T) {
	for _, base := range identityCollections {
		for _, path := range []string{base, base + "/"} {
			t.Run(path, func(t *testing.T) {
				front, seen, rl := identityRig(t)

				req, err := http.NewRequest(http.MethodPost, front.URL+path, strings.NewReader(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer client-token")
				req.Header.Set("X-Api-Key", "client-api-key")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200 — %q was pool-routed into the exhausted pool instead of passed through", resp.StatusCode, path)
				}
				select {
				case hdr := <-seen:
					if a := hdr.Get("Authorization"); a != "Bearer client-token" {
						t.Errorf("upstream Authorization = %q, want the client's own credential verbatim", a)
					}
					if k := hdr.Get("X-Api-Key"); k != "client-api-key" {
						t.Errorf("upstream X-Api-Key = %q, want the client's own credential verbatim", k)
					}
				default:
					t.Fatal("the upstream was never reached")
				}

				e := waitForEntry(t, rl)
				if e.Account != "(passthrough)" {
					t.Errorf("request log account = %q, want %q — %q must not consume a pool account", e.Account, "(passthrough)", path)
				}
				if e.Event != reqlog.EventPassthrough {
					t.Errorf("request log event = %q, want %q", e.Event, reqlog.EventPassthrough)
				}
			})
		}
	}
}

// TestNonIdentityPathsEndToEnd is the negative, also end to end: neither
// POST /v1/messages nor the near-miss /v1/sessionsfoo may be classified
// identity-bound.
//
// Issue #176 split what this test used to assert in one go. Before the
// inversion "not identity-bound" and "pool-routed" were the same thing, so
// a 429 from an exhausted pool proved both. They are now different
// questions with different answers for these two paths: /v1/messages is
// still pooled and still 429s, while /v1/sessionsfoo is not pooled and
// passes through under its own label. Both are checked, because the
// original point — the identity rule must not widen one character further
// (issue #166) — applies to both and is the reason /v1/sessionsfoo is here.
func TestNonIdentityPathsEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		path       string
		wantStatus int
		wantLabel  string
	}{
		// Inference: still pooled, so an exhausted pool still 429s it.
		{"/v1/messages", http.StatusTooManyRequests, "(none available)"},
		// A near-miss on /v1/sessions: not identity-bound, and since the
		// inversion not pooled either — passed through, labelled as the
		// path spillway has no opinion about.
		{"/v1/sessionsfoo", http.StatusOK, "(unpooled)"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			front, _, rl := identityRig(t)

			req, err := http.NewRequest(http.MethodPost, front.URL+tc.path, strings.NewReader(testBody))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer client-token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			e := waitForEntry(t, rl)
			if e.Account == "(passthrough)" {
				t.Errorf("%q was classified identity-bound — the identity rule over-matched", tc.path)
			}
			if e.Account != tc.wantLabel {
				t.Errorf("request log account = %q, want %q", e.Account, tc.wantLabel)
			}
		})
	}
}
