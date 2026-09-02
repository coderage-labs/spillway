package proxy

// The replay suite (design doc §6.8a): a recorded Claude Code request is
// pushed through the proxy against a mock upstream, and what arrives is
// compared byte-for-byte against what was sent — except the four mutations
// §4 permits.
//
// This is the test that guards the invariant the whole project rests on. If
// spillway alters the request shape, Anthropic stops treating the traffic as
// Claude-Code-on-subscription and bills it as metered API instead. Unit tests
// on individual mutations cannot catch an accidental addition; only comparing
// whole requests can.
//
// The fingerprint was widened from three mutations to four for issue #29:
// spillway now also rewrites a "model" field nested directly inside a
// tools[] element (an advisor model), because leaving it untouched let a
// Claude model id reach a non-Claude provider verbatim on cross-provider
// rotation — exactly what §6.12's hard error exists to prevent, just via a
// second, previously-unguarded field. TestReplayPreservesRequestFingerprint
// below still runs with no modelMap configured (a same-provider account), so
// its "tools" field must stay wholly unchanged; the case where a modelMap IS
// configured and DOES rewrite a nested model is covered separately by
// TestNestedToolModelMapThroughProxy in nested_model_test.go, which is the
// fingerprint test for mutation #4 specifically.
//
// This suite guards the REQUEST side only, and that has not widened: the
// opt-in response mutation (hideOverageFromClient, issue #103) touches
// nothing spillway sends upstream, so nothing here changes for it. Its own
// guards — including that responses stay byte-faithful while the flag is
// off — live in credit_signals_test.go.
//
// "Byte-for-byte" is meant literally, and did not used to be (issue #128).
// The comparison here was JSON-NORMALISED on both sides, which quietly
// forgave every difference normalisation erases: whitespace, key order,
// number formatting (1 vs 1.0), escape-sequence choice. A byte deliberately
// appended to the outgoing body during #127 sailed straight through it.
//
// That gap was not cosmetic. Anthropic's prompt cache keys on a byte-exact
// prefix, so a whitespace-only change is semantically nothing and
// cache-wise everything: it invalidates the cached prefix and re-bills it
// at cache-create prices. The exact class of change the old guard could not
// see is the class that silently costs the user quota (issue #111).
//
// So the guarantee now rests on bytes.Equal — assertBodyFaithful below —
// against the recorded body with the permitted mutations applied as literal
// substitutions. The normalised comparison survives only to label a
// failure ("JSON-EQUAL but not byte-equal"), never to decide one.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
)

// The values the permitted mutations are allowed to touch, named so the
// tests can express "these bytes may change, nothing else may".
const (
	recordedAccountUUID = "00000000-1111-2222-3333-444444444444"
	recordedTopModel    = "claude-sonnet-4-6" // mutation #3 target
	recordedToolModel   = "claude-fable-5"    // mutation #4 target (issue #29)
)

// recordedRequest is a Claude Code /v1/messages call, trimmed to the shape
// that matters. The system prompt and tool block are what Anthropic
// fingerprints on, so they are present in full rather than elided.
//
// The fixture deliberately carries a target for EVERY permitted body
// mutation, so a byte comparison can pin the rest of the body against each
// one in turn: metadata.user_id is the stringified-JSON blob Claude Code
// really sends (mutation #2 needs an embedded account_uuid to find — before
// #128 this fixture had none, so the uuid test rewrote nothing and proved
// nothing), and tools[] carries an advisor element with its own "model"
// (mutation #4).
//
// The indentation, the two-space gutter and the line breaks inside "system"
// are load-bearing: they are what a whitespace-blind comparison cannot see,
// and what the prompt cache's byte-exact prefix match very much can.
const recordedBody = `{
  "model": "claude-sonnet-4-6",
  "max_tokens": 8192,
  "system": [
    {"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude.",
     "cache_control": {"type": "ephemeral"}}
  ],
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "list the files here"}]}
  ],
  "tools": [
    {"name": "Bash", "description": "Run a shell command",
     "input_schema": {"type": "object", "properties": {"command": {"type": "string"}},
                      "required": ["command"]}},
    {"type": "advisor_fable", "model": "claude-fable-5"}
  ],
  "metadata": {"user_id": "{\"user_id\":\"user_abc123\",\"account_uuid\":\"00000000-1111-2222-3333-444444444444\",\"session_id\":\"session_def456\"}"},
  "temperature": 1,
  "stream": true
}`

var recordedHeaders = map[string]string{
	"Content-Type":      "application/json",
	"anthropic-version": "2023-06-01",
	"anthropic-beta":    "oauth-2025-04-20,fine-grained-tool-streaming-2025-05-14",
	"user-agent":        "claude-cli/2.1.0 (external, cli)",
	"x-app":             "cli",
	"x-stainless-lang":  "js",
	"accept":            "application/json",
}

// captured is what the mock upstream saw.
type captured struct {
	header http.Header
	body   []byte
}

func replayThroughProxy(t *testing.T, acct func(*pool.Account)) captured {
	t.Helper()
	got := make(chan captured, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- captured{header: r.Header.Clone(), body: b}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","content":[]}`)
	}))
	defer up.Close()

	a := pool.NewAccount("acct", pool.SourceYAML, "injected-token", "", 0, up.URL)
	a.Type = "claude-oauth"
	if acct != nil {
		acct(a)
	}
	p := pool.New([]*pool.Account{a}, time.Now())
	cfg := config.Defaults()
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages",
		strings.NewReader(recordedBody))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range recordedHeaders {
		req.Header.Set(k, v)
	}
	// The client's own credential, which must be replaced rather than
	// forwarded alongside the injected one.
	req.Header.Set("Authorization", "Bearer client-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case c := <-got:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the request")
		return captured{}
	}
}

// The fingerprint must survive intact: same headers, same body, except the
// mutations §4 allows.
func TestReplayPreservesRequestFingerprint(t *testing.T) {
	c := replayThroughProxy(t, nil)

	// Headers: everything the client sent must arrive unchanged, except
	// Authorization (replaced) and hop-by-hop.
	for k, want := range recordedHeaders {
		if got := c.header.Get(k); got != want {
			t.Errorf("header %q = %q, want %q — the fingerprint must arrive intact", k, got, want)
		}
	}
	if got := c.header.Get("Authorization"); got != "Bearer injected-token" {
		t.Errorf("Authorization = %q, want the pool's token", got)
	}
	if c.header.Get("X-Api-Key") != "" {
		t.Error("x-api-key must be stripped, not forwarded alongside the injected token")
	}
	// No proxy fingerprint of our own. X-Forwarded-For was added once and
	// removed precisely because it announces the proxy upstream.
	for _, banned := range []string{"X-Forwarded-For", "X-Forwarded-Host", "Via", "Forwarded"} {
		if v := c.header.Get(banned); v != "" {
			t.Errorf("header %q was added by the proxy (%q) — it announces us upstream", banned, v)
		}
	}

	// Body: identical except metadata.user_id, which §4 permits rewriting.
	var sent, arrived map[string]any
	if err := json.Unmarshal([]byte(recordedBody), &sent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(c.body, &arrived); err != nil {
		t.Fatalf("body is no longer valid JSON: %v", err)
	}
	if !reflect.DeepEqual(sortedKeys(sent), sortedKeys(arrived)) {
		t.Errorf("top-level fields changed:\n sent    %v\n arrived %v",
			sortedKeys(sent), sortedKeys(arrived))
	}
	for _, k := range []string{"model", "max_tokens", "system", "messages", "tools", "temperature", "stream"} {
		if !reflect.DeepEqual(sent[k], arrived[k]) {
			t.Errorf("field %q was altered:\n sent    %#v\n arrived %#v", k, sent[k], arrived[k])
		}
	}
}

// With no mutation configured the body must arrive BYTE-identical — not
// merely JSON-equivalent. This is the guarantee the project rests on, so it
// is asserted on raw bytes: see assertBodyFaithful for why a normalised
// comparison is not good enough (issue #128).
func TestReplayLeavesBodyByteIdenticalWithoutUUID(t *testing.T) {
	c := replayThroughProxy(t, nil)
	assertBodyFaithful(t, c.body, recordedBody)
}

// The account_uuid rewrite is permitted, and must touch ONLY those 36 bytes.
// Everything either side of them, whitespace included, must be identical.
func TestReplayRewritesOnlyTheAccountUUID(t *testing.T) {
	const newUUID = "11111111-2222-3333-4444-555555555555"
	c := replayThroughProxy(t, func(a *pool.Account) {
		a.AccountUUID = newUUID
	})
	assertBodyFaithful(t, c.body, recordedBody,
		permittedEdit{"#2 account_uuid", recordedAccountUUID, newUUID})

	// Diagnostic: say plainly what went wrong if the patcher landed the
	// uuid somewhere other than where it belongs, rather than only
	// reporting a byte offset.
	var arrived map[string]any
	if err := json.Unmarshal(c.body, &arrived); err != nil {
		t.Fatalf("body is no longer valid JSON: %v", err)
	}
	md, _ := arrived["metadata"].(map[string]any)
	uid, _ := md["user_id"].(string)
	if !strings.Contains(uid, newUUID) {
		t.Errorf("account_uuid was not rewritten at all: %q", uid)
	}
	if !strings.Contains(uid, "user_abc123") || !strings.Contains(uid, "session_def456") {
		t.Errorf("user_id lost its surrounding structure: %q", uid)
	}
}

// The modelMap rewrites (mutations #3 and #4) are permitted, and must touch
// ONLY the two model values. This is the cross-provider path — the one that
// rewrites the most bytes, and so the one with the most room to disturb
// something it should not.
func TestReplayRewritesOnlyTheModelValues(t *testing.T) {
	const (
		newTopModel  = "k3[1m]"
		newToolModel = "kimi-for-coding"
	)
	c := replayThroughProxy(t, func(a *pool.Account) {
		a.Type = "kimi-oauth"
		a.ModelMap = map[string]string{
			recordedTopModel:  newTopModel,
			recordedToolModel: newToolModel,
		}
	})
	assertBodyFaithful(t, c.body, recordedBody,
		permittedEdit{"#3 top-level model", `"model": "` + recordedTopModel + `"`, `"model": "` + newTopModel + `"`},
		permittedEdit{"#4 tools[].model", `"model": "` + recordedToolModel + `"`, `"model": "` + newToolModel + `"`})
}

// Identity-bound paths carry the CLIENT's credential, never the pool's —
// this is what keeps Remote Control working through the proxy.
func TestReplayIdentityPathKeepsClientCredential(t *testing.T) {
	got := make(chan string, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	a := pool.NewAccount("acct", pool.SourceYAML, "injected-token", "", 0, up.URL)
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, time.Now())
	cfg := config.Defaults()
	cfg.Upstream = up.URL
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/v1/code/whatever", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case auth := <-got:
		if auth != "Bearer client-token" {
			t.Errorf("identity path got %q, want the client's own credential", auth)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the request")
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// permittedEdit is one design-doc §4 mutation, written as the literal
// substitution it is allowed to make in the recorded body: `what` names the
// mutation for the failure message, `from` the exact bytes it may replace,
// `to` what it may replace them with. Anything spillway changes that no
// edit accounts for is a contract violation.
type permittedEdit struct {
	what string
	from string
	to   string
}

// wantBody applies the permitted edits to the recorded body and returns the
// exact bytes the upstream must receive.
//
// It is deliberately built with strings.Replace rather than by calling
// spillway's own patchers: an independent construction means a bug in a
// patcher shows up as a byte difference instead of cancelling itself out on
// both sides of the comparison. Each `from` must occur exactly once, or the
// expectation itself would be ambiguous and the test is wrong, not the code.
func wantBody(t *testing.T, sent string, edits ...permittedEdit) []byte {
	t.Helper()
	out := sent
	for _, e := range edits {
		if n := strings.Count(out, e.from); n != 1 {
			t.Fatalf("permitted edit %s: %q occurs %d times in the fixture, want exactly 1 — the expectation is ambiguous",
				e.what, e.from, n)
		}
		out = strings.Replace(out, e.from, e.to, 1)
	}
	return []byte(out)
}

// assertBodyFaithful is THE guard: what arrived upstream must equal the
// recorded body with the permitted edits applied, byte for byte.
//
// It compares raw bytes, not re-marshalled JSON, because the thing being
// protected is a byte-exact prefix. Anthropic's prompt cache keys on the
// literal bytes, so a change JSON comparison calls identical — a moved
// space, a reordered key, 1 written as 1.0, a character written as its \u
// escape — invalidates the cached prefix and re-bills the whole thing at
// cache-create prices. That class of change is exactly the class a
// normalised comparison cannot see, which is why the old version of this
// guard stayed green with a byte appended to the body (issue #128). The
// normalised comparison survives below, but only to label the failure,
// never to decide it.
func assertBodyFaithful(t *testing.T, arrived []byte, sent string, edits ...permittedEdit) {
	t.Helper()
	want := wantBody(t, sent, edits...)
	if bytes.Equal(want, arrived) {
		return
	}
	kind := "a value differs"
	if jsonEqual(want, arrived) {
		kind = "the bodies are JSON-EQUAL but not byte-equal — invisible to a normalised comparison, " +
			"and a full cache miss on a prefix the user has already paid to write"
	}
	i := firstDiff(want, arrived)
	t.Errorf("forwarded body is not byte-faithful (%s)\n"+
		" first difference at byte %d\n"+
		" want (%d bytes) ...%s...\n"+
		" got  (%d bytes) ...%s...\n"+
		" permitted mutations here: %s",
		kind, i, len(want), window(want, i), len(arrived), window(arrived, i), editNames(edits))
}

// jsonEqual reports whether two bodies mean the same thing once key order
// and whitespace are normalised away. Diagnostic only — it is what the old
// guard rested on, and it is not load-bearing any more.
func jsonEqual(a, b []byte) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// firstDiff returns the index of the first differing byte, or the length of
// the shorter body when one is a prefix of the other.
func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// window renders the bytes around i, quoted so whitespace and escapes are
// visible — the whole point being differences the eye slides over.
func window(b []byte, i int) string {
	const pad = 40
	lo, hi := i-pad, i+pad
	if lo < 0 {
		lo = 0
	}
	if hi > len(b) {
		hi = len(b)
	}
	return fmt.Sprintf("%q", b[lo:hi])
}

func editNames(edits []permittedEdit) string {
	if len(edits) == 0 {
		return "none — the body must arrive exactly as sent"
	}
	names := make([]string, 0, len(edits))
	for _, e := range edits {
		names = append(names, e.what)
	}
	return strings.Join(names, ", ")
}
