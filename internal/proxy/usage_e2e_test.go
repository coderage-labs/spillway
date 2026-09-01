package proxy

// End-to-end coverage for issue #110: usage counters observed off a real
// HTTP round trip through Handler, not just the sniffer in isolation. Two
// things these tests exist to catch that the unit tests above cannot:
// wiring (does route()/serve() actually reach reqlog with the right
// values), and behaviour under a real streaming HTTP response (does the
// client actually receive bytes as they arrive, not after the response
// finishes).

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// promptMarker stands in for real prompt/completion content — planted deep
// inside a content_block_delta, exactly where a real completion's text
// would be, so TestNoPromptContentReachesLog can prove it never reaches the
// database in any form, not just that the Entry struct happens to lack a
// field for it.
const promptMarker = "SECRET-PROMPT-MARKER-DO-NOT-PERSIST"

func sseUsageBodyWithMarker() string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1234,"cache_creation_input_tokens":4165,"cache_read_input_tokens":1816,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + promptMarker + `"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

// newUsageTestHandler wires a Handler to a fresh request log at a known
// path and a one-account pool pointed at upstreamURL, for tests that need
// to inspect both the client-visible response and what landed in reqlog.
func newUsageTestHandler(t *testing.T, upstreamURL string) (h *Handler, rl *reqlog.Log, dbPath string) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "r.db")
	rl, err := reqlog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rl.Close() })

	cfg := config.Defaults()
	cfg.Upstream = upstreamURL
	cfg.Pool.ExhaustedMode = "fail"
	p := pool.New([]*pool.Account{pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, "")}, time.Now())
	h, err = NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	h.SetHooks(Hooks{Log: rl})
	return h, rl, dbPath
}

// TestStreamedUsageRecordedByteIdenticalAndUndelayed is the central claim
// of issue #110: usage is observed off a streamed SSE response WITHOUT
// buffering it — the client's bytes must be identical to what the upstream
// sent, and must arrive as they're produced rather than only once the
// whole response is in.
func TestStreamedUsageRecordedByteIdenticalAndUndelayed(t *testing.T) {
	body := sseUsageBodyWithMarker()
	half := len(body) / 2
	const pause = 200 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("test upstream ResponseWriter does not support Flusher")
			return
		}
		io.WriteString(w, body[:half])
		fl.Flush()
		time.Sleep(pause)
		io.WriteString(w, body[half:])
		fl.Flush()
	}))
	defer upstream.Close()

	h, rl, _ := newUsageTestHandler(t, upstream.URL)
	front := httptest.NewServer(h)
	defer front.Close()

	start := time.Now()
	resp := postMessages(t, front.URL, `{"model":"claude","max_tokens":1}`)
	defer resp.Body.Close()

	r := bufio.NewReader(resp.Body)
	firstChunk := make([]byte, half)
	if _, err := io.ReadFull(r, firstChunk); err != nil {
		t.Fatalf("reading first chunk: %v", err)
	}
	firstArrived := time.Since(start)
	if firstArrived >= pause {
		t.Errorf("first chunk arrived after %v (upstream's pause is %v) — "+
			"looks buffered until the full response, not streamed through", firstArrived, pause)
	}

	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading remainder: %v", err)
	}
	got := string(firstChunk) + string(rest)
	if got != body {
		t.Errorf("client body differs from upstream body:\n got=%q\nwant=%q", got, body)
	}

	e := waitForEntry(t, rl)
	want := reqlog.Entry{InputTokens: 1234, OutputTokens: 42, CacheCreationInputTokens: 4165, CacheReadInputTokens: 1816}
	if e.InputTokens != want.InputTokens || e.OutputTokens != want.OutputTokens ||
		e.CacheCreationInputTokens != want.CacheCreationInputTokens || e.CacheReadInputTokens != want.CacheReadInputTokens {
		t.Errorf("usage counters = %+v, want %+v", e, want)
	}
}

// TestPlainJSONUsageRecorded covers the non-SSE shape: usage sits directly
// in a single JSON body, no streaming involved.
func TestPlainJSONUsageRecorded(t *testing.T) {
	respBody := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],` +
		`"usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":30,"cache_read_input_tokens":40}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, respBody)
	}))
	defer upstream.Close()

	h, rl, _ := newUsageTestHandler(t, upstream.URL)
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, `{"model":"claude","max_tokens":1}`)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != respBody {
		t.Errorf("client body differs from upstream body:\n got=%q\nwant=%q", got, respBody)
	}

	e := waitForEntry(t, rl)
	if e.InputTokens != 10 || e.OutputTokens != 20 || e.CacheCreationInputTokens != 30 || e.CacheReadInputTokens != 40 {
		t.Errorf("usage counters = %+v, want input=10 output=20 create=30 read=40", e)
	}
}

// TestTruncatedStreamRecordsZeroAndDoesNotFailRequest: the upstream
// connection dies mid-object, well before message_start's usage block ever
// completes. The client must still get whatever bytes were sent (no panic,
// no proxy-side error response injected after headers are already out —
// §6.1, no failover past the first byte) and the log must show zeros, not
// a crash and not a stale/garbage value.
func TestTruncatedStreamRecordsZeroAndDoesNotFailRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"usage":{"input_tok`)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test upstream ResponseWriter does not support Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		conn.Close() // abrupt: no more bytes, no clean SSE termination
	}))
	defer upstream.Close()

	h, rl, _ := newUsageTestHandler(t, upstream.URL)
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, `{"model":"claude","max_tokens":1}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (headers were sent before the cut)", resp.StatusCode)
	}
	// The body read is expected to end in an error (the connection was cut
	// mid-stream) — that is the client's own problem to handle, same as any
	// other broken connection. The request-log side is what this test
	// actually checks.
	_, _ = io.ReadAll(resp.Body)

	e := waitForEntry(t, rl)
	if e.InputTokens != 0 || e.OutputTokens != 0 || e.CacheCreationInputTokens != 0 || e.CacheReadInputTokens != 0 {
		t.Errorf("usage counters = %+v, want all zero for a stream truncated before any usable event", e)
	}
}

// TestNoPromptContentReachesLog: a completion's actual text — planted here
// as promptMarker, standing in for anything a real conversation would
// contain — must never reach the database in any form. Checked two ways:
// against every string field reqlog.Entry exposes, AND against the raw
// database file's bytes, so a future column added to the schema without a
// matching Entry field would still be caught.
func TestNoPromptContentReachesLog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sseUsageBodyWithMarker())
	}))
	defer upstream.Close()

	h, rl, dbPath := newUsageTestHandler(t, upstream.URL)
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, `{"model":"claude","max_tokens":1}`)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: the marker really did flow to the client (proves the test
	// fixture is meaningful, not that nothing was ever sent).
	if !bytes.Contains(body, []byte(promptMarker)) {
		t.Fatal("test fixture broken: promptMarker never reached the client")
	}

	e := waitForEntry(t, rl)
	for name, v := range map[string]string{
		"Account": e.Account, "Path": e.Path, "Event": e.Event,
		"ModelAsked": e.ModelAsked, "ModelServed": e.ModelServed,
		"UserAgent": e.UserAgent, "SessionHash": e.SessionHash,
	} {
		if strings.Contains(v, promptMarker) {
			t.Errorf("Entry.%s contains prompt content: %q", name, v)
		}
	}

	// Belt and braces: the marker must not appear anywhere in the database
	// file at all, not only in the fields Entry happens to expose.
	if err := rl.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(promptMarker)) {
		t.Error("prompt content found in the raw request-log database file")
	}
}
