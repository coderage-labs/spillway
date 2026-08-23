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

// recordedRequest is a Claude Code /v1/messages call, trimmed to the shape
// that matters. The system prompt and tool block are what Anthropic
// fingerprints on, so they are present in full rather than elided.
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
                      "required": ["command"]}}
  ],
  "metadata": {"user_id": "user_abc123_account__session_def456"},
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

// With no account uuid configured there is nothing to align, so even the one
// permitted body mutation must not fire.
func TestReplayLeavesBodyByteIdenticalWithoutUUID(t *testing.T) {
	c := replayThroughProxy(t, nil)
	if !bytes.Equal(normaliseJSON(t, []byte(recordedBody)), normaliseJSON(t, c.body)) {
		t.Errorf("body changed with no mutation configured:\n sent    %s\n arrived %s",
			recordedBody, c.body)
	}
}

// The account_uuid rewrite is permitted, and must touch ONLY that value.
func TestReplayRewritesOnlyTheAccountUUID(t *testing.T) {
	c := replayThroughProxy(t, func(a *pool.Account) {
		a.AccountUUID = "11111111-2222-3333-4444-555555555555"
	})
	var arrived map[string]any
	if err := json.Unmarshal(c.body, &arrived); err != nil {
		t.Fatal(err)
	}
	md, _ := arrived["metadata"].(map[string]any)
	uid, _ := md["user_id"].(string)
	if !strings.Contains(uid, "user_abc123") || !strings.Contains(uid, "session_def456") {
		t.Errorf("user_id lost its surrounding structure: %q", uid)
	}
	// Everything else identical.
	var sent map[string]any
	_ = json.Unmarshal([]byte(recordedBody), &sent)
	delete(sent, "metadata")
	delete(arrived, "metadata")
	if !reflect.DeepEqual(sent, arrived) {
		t.Error("the uuid rewrite disturbed a field other than metadata")
	}
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

// normaliseJSON re-marshals so key order and whitespace do not count as
// differences — only values do.
func normaliseJSON(t *testing.T, b []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
