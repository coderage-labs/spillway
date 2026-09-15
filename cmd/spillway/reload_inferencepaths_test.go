package main

// Issue #176: proxy.inferencePaths must reach the running handler through
// the same live-apply path everything else uses.
//
// Making it live is most of what makes the inverted routing default safe to
// ship. The daemon warns the first time it sees an unrecognised path take a
// POST with a body; the user's answer is one config line, and if that line
// needed a restart it would drop every proxied CLI's connection to fix a
// problem the daemon has only just told them about.

import (
	"strings"
	"testing"
)

const basePathsYAML = `proxy:
  port: 7654
upstream: https://api.anthropic.com
accounts:
  - name: a
    type: claude-oauth
`

// TestInferencePathsApplyLive: an edit adding a pooled path reaches the
// handler on reload, without a restart.
func TestInferencePathsApplyLive(t *testing.T) {
	h := newHarness(t, basePathsYAML, acct("a"))
	var got []string
	h.applier.enableInferencePaths(func(p []string) { got = p })

	h.write(basePathsYAML + `  - name: b
    type: claude-oauth
`)
	// A no-op for the pooled set: the handler is still told, because the
	// apply is unconditional, but it is told the empty set.
	h.reload()
	if len(got) != 0 {
		t.Fatalf("pooled paths = %v, want none configured yet", got)
	}

	h.write(strings.Replace(basePathsYAML, "  port: 7654\n",
		"  port: 7654\n  inferencePaths:\n    - /v1/responses\n", 1))
	h.reload()
	if len(got) != 1 || got[0] != "/v1/responses" {
		t.Errorf("pooled paths = %v, want [/v1/responses] — the edit did not reach the handler", got)
	}
	if !strings.Contains(h.logs.String(), "/v1/responses") {
		t.Errorf("the reload said nothing about the change:\n%s", h.logs.String())
	}
}

// TestInferencePathsRemovalAppliesLive: taking the line out again must also
// reach the handler. A set that only ever grows would mean an endpoint
// wrongly added could never be taken back without a restart.
func TestInferencePathsRemovalAppliesLive(t *testing.T) {
	withPath := strings.Replace(basePathsYAML, "  port: 7654\n",
		"  port: 7654\n  inferencePaths:\n    - /v1/responses\n", 1)
	h := newHarness(t, withPath, acct("a"))
	var got = []string{"sentinel"}
	h.applier.enableInferencePaths(func(p []string) { got = p })

	h.write(basePathsYAML)
	h.reload()
	if len(got) != 0 {
		t.Errorf("pooled paths = %v, want none after the line was removed", got)
	}
	if !strings.Contains(h.logs.String(), "POST /v1/messages only") {
		t.Errorf("the reload said nothing about the removal:\n%s", h.logs.String())
	}
}

// TestInferencePathsWithoutAHandlerIsANoOp: a build with no proxy handler
// wired up (the pool-only test harnesses) must not panic on a config that
// names inference paths.
func TestInferencePathsWithoutAHandlerIsANoOp(t *testing.T) {
	h := newHarness(t, basePathsYAML, acct("a"))
	h.write(strings.Replace(basePathsYAML, "  port: 7654\n",
		"  port: 7654\n  inferencePaths:\n    - /v1/responses\n", 1))
	h.reload() // must not panic
}
