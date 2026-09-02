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
	"context"
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

// rawClient returns an http.Client that never negotiates or transparently
// undoes compression on its own — see setExplicitAcceptEncoding for why
// that alone isn't enough and an explicit Accept-Encoding is also needed.
// Plain net/http.Client (and http.Post, used by postMessages elsewhere in
// this package) does the opposite by default: when a request sets no
// Accept-Encoding header, Transport adds "gzip" itself and silently
// decompresses a gzip response before handing back resp.Body.
func rawClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableCompression: true}}
}

// setExplicitAcceptEncoding is the actual fix for these tests, and it has
// to happen on the request TO SPILLWAY, not just the one reading spillway's
// response. Go's Transport only auto-adds Accept-Encoding and transparently
// decompresses when a request carries NO Accept-Encoding header at all —
// and spillway's own outbound request to upstream is r.Header.Clone() of
// the inbound one (buildRequest), so an inbound request with no explicit
// header leaves the OUTBOUND leg looking equally bare, and spillway's own
// h.Transport does exactly the same automatic thing reaching the upstream
// test server. Caught live: a first version of these tests only set
// DisableCompression on the test's own client and still deadlocked,
// because spillway's outbound leg — a completely different Transport —
// was doing its own unrequested gzip negotiation and transparently
// unwrapping a stream that, in the cancellation test, never finishes.
// Anthropic's real traffic never hits this (issue #121's own measurement
// shows Content-Encoding: gzip surviving all the way to the client, and
// #126's shows Content-Encoding: br doing the same), which only makes
// sense if the real Claude Code CLI sends its own explicit
// Accept-Encoding — this header is standing in for that, and lists both
// codecs because that is what produces a br response upstream: spillway
// forwards the client's Accept-Encoding verbatim (buildRequest clones the
// inbound headers; Accept-Encoding is neither hop-by-hop nor a permitted
// mutation), so the response encoding is always the client's own choice,
// never spillway's.
func setExplicitAcceptEncoding(r *http.Request) { r.Header.Set("Accept-Encoding", "gzip, br") }

// postMessagesRaw is postMessages (failover_test.go), but via rawClient and
// with an explicit Accept-Encoding — see rawClient and
// setExplicitAcceptEncoding for why both matter whenever Content-Encoding:
// gzip is in play, on either leg of the proxy.
func postMessagesRaw(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	setExplicitAcceptEncoding(req)
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
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
// of issue #110 (and, for the gzip case, issue #121): usage is observed off
// a streamed SSE response WITHOUT buffering it — the client's bytes must be
// identical to what the upstream sent, and must arrive as they're produced
// rather than only once the whole response is in.
//
// Table-driven over Content-Encoding rather than a parallel test file
// (issue #121 asked for this test to be extended, not duplicated): "plain"
// is #110's original claim, unencoded; "gzip" and "br" are the same claim
// against the two encodings actually observed live from Anthropic (#121 and
// #126 respectively), where the wire bytes the client receives are the
// COMPRESSED ones and must stay that way even though usage is now read
// from their decompressed content. The comparison below is a raw
// bytes.Equal against the upstream's wire bytes, deliberately not a
// decode-then-compare.
func TestStreamedUsageRecordedByteIdenticalAndUndelayed(t *testing.T) {
	plain := sseUsageBodyWithMarker()

	cases := []struct {
		name string
		enc  string
		wire []byte // what actually goes out on the wire (and back to the client)
	}{
		{name: "plain", enc: "", wire: []byte(plain)},
		{name: "gzip", enc: "gzip", wire: gzipBytes(t, plain)},
		{name: "br", enc: "br", wire: brotliBytes(t, plain)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire := tc.wire
			half := len(wire) / 2
			const pause = 200 * time.Millisecond

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.enc != "" {
					w.Header().Set("Content-Encoding", tc.enc)
				}
				w.WriteHeader(http.StatusOK)
				fl, ok := w.(http.Flusher)
				if !ok {
					t.Error("test upstream ResponseWriter does not support Flusher")
					return
				}
				w.Write(wire[:half])
				fl.Flush()
				time.Sleep(pause)
				w.Write(wire[half:])
				fl.Flush()
			}))
			defer upstream.Close()

			h, rl, _ := newUsageTestHandler(t, upstream.URL)
			front := httptest.NewServer(h)
			defer front.Close()

			start := time.Now()
			resp := postMessagesRaw(t, front.URL, `{"model":"claude","max_tokens":1}`)
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
			got := append(firstChunk, rest...)
			if !bytes.Equal(got, wire) {
				t.Errorf("client bytes differ from upstream's wire bytes:\n got=%q\nwant=%q", got, wire)
			}

			e := waitForEntry(t, rl)
			want := reqlog.Entry{InputTokens: 1234, OutputTokens: 42, CacheCreationInputTokens: 4165, CacheReadInputTokens: 1816}
			if e.InputTokens != want.InputTokens || e.OutputTokens != want.OutputTokens ||
				e.CacheCreationInputTokens != want.CacheCreationInputTokens || e.CacheReadInputTokens != want.CacheReadInputTokens {
				t.Errorf("usage counters = %+v, want %+v", e, want)
			}
		})
	}
}

// TestAcceptEncodingForwardedVerbatim pins the mechanism behind issue #126
// and forecloses the wrong fix for it.
//
// Brotli arrives because the CLIENT asked for it: spillway clones the
// inbound headers (buildRequest) and Accept-Encoding is neither hop-by-hop
// nor one of the four permitted request mutations (README, "a proxy, never
// a client"), so whatever the client negotiated is what upstream sees and
// what comes back. That makes narrowing Accept-Encoding — dropping `br` so
// the sniffer only ever meets codecs it knows — a request mutation outside
// the permitted set, changing what the client asked for to suit spillway's
// telemetry. This test fails if anyone ever does it, including as an
// incidental side effect of some other header handling.
func TestAcceptEncodingForwardedVerbatim(t *testing.T) {
	// Deliberately awkward: multiple codecs, a q-value, and an explicit
	// refusal of identity. Nothing about it may be normalised, reordered
	// or trimmed.
	const sent = "br;q=1.0, zstd;q=0.9, gzip;q=0.5, identity;q=0"

	got := make(chan []string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Values("Accept-Encoding"):
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	h, _, _ := newUsageTestHandler(t, upstream.URL)
	front := httptest.NewServer(h)
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(testBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", sent)
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	select {
	case vals := <-got:
		if len(vals) != 1 || vals[0] != sent {
			t.Errorf("upstream saw Accept-Encoding %q, want exactly [%q] — spillway rewrote the client's "+
				"content negotiation, which is a request mutation outside the permitted set", vals, sent)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the request")
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

// TestTruncatedEncodedStreamRecordsZeroAndDoesNotFailRequest: the
// compressed paths (gzip, issue #121; br, issue #126) through the same
// scenario as TestTruncatedStreamRecordsZeroAndDoesNotFailRequest above —
// the connection dies with only half of a real compressed stream ever
// sent, no trailer, no clean end. The client still gets exactly the bytes
// that were flushed before the cut (untouched — decoding happens on the
// sniffer's own copy, never the client's), and the log shows zeros rather
// than a crash or a stale value.
//
// Driven off decodableEncodings itself rather than a hand-written list, so
// a codec added to the production table is carried through this scenario
// automatically instead of by whoever remembers to.
func TestTruncatedEncodedStreamRecordsZeroAndDoesNotFailRequest(t *testing.T) {
	for _, enc := range decodableEncodingNames() {
		t.Run(enc, func(t *testing.T) {
			full := encodeFor(t, enc, sseUsageBodyWithMarker())
			half := full[:len(full)/2]

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", enc)
				w.WriteHeader(http.StatusOK)
				w.Write(half)
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
				conn.Close() // abrupt: no trailer, no clean end
			}))
			defer upstream.Close()

			h, rl, _ := newUsageTestHandler(t, upstream.URL)
			front := httptest.NewServer(h)
			defer front.Close()

			resp := postMessagesRaw(t, front.URL, `{"model":"claude","max_tokens":1}`)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200 (headers were sent before the cut)", resp.StatusCode)
			}
			got, _ := io.ReadAll(resp.Body) // a read error is expected — the connection was cut
			if !bytes.Equal(got, half) {
				t.Errorf("client bytes differ from what was flushed before the cut:\n got=%d bytes\nwant=%d bytes", len(got), len(half))
			}

			e := waitForEntry(t, rl)
			if e.InputTokens != 0 || e.OutputTokens != 0 || e.CacheCreationInputTokens != 0 || e.CacheReadInputTokens != 0 {
				t.Errorf("usage counters = %+v, want all zero for a %s stream truncated mid-stream", e, enc)
			}
		})
	}
}

// TestCancelledEncodedRequestTerminatesDecoder: the client cancels its
// request context mid-stream, before the compressed stream ever completes
// — a cancelled request is the other termination path issue #121 calls out
// alongside an early-ended stream, and it applies to every codec in
// decodableEncodings, hence the loop. This must not hang the request
// handler waiting on the decode goroutine: waitForEntry's deadline below is
// the actual proof — a real deadlock (drainDecoded stuck reading the pipe,
// or the handler stuck in usage()) fails this test by timing out waiting
// for a log entry that never arrives, not by asserting a wrong value.
func TestCancelledEncodedRequestTerminatesDecoder(t *testing.T) {
	for _, enc := range decodableEncodingNames() {
		t.Run(enc, func(t *testing.T) {
			full := encodeFor(t, enc, sseUsageBodyWithMarker())
			half := len(full) / 2

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", enc)
				w.WriteHeader(http.StatusOK)
				w.Write(full[:half])
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
				<-r.Context().Done() // released once the cancelled client's connection tears down
			}))
			defer upstream.Close()

			h, rl, _ := newUsageTestHandler(t, upstream.URL)
			front := httptest.NewServer(h)
			defer front.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/messages", strings.NewReader(testBody))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			// rawClient, not front.Client(), and an explicit
			// Accept-Encoding: a client (or, via buildRequest's header
			// clone, spillway's OWN outbound leg to upstream) that
			// negotiates and transparently undoes gzip on its own would try
			// to decompress this deliberately never-ending stream itself
			// and block forever doing it — see setExplicitAcceptEncoding.
			setExplicitAcceptEncoding(req)
			resp, err := rawClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, half)
			if _, err := io.ReadFull(resp.Body, buf); err != nil {
				t.Fatalf("reading first chunk: %v", err)
			}
			cancel()
			resp.Body.Close()

			e := waitForEntry(t, rl)
			if e.InputTokens != 0 || e.OutputTokens != 0 || e.CacheCreationInputTokens != 0 || e.CacheReadInputTokens != 0 {
				t.Errorf("usage counters = %+v, want all zero for a cancelled, incomplete %s stream", e, enc)
			}
		})
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
