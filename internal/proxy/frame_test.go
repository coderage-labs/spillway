package proxy

// Issue #175: /api/frame is the artifact endpoint, and it was absent from
// identityTrees. Artifacts were therefore published on whichever pooled
// account selection happened to pick — 79 requests measured across 5
// accounts, none of them the account the user works from, so none of the
// artifacts were visible to the user who made them.
//
// Same shape as #166's table (identitypath_test.go): every observed form is
// identity-bound at the predicate AND end to end through the real handler,
// plus the boundary that must NOT match.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/coderage-labs/spillway/internal/reqlog"
)

// framePaths are the sub-path forms actually observed in the request log
// (issue #175), plus the bare collection. The bare form matters for the
// same reason it did in #166: the base is where the thing is created.
var framePaths = []string{
	"/api/frame",
	"/api/frame/",
	"/api/frame/deploy/direct",
	"/api/frame/read/58883633-52d9-4a53-a4b6-36e5deadbeef",
	"/api/frame/58883633-52d9-4a53-a4b6-36e5deadbeef",
}

// TestFramePathsAreIdentityBound is the predicate-level table.
func TestFramePathsAreIdentityBound(t *testing.T) {
	for _, path := range framePaths {
		t.Run(path, func(t *testing.T) {
			if !isIdentityPath(path) {
				t.Errorf("isIdentityPath(%q) = false, want true — an artifact belongs to the client's own account", path)
			}
		})
	}
}

// TestFramePathBoundary is the negative half: widening the rule to
// /api/frame must not swallow a longer unrelated path.
func TestFramePathBoundary(t *testing.T) {
	for _, path := range []string{
		"/api/frameworks",
		"/api/frameworks/react",
		"/api/framed",
		"/api/fram",
		"/api/frame_read",
		"/v1/messages",
	} {
		t.Run(path, func(t *testing.T) {
			if isIdentityPath(path) {
				t.Errorf("isIdentityPath(%q) = true, want false — /api/frame must not match beyond its own segment boundary", path)
			}
		})
	}
}

// TestFrameIsNotAlsoNonQuota records the #175 classification decision as a
// test rather than only as prose: /api/frame is identity-bound and is
// deliberately NOT in isNonQuotaPath. route() checks isIdentityPath first
// and both branches call the same passThrough, so a second entry could
// never fire; it would be dead code that reads as policy, and it would
// relabel the user's own artifact traffic "(non-quota)" in the request log.
// The behaviour non-quota status buys — never touching pool selection or
// the hold path — is proven directly by the end-to-end test below.
func TestFrameIsNotAlsoNonQuota(t *testing.T) {
	for _, path := range framePaths {
		if isNonQuotaPath(path) {
			t.Errorf("isNonQuotaPath(%q) = true — /api/frame is classified identity-bound, not non-quota", path)
		}
	}
}

// TestFramePathsReachUpstreamOnClientCredential is the end-to-end proof
// through the real handler. identityRig's only pooled account is already
// exhausted in fail-fast mode, so a pool-routed request can only 429 and
// never reach the upstream: getting there at all proves pool selection was
// bypassed, and the upstream reports which credential it arrived on.
func TestFramePathsReachUpstreamOnClientCredential(t *testing.T) {
	for _, path := range framePaths {
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
					t.Errorf("upstream Authorization = %q, want the client's own credential — the artifact would land on a pooled account", a)
				}
				if k := hdr.Get("X-Api-Key"); k != "client-api-key" {
					t.Errorf("upstream X-Api-Key = %q, want the client's own credential verbatim", k)
				}
			default:
				t.Fatal("the upstream was never reached")
			}

			e := waitForEntry(t, rl)
			if e.Account != "(passthrough)" {
				t.Errorf("request log account = %q, want %q — an artifact publish must not consume a pool account", e.Account, "(passthrough)")
			}
			if e.Event != reqlog.EventPassthrough {
				t.Errorf("request log event = %q, want %q", e.Event, reqlog.EventPassthrough)
			}
		})
	}
}

// TestFrameworksIsNotIdentityBoundEndToEnd is the negative end to end: the
// near-miss must not be swept in by the /api/frame entry.
//
// Since issue #176 inverted the routing default, "not identity-bound" no
// longer shows up as a 429 from an exhausted pool — everything spillway
// does not pool now passes through, so both halves of this near-miss reach
// the upstream and the question is which CLAIM spillway made about them.
// The request-log label is where that claim lives, so that is what is
// asserted: "(passthrough)" means "this is the client's own artifact" and
// would be the over-match; "(unpooled)" means "spillway has no opinion
// about this path", which is the correct answer for /api/frameworks.
func TestFrameworksIsNotIdentityBoundEndToEnd(t *testing.T) {
	front, _, rl := identityRig(t)

	req, err := http.NewRequest(http.MethodPost, front.URL+"/api/frameworks", strings.NewReader(testBody))
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

	e := waitForEntry(t, rl)
	if e.Account == "(passthrough)" {
		t.Error("/api/frameworks was classified identity-bound — the /api/frame entry over-matched")
	}
	if e.Account != "(unpooled)" {
		t.Errorf("request log account = %q, want %q", e.Account, "(unpooled)")
	}
}
