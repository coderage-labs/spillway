package proxy

// End-to-end coverage for issue #111 phase 1: the fingerprints have to
// survive a real HTTP round trip through Handler into a real on-disk
// request log, and the request that reaches the upstream has to be
// byte-identical to the one the client sent.
//
// #110 shipped a measurement that recorded zeros in production because the
// decisive test — does the value actually reach the database — was skipped.
// These are that test.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/reqlog"
)

// waitForEntries is waitForEntry for a known number of rows: the client's
// Do returns once the response headers arrive, so the log write for a
// request is still in progress when the test resumes.
func waitForEntries(t *testing.T, rl *reqlog.Log, n int) []reqlog.Entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := rl.Recent(100)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) >= n {
			return entries
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d requests reached the request log", len(entries), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// e2eBody is a /v1/messages body carrying a session id (so the rows group
// into one session) and the named tools, in the given order.
func e2eBody(sessionID string, toolNames []string, systemText string) string {
	tools := make([]string, 0, len(toolNames))
	for _, n := range toolNames {
		tools = append(tools, fpTool(n, "Run "+n+"."))
	}
	return fmt.Sprintf(`{"model":"claude-sonnet-4-6","max_tokens":8192,
"system":[{"type":"text","text":%q}],
"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],
"tools":[%s],
"metadata":{"user_id":"{\"session_id\":\"%s\"}"},"stream":true}`,
		systemText, strings.Join(tools, ","), sessionID)
}

// TestPrefixFingerprintsReachTheDatabase is the decisive one: a real
// request, a real store, and the row read back through reqlog rather than
// asserted on an in-memory struct.
func TestPrefixFingerprintsReachTheDatabase(t *testing.T) {
	var got [][]byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseUsageBodyWithMarker())
	}))
	defer up.Close()

	h, rl, _ := newUsageTestHandler(t, up.URL)
	front := httptest.NewServer(h)
	defer front.Close()

	sent := e2eBody("sess-A", []string{"Bash", "Read", "Edit"}, "you are claude code")
	resp := postMessagesRaw(t, front.URL, sent)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// §4 still permits exactly four mutations and this change adds none:
	// the body that reached the upstream must be the one the client sent.
	if len(got) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(got))
	}
	if !bytes.Equal(got[0], []byte(sent)) {
		t.Errorf("the request was mutated on its way upstream — measurement must not touch the request\n sent    %s\n arrived %s", sent, got[0])
	}

	e := waitForEntry(t, rl)
	if e.ToolCount != 3 {
		t.Errorf("ToolCount = %d, want 3 — the fingerprint never reached the database", e.ToolCount)
	}
	for name, h := range map[string]string{
		"ToolsOrderHash":    e.ToolsOrderHash,
		"ToolsSortedHash":   e.ToolsSortedHash,
		"ToolsRawHash":      e.ToolsRawHash,
		"SystemHash":        e.SystemHash,
		"FirstMsgShapeHash": e.FirstMsgShapeHash,
	} {
		if !fpHexOnly.MatchString(h) {
			t.Errorf("%s = %q, want a recorded hash", name, h)
		}
	}
	if e.FirstMsgBlocks != 1 {
		t.Errorf("FirstMsgBlocks = %d, want 1", e.FirstMsgBlocks)
	}
	if e.PrefixBytes <= 0 {
		t.Errorf("PrefixBytes = %d, want the measured prefix size", e.PrefixBytes)
	}
	// The row must carry both the account and the session, or the report
	// cannot separate rotation cost from in-session churn.
	if e.Account == "" || e.SessionHash == "" {
		t.Errorf("account=%q sessionHash=%q — both are needed to attribute a change", e.Account, e.SessionHash)
	}
	// And no content, from a body that contained plenty.
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"you are claude code", "hello", "Bash", "Run Read."} {
		if bytes.Contains(raw, []byte(needle)) {
			t.Errorf("logged entry %s contains %q from the request body", raw, needle)
		}
	}
}

// Issue #126: Anthropic has started returning Content-Encoding: br, which
// the usage sniffer does not decode, so those rows record zero usage. The
// fingerprints come from the REQUEST and must be recorded anyway —
// otherwise every Brotli response is invisible to the phase-2 analysis, and
// invisible selectively, which biases the result rather than shrinking it.
func TestPrefixFingerprintsRecordedWhenUsageCannotBeDecoded(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Not actually Brotli-compressed; the sniffer refuses the encoding
		// before it ever looks at the bytes, which is the behaviour under
		// test. Compressing it for real would test compress/brotli, which
		// spillway does not have and #126 will decide about separately.
		w.Header().Set("Content-Encoding", "br")
		_, _ = io.WriteString(w, sseUsageBodyWithMarker())
	}))
	defer up.Close()

	h, rl, _ := newUsageTestHandler(t, up.URL)
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessagesRaw(t, front.URL, e2eBody("sess-br", []string{"Bash", "Read"}, "sys"))
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	e := waitForEntry(t, rl)
	// The premise: usage really is missing on this row.
	if e.InputTokens != 0 || e.CacheCreationInputTokens != 0 {
		t.Fatalf("this test assumes an undecodable response records no usage, got %+v — the premise has changed", e)
	}
	// The claim: the fingerprints are there regardless.
	if e.ToolCount != 2 || e.ToolsOrderHash == "" || e.SystemHash == "" || e.FirstMsgShapeHash == "" {
		t.Errorf("fingerprints missing on a row whose response could not be decoded: %+v — the measurement would be blind to every Brotli response", e)
	}
}

// The full loop: two requests in one session with the same tools in a
// different order must be reported as ordering jitter, through the real
// proxy and the real store. This is the number phase 2 would be built on,
// so it is measured end to end rather than inferred from the unit tests.
func TestOrderingJitterIsVisibleEndToEnd(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseUsageBodyWithMarker())
	}))
	defer up.Close()

	h, rl, _ := newUsageTestHandler(t, up.URL)
	front := httptest.NewServer(h)
	defer front.Close()

	for _, tools := range [][]string{
		{"Bash", "Read", "Edit"},
		{"Read", "Edit", "Bash"}, // same set, shuffled
	} {
		resp := postMessagesRaw(t, front.URL, e2eBody("sess-jitter", tools, "you are claude code"))
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		// Distinct timestamps, so the pair orders deterministically.
		time.Sleep(2 * time.Millisecond)
	}

	waitForEntries(t, rl, 2)
	report, err := rl.PrefixDrift()
	if err != nil {
		t.Fatalf("PrefixDrift: %v", err)
	}
	var jitter, set, stable reqlog.PrefixChange
	for _, c := range report {
		if c.AccountChanged {
			continue
		}
		switch c.Kind {
		case reqlog.KindToolsOrderOnly:
			jitter = c
		case reqlog.KindToolsSet:
			set = c
		case reqlog.KindStable:
			stable = c
		}
	}
	if jitter.Pairs != 1 {
		t.Errorf("tools-order-only pairs = %d, want 1 — a reordered tool set was not recognised as ordering jitter end to end", jitter.Pairs)
	}
	if jitter.CacheCreationInputTokens != 4165 {
		t.Errorf("cache-creation attributed to ordering jitter = %d, want the 4165 the response reported",
			jitter.CacheCreationInputTokens)
	}
	if set.Pairs != 0 {
		t.Errorf("tools-set pairs = %d, want 0 — the set did not change", set.Pairs)
	}
	if stable.Pairs != 0 {
		t.Errorf("none pairs = %d, want 0 — something did change", stable.Pairs)
	}
}
