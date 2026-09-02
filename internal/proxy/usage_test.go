package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

// capturingLogger returns a logger whose text output can be inspected, for
// tests that need to assert something was (or wasn't) logged.
func capturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// gzipBytes compresses s as a single gzip member, the shape a real upstream
// response arrives in.
func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatalf("gzip.Write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip.Close: %v", err)
	}
	return buf.Bytes()
}

// brotliBytes compresses s as a single brotli stream, the shape a
// Content-Encoding: br response arrives in (issue #126).
func brotliBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	bw := brotli.NewWriter(&buf)
	if _, err := bw.Write([]byte(s)); err != nil {
		t.Fatalf("brotli.Write: %v", err)
	}
	if err := bw.Close(); err != nil {
		t.Fatalf("brotli.Close: %v", err)
	}
	return buf.Bytes()
}

// testEncoders mirrors decodableEncodings: for every codec the sniffer
// claims to decode, this is how a test builds a body in that codec.
// TestEveryDecodableEncodingIsCovered enforces that the two stay in step,
// so adding a codec to the production table without also proving it
// records real tokens fails the build rather than shipping the exact
// silent hole issues #110, #121 and #126 each were.
var testEncoders = map[string]func(*testing.T, string) []byte{
	"gzip": gzipBytes,
	"br":   brotliBytes,
}

// allSniffableEncodings is every Content-Encoding a response can arrive in
// and still have its usage read: the decodable table plus identity, which
// is not an encoding but is a real header value. Sorted so subtest names
// are stable across runs (map iteration order is not).
func allSniffableEncodings() []string {
	encs := []string{"", "identity"}
	for enc := range decodableEncodings {
		encs = append(encs, enc)
	}
	sort.Strings(encs)
	return encs
}

// decodableEncodingNames is every entry in the production decode table, in
// a stable order.
func decodableEncodingNames() []string {
	encs := make([]string, 0, len(decodableEncodings))
	for enc := range decodableEncodings {
		encs = append(encs, enc)
	}
	sort.Strings(encs)
	return encs
}

// encodeFor wraps s in enc. Identity ("" and "identity") passes through
// unchanged; anything else must have a testEncoders entry.
func encodeFor(t *testing.T, enc, s string) []byte {
	t.Helper()
	if enc == "" || enc == "identity" {
		return []byte(s)
	}
	fn, ok := testEncoders[enc]
	if !ok {
		t.Fatalf("no test encoder for %q", enc)
	}
	return fn(t, s)
}

// waitForGoroutines polls until the live goroutine count drops to at most
// want, dumping every stack if it never does. Polling rather than a single
// sample: a goroutine that has already been unblocked still needs a
// scheduler tick to actually exit, so a bare post-condition sample is
// flaky in the passing direction as well as the failing one.
func waitForGoroutines(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= want {
			return
		}
		if time.Now().After(deadline) {
			stacks := make([]byte, 1<<20)
			stacks = stacks[:runtime.Stack(stacks, true)]
			t.Fatalf("goroutines = %d, want <= %d after every sniffer was joined — decode goroutines leaked\n%s", n, want, stacks)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sseUsageFixture is a minimal but realistic Anthropic Messages SSE stream:
// message_start carries input/cache usage, message_delta carries the final
// output_tokens, message_stop closes it out. Modeled on the real event
// shapes (design doc issue #110), not a synthetic simplification of them.
func sseUsageFixture() string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1234,"cache_creation_input_tokens":4165,"cache_read_input_tokens":1816,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

func TestUsageSnifferParsesSSEAcrossArbitraryChunkBoundaries(t *testing.T) {
	full := sseUsageFixture()
	s := newUsageSniffer("text/event-stream", "", testLogger())
	// Worst case for a line-buffering parser: one byte per Write, so every
	// "line" is reassembled across many calls.
	for i := 0; i < len(full); i++ {
		n, err := s.Write([]byte{full[i]})
		if err != nil || n != 1 {
			t.Fatalf("Write byte %d: n=%d err=%v", i, n, err)
		}
	}
	got := s.usage()
	want := usageTotals{InputTokens: 1234, OutputTokens: 42, CacheCreationInputTokens: 4165, CacheReadInputTokens: 1816}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func TestUsageSnifferParsesSSEDeliveredWhole(t *testing.T) {
	s := newUsageSniffer("text/event-stream", "", testLogger())
	if _, err := s.Write([]byte(sseUsageFixture())); err != nil {
		t.Fatal(err)
	}
	got := s.usage()
	want := usageTotals{InputTokens: 1234, OutputTokens: 42, CacheCreationInputTokens: 4165, CacheReadInputTokens: 1816}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func TestUsageSnifferParsesPlainJSON(t *testing.T) {
	s := newUsageSniffer("application/json", "", testLogger())
	body := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":30,"cache_read_input_tokens":40}}`
	if _, err := s.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	got := s.usage()
	want := usageTotals{InputTokens: 10, OutputTokens: 20, CacheCreationInputTokens: 30, CacheReadInputTokens: 40}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func TestUsageSnifferPlainJSONDeliveredInPieces(t *testing.T) {
	body := `{"id":"msg_1","usage":{"input_tokens":5,"output_tokens":6,"cache_creation_input_tokens":7,"cache_read_input_tokens":8}}`
	s := newUsageSniffer("application/json", "", testLogger())
	mid := len(body) / 2
	if _, err := s.Write([]byte(body[:mid])); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte(body[mid:])); err != nil {
		t.Fatal(err)
	}
	got := s.usage()
	want := usageTotals{InputTokens: 5, OutputTokens: 6, CacheCreationInputTokens: 7, CacheReadInputTokens: 8}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

// TestUsageSnifferTruncatedSSERecordsZero: the connection dies mid-object,
// before even message_start's line ever completes. Nothing usable was ever
// parsed, so the answer must be the zero value, not a panic or a partial
// guess.
func TestUsageSnifferTruncatedSSERecordsZero(t *testing.T) {
	s := newUsageSniffer("text/event-stream", "", testLogger())
	partial := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_toke`
	if _, err := s.Write([]byte(partial)); err != nil {
		t.Fatal(err)
	}
	if got := s.usage(); got != (usageTotals{}) {
		t.Errorf("usage = %+v, want zero value for a stream truncated before any complete event", got)
	}
}

// TestUsageSnifferTruncatedPlainJSONRecordsZero: same idea for the
// non-streaming path — a body cut off before the closing brace is not
// valid JSON, and must not be guessed at.
func TestUsageSnifferTruncatedPlainJSONRecordsZero(t *testing.T) {
	s := newUsageSniffer("application/json", "", testLogger())
	partial := `{"id":"msg_1","usage":{"input_tokens":10,"output_to`
	if _, err := s.Write([]byte(partial)); err != nil {
		t.Fatal(err)
	}
	if got := s.usage(); got != (usageTotals{}) {
		t.Errorf("usage = %+v, want zero value for a truncated JSON body", got)
	}
}

// TestUsageSnifferMalformedLineSkippedNotFatal: a garbage "data:" line
// (could be a keep-alive comment gone wrong, or genuine corruption) must be
// skipped, not treated as fatal — a later, valid event in the same stream
// still gets parsed.
func TestUsageSnifferMalformedLineSkippedNotFatal(t *testing.T) {
	s := newUsageSniffer("text/event-stream", "", testLogger())
	stream := "data: {this is not json\n\n" +
		"event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":7,"cache_creation_input_tokens":1,"cache_read_input_tokens":2,"output_tokens":1}}}` + "\n\n"
	if _, err := s.Write([]byte(stream)); err != nil {
		t.Fatal(err)
	}
	got := s.usage()
	want := usageTotals{InputTokens: 7, OutputTokens: 1, CacheCreationInputTokens: 1, CacheReadInputTokens: 2}
	if got != want {
		t.Errorf("usage = %+v, want %+v (malformed line should be skipped, not fatal)", got, want)
	}
}

// TestUsageSnifferSkipsUnsupportedEncodedBody: an encoding with no entry in
// decodableEncodings is never decoded — recorded as zero, same as any other
// parse failure, and never touching the bytes it's fed. zstd is the
// realistic next codec Anthropic could enable and deflate stands in for
// "anything else"; neither gzip (issue #121) nor br (issue #126) is in this
// list any more — see TestUsageSnifferRecordsRealTokensForEveryEncoding.
func TestUsageSnifferSkipsUnsupportedEncodedBody(t *testing.T) {
	for _, enc := range []string{"zstd", "deflate", "gzip, br"} {
		if _, decodable := decodableEncodings[enc]; decodable {
			t.Fatalf("test fixture stale: %q is now decodable, pick another unsupported value", enc)
		}
		s := newUsageSniffer("application/json", enc, testLogger())
		if !s.skip {
			t.Errorf("Content-Encoding=%s: skip = false, want true (unsupported, not decoded)", enc)
		}
		if _, err := s.Write([]byte("binarygarbage")); err != nil {
			t.Fatal(err)
		}
		if got := s.usage(); got != (usageTotals{}) {
			t.Errorf("Content-Encoding=%s: usage = %+v, want zero", enc, got)
		}
	}
}

// TestUsageSnifferUnsupportedEncodingLogged: an encoding the sniffer skips
// must be visible in the log — silently recording zeros for it is exactly
// what issue #121 happened because of, one layer up (gzip, which is now
// decoded). Logged once per distinct encoding value, not once per request.
func TestUsageSnifferUnsupportedEncodingLogged(t *testing.T) {
	logger, buf := capturingLogger()
	// Unique per test run, not just per test name: the dedup map is
	// process-lifetime, and `go test -count=N>1` reruns this same test in
	// the same process — a fixed literal here would only ever log on the
	// first of those N runs, failing every one after it for a reason that
	// has nothing to do with the sniffer (caught by exactly that: a -count=20
	// stress run of this file failed on iteration 2 before this fix).
	enc := fmt.Sprintf("br-test-once-%d", time.Now().UnixNano())
	for i := 0; i < 3; i++ {
		s := newUsageSniffer("application/json", enc, logger)
		_, _ = s.Write([]byte("binarygarbage"))
		_ = s.usage()
	}
	out := buf.String()
	if !strings.Contains(out, enc) {
		t.Fatalf("log output does not mention the unsupported encoding %q:\n%s", enc, out)
	}
	if n := strings.Count(out, "unsupported Content-Encoding"); n != 1 {
		t.Errorf("logged %d times across 3 sniffers with the same encoding, want exactly 1 (log once, not once per request):\n%s", n, out)
	}
}

// TestUsageSnifferIdentityEncodingStillParsed: "identity" is a real,
// explicit value for "not encoded" — must not be treated like gzip/br.
func TestUsageSnifferIdentityEncodingStillParsed(t *testing.T) {
	s := newUsageSniffer("application/json", "identity", testLogger())
	body := `{"usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4}}`
	if _, err := s.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	want := usageTotals{InputTokens: 1, OutputTokens: 2, CacheCreationInputTokens: 3, CacheReadInputTokens: 4}
	if got := s.usage(); got != want {
		t.Errorf("usage = %+v, want %+v — Content-Encoding: identity is not an encoding", got, want)
	}
}

// TestUsageSnifferBoundsMemoryOnOversizedJSONBody: a very large non-streaming
// body must not make the sniffer hold an unbounded amount of it. Once the
// cap is exceeded it gives up and releases what it had.
func TestUsageSnifferBoundsMemoryOnOversizedJSONBody(t *testing.T) {
	s := newUsageSniffer("application/json", "", testLogger())
	big := bytes.Repeat([]byte("x"), usageSniffMax+1)
	if _, err := s.Write(big); err != nil {
		t.Fatal(err)
	}
	if !s.over {
		t.Error("over = false, want true once usageSniffMax is exceeded")
	}
	if s.buf != nil {
		t.Error("buf still held after exceeding the cap — memory not bounded")
	}
	if got := s.usage(); got != (usageTotals{}) {
		t.Errorf("usage = %+v, want zero once the sniffer has given up on an oversized body", got)
	}
}

// TestUsageSnifferBoundsMemoryOnUnterminatedSSELine: an SSE stream whose
// current line never terminates (no '\n' ever arrives) must not grow
// forever either.
func TestUsageSnifferBoundsMemoryOnUnterminatedSSELine(t *testing.T) {
	s := newUsageSniffer("text/event-stream", "", testLogger())
	big := bytes.Repeat([]byte("x"), usageSniffMax+1)
	if _, err := s.Write(big); err != nil {
		t.Fatal(err)
	}
	if !s.lineGaveUp {
		t.Error("lineGaveUp = false, want true once a single unterminated line exceeds usageSniffMax")
	}
	if s.line != nil {
		t.Error("line buffer still held after exceeding the cap — memory not bounded")
	}
}

// TestUsageSnifferWriteNeverErrors: a MultiWriter aborts the whole copy on
// any writer error, so this sniffer must never be the reason client bytes
// stop moving, in any of its states.
//
// The zero-length chunk in the middle matters more than it looks: on a
// decoding sniffer that reaches the decoder as a zero-length pipe write,
// which brotli's Reader surfaces as a legal (0, nil) Read — see the loop
// in drainDecoded.
func TestUsageSnifferWriteNeverErrors(t *testing.T) {
	cases := []*usageSniffer{
		newUsageSniffer("text/event-stream", "", testLogger()),
		newUsageSniffer("application/json", "", testLogger()),
		newUsageSniffer("application/json", "zstd", testLogger()), // skipped
		newUsageSniffer("text/event-stream", "gzip", testLogger()),
		newUsageSniffer("text/event-stream", "br", testLogger()),
	}
	for i, s := range cases {
		for _, chunk := range []string{"garbage", "\x00\x01\x02", "", "data: {}\n\n"} {
			n, err := s.Write([]byte(chunk))
			if err != nil {
				t.Errorf("case %d: Write(%q) returned error: %v", i, chunk, err)
			}
			if n != len(chunk) {
				t.Errorf("case %d: Write(%q) = %d, want %d", i, chunk, n, len(chunk))
			}
		}
		_ = s.usage() // drains/joins any decode goroutine
	}
}

// --- issue #121: gzipped responses ---

// TestUsageSnifferDecodesGzippedSSE is the reported bug, at the unit level:
// a gzip-compressed SSE stream, delivered as Content-Encoding: gzip really
// arrives, must still yield the four usage counters — not zero.
func TestUsageSnifferDecodesGzippedSSE(t *testing.T) {
	s := newUsageSniffer("text/event-stream", "gzip", testLogger())
	if s.skip {
		t.Fatal("skip = true for gzip — gzip must be decoded, not skipped (issue #121)")
	}
	if _, err := s.Write(gzipBytes(t, sseUsageFixture())); err != nil {
		t.Fatal(err)
	}
	got := s.usage()
	want := usageTotals{InputTokens: 1234, OutputTokens: 42, CacheCreationInputTokens: 4165, CacheReadInputTokens: 1816}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

// TestUsageSnifferDecodesGzippedSSEAcrossArbitraryChunkBoundaries: the
// compressed bytes arrive one at a time, the worst case for the
// pipe-fed decode goroutine — every single Write hands the decoder one
// byte, and the decoder must still reassemble the full stream correctly.
func TestUsageSnifferDecodesGzippedSSEAcrossArbitraryChunkBoundaries(t *testing.T) {
	compressed := gzipBytes(t, sseUsageFixture())
	s := newUsageSniffer("text/event-stream", "gzip", testLogger())
	for i, b := range compressed {
		n, err := s.Write([]byte{b})
		if err != nil || n != 1 {
			t.Fatalf("Write byte %d: n=%d err=%v", i, n, err)
		}
	}
	got := s.usage()
	want := usageTotals{InputTokens: 1234, OutputTokens: 42, CacheCreationInputTokens: 4165, CacheReadInputTokens: 1816}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

// TestUsageSnifferDecodesGzippedPlainJSON: the non-streaming shape, gzipped
// — the decoded bytes must reach the same whole-body JSON parse path a
// plain (identity) JSON body does.
func TestUsageSnifferDecodesGzippedPlainJSON(t *testing.T) {
	body := `{"id":"msg_1","type":"message","usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":30,"cache_read_input_tokens":40}}`
	s := newUsageSniffer("application/json", "gzip", testLogger())
	if _, err := s.Write(gzipBytes(t, body)); err != nil {
		t.Fatal(err)
	}
	got := s.usage()
	want := usageTotals{InputTokens: 10, OutputTokens: 20, CacheCreationInputTokens: 30, CacheReadInputTokens: 40}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

// TestUsageSnifferCorruptGzipRecordsZero: bytes that merely start with the
// gzip magic number but are not a valid gzip member. Must record zero, not
// panic and not fail the Write.
func TestUsageSnifferCorruptGzipRecordsZero(t *testing.T) {
	s := newUsageSniffer("text/event-stream", "gzip", testLogger())
	// \x1f\x8b is the real gzip magic; everything after it is garbage, not
	// a valid deflate stream.
	garbage := append([]byte{0x1f, 0x8b, 0x08, 0x00, 0, 0, 0, 0}, []byte("not a real deflate stream at all")...)
	if _, err := s.Write(garbage); err != nil {
		t.Fatal(err)
	}
	if got := s.usage(); got != (usageTotals{}) {
		t.Errorf("usage = %+v, want zero for a corrupt gzip member", got)
	}
}

// TestUsageSnifferTruncatedGzipRecordsZero: a real gzip stream, cut off
// partway through — no trailer, an incomplete deflate block. Must record
// zero (or whatever was fully parsed before the cut) without hanging or
// erroring the Write.
func TestUsageSnifferTruncatedGzipRecordsZero(t *testing.T) {
	full := gzipBytes(t, sseUsageFixture())
	half := full[:len(full)/2]
	s := newUsageSniffer("text/event-stream", "gzip", testLogger())
	if _, err := s.Write(half); err != nil {
		t.Fatal(err)
	}
	if got := s.usage(); got != (usageTotals{}) {
		t.Errorf("usage = %+v, want zero for a gzip member truncated mid-stream", got)
	}
}

// TestUsageSnifferCapCountsDecodedBytes: the whole reason a streaming
// decoder is needed (issue #121) is that the size cap must bound DECODED
// bytes, not compressed ones. This body compresses from well over
// usageSniffMax down to a few KB (highly repetitive input) — if the cap
// were still being applied to the compressed size, this would sail through
// it and parsing would (wrongly) succeed.
//
// Sized to comfortably exceed the cap mid-stream (three times over, not an
// exact multiple of the decoder's internal read-chunk size): once total
// crosses usageSniffMax, drainDecoded can return on the read call that
// crossed it without needing anything more from the pipe, so it terminates
// on its own — proof the cap is enforced during decode, not only checked
// afterwards, and proof the goroutine cannot be left running. usage() is
// still the authoritative check (it works either way, since it always
// closes the pipe and waits — see the next assertion and
// TestUsageSnifferUnaffectedByExactChunkBoundary for the case where
// self-termination alone can't be relied on).
//
// Run against every entry in decodableEncodings, not just gzip: the cap
// lives in shared code, but "the decoder self-terminates rather than
// waiting for input it will never get" is a claim about the codec, and
// issue #126 is what a per-codec claim nobody checked costs.
func TestUsageSnifferCapCountsDecodedBytes(t *testing.T) {
	for _, enc := range decodableEncodingNames() {
		t.Run(enc, func(t *testing.T) {
			decodedSize := usageSniffMax*3 + 12345
			compressed := encodeFor(t, enc, strings.Repeat("x", decodedSize))
			if len(compressed) >= usageSniffMax {
				t.Fatalf("test fixture is not actually small when compressed (%d bytes) — cap test would be meaningless", len(compressed))
			}

			s := newUsageSniffer("application/json", enc, testLogger())
			if _, err := s.Write(compressed); err != nil {
				t.Fatal(err)
			}

			select {
			case <-s.done:
			case <-time.After(5 * time.Second):
				t.Fatal("drainDecoded did not terminate on its own after the decoded-size cap was exceeded")
			}

			if got := s.usage(); got != (usageTotals{}) {
				t.Errorf("usage = %+v, want zero once the decoded size exceeds usageSniffMax", got)
			}
		})
	}
}

// TestUsageSnifferUnaffectedByExactChunkBoundary: the pathological case
// where the cap happens to land exactly on a multiple of the decoder's
// internal read-chunk size, so the read that would cross it needs to touch
// the pipe once more — and there is nothing more to give it yet. This must
// NOT deadlock: usage() (always called by writeResponse, success, error,
// disconnect or cancellation alike — see its doc) closes the pipe first,
// which is what unblocks a read in exactly this situation, and only then
// waits. The bound below is what actually proves it — a real deadlock
// would hang the whole test binary.
func TestUsageSnifferUnaffectedByExactChunkBoundary(t *testing.T) {
	for _, enc := range decodableEncodingNames() {
		t.Run(enc, func(t *testing.T) {
			compressed := encodeFor(t, enc, strings.Repeat("x", usageSniffMax+4096))
			s := newUsageSniffer("application/json", enc, testLogger())
			if _, err := s.Write(compressed); err != nil {
				t.Fatal(err)
			}

			got := make(chan usageTotals, 1)
			go func() { got <- s.usage() }()
			select {
			case u := <-got:
				if u != (usageTotals{}) {
					t.Errorf("usage = %+v, want zero once the decoded size exceeds usageSniffMax", u)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("usage() did not return — decode goroutine deadlocked waiting on the pipe " +
					"instead of being unblocked by usage()'s Close")
			}
		})
	}
}

// TestUsageSnifferGzipUnaffectedBySingleByteWrites already covered above
// (TestUsageSnifferDecodesGzippedSSEAcrossArbitraryChunkBoundaries); kept
// as a separate name here would be redundant, noted for reviewers who go
// looking for it.

// TestUsageSnifferGzipEmptyStreamRecordsZero: Write is never called at all
// (e.g. a zero-length response body) — usage() must still return cleanly,
// closing the pipe and joining the decode goroutine rather than hanging.
func TestUsageSnifferGzipEmptyStreamRecordsZero(t *testing.T) {
	s := newUsageSniffer("text/event-stream", "gzip", testLogger())
	done := make(chan usageTotals, 1)
	go func() { done <- s.usage() }()
	select {
	case got := <-done:
		if got != (usageTotals{}) {
			t.Errorf("usage = %+v, want zero for a stream with no bytes at all", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("usage() hung on an empty gzip stream")
	}
}

// --- issue #126: the encoding set as a table, brotli as its first new entry ---

// sseUsageWant is what sseUsageFixture must produce when the whole stream
// is read. Named because several tests below assert it, and because "real,
// non-zero token values" is the actual claim under test — issue #110
// shipped on tests that only checked nothing errored.
var sseUsageWant = usageTotals{InputTokens: 1234, OutputTokens: 42, CacheCreationInputTokens: 4165, CacheReadInputTokens: 1816}

// sseUsagePartialWant is what a stream cut after message_start but before
// message_delta legitimately yields: the input and cache counters, plus
// message_start's placeholder output_tokens of 1. A truncated stream is
// allowed to produce this or nothing; it must never produce sseUsageWant,
// which would mean counters were invented rather than read.
var sseUsagePartialWant = usageTotals{InputTokens: 1234, OutputTokens: 1, CacheCreationInputTokens: 4165, CacheReadInputTokens: 1816}

// jsonUsageFixture is the non-streaming Messages shape.
const jsonUsageFixture = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],` +
	`"usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":30,"cache_read_input_tokens":40}}`

var jsonUsageWant = usageTotals{InputTokens: 10, OutputTokens: 20, CacheCreationInputTokens: 30, CacheReadInputTokens: 40}

// encName gives an empty Content-Encoding a printable subtest name.
func encName(enc string) string {
	if enc == "" {
		return "absent"
	}
	return enc
}

// TestEveryDecodableEncodingIsCovered is the guard that keeps the table
// honest. decodableEncodings is deliberately the one place a codec is
// added; this makes adding one without a matching test fixture a build-time
// failure, so the next codec cannot repeat #110/#121/#126 — shipping a
// decode path nobody proved records real tokens.
func TestEveryDecodableEncodingIsCovered(t *testing.T) {
	for _, enc := range decodableEncodingNames() {
		if _, ok := testEncoders[enc]; !ok {
			t.Errorf("decodableEncodings has %q but testEncoders does not — add a fixture so the "+
				"per-encoding token, truncation and goroutine tests actually exercise it", enc)
		}
	}
	for enc := range testEncoders {
		if _, ok := decodableEncodings[enc]; !ok {
			t.Errorf("testEncoders has %q but decodableEncodings does not — stale fixture", enc)
		}
	}
	// Sanity on the table itself: gzip (#121) and br (#126) are the two
	// encodings real Anthropic traffic has actually been observed in.
	for _, enc := range []string{"gzip", "br"} {
		if _, ok := decodableEncodings[enc]; !ok {
			t.Errorf("decodableEncodings is missing %q — an encoding measured on real traffic", enc)
		}
	}
}

// TestUsageSnifferRecordsRealTokensForEveryEncoding is the central claim of
// issue #126, and the one issues #110 and #121 each failed at in turn: for
// EVERY encoding a response can arrive in, the four counters must come back
// with the real values from the payload. Asserting "no error" is what let
// #110 ship blind, so every case below asserts the exact non-zero totals
// and separately refuses a zero result.
//
// Three deliveries per encoding, because the sniffer sees whatever chunk
// boundaries the network gives it: the whole body in one Write, one byte
// per Write (worst case for both the SSE line buffer and the pipe-fed
// decoder), and the non-streaming JSON shape.
func TestUsageSnifferRecordsRealTokensForEveryEncoding(t *testing.T) {
	for _, enc := range allSniffableEncodings() {
		t.Run(encName(enc), func(t *testing.T) {
			t.Run("sse-whole", func(t *testing.T) {
				s := newUsageSniffer("text/event-stream", enc, testLogger())
				if _, err := s.Write(encodeFor(t, enc, sseUsageFixture())); err != nil {
					t.Fatal(err)
				}
				assertRealTokens(t, s.usage(), sseUsageWant)
			})

			t.Run("sse-one-byte-at-a-time", func(t *testing.T) {
				wire := encodeFor(t, enc, sseUsageFixture())
				s := newUsageSniffer("text/event-stream", enc, testLogger())
				for i := range wire {
					n, err := s.Write(wire[i : i+1])
					if err != nil || n != 1 {
						t.Fatalf("Write byte %d: n=%d err=%v", i, n, err)
					}
				}
				assertRealTokens(t, s.usage(), sseUsageWant)
			})

			t.Run("json-whole", func(t *testing.T) {
				s := newUsageSniffer("application/json", enc, testLogger())
				if _, err := s.Write(encodeFor(t, enc, jsonUsageFixture)); err != nil {
					t.Fatal(err)
				}
				assertRealTokens(t, s.usage(), jsonUsageWant)
			})
		})
	}
}

// assertRealTokens fails both on the wrong totals and, separately and
// loudly, on the all-zero result that is the actual production symptom of
// every one of #110, #121 and #126.
func assertRealTokens(t *testing.T, got, want usageTotals) {
	t.Helper()
	if got == (usageTotals{}) {
		t.Fatalf("usage = all zero, want %+v — this is the exact symptom of issues #110/#121/#126: "+
			"a usage row recorded with no tokens in it", want)
	}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

// encodedBodyCase is one shape of response body in a given encoding.
type encodedBodyCase struct {
	name string
	body []byte
}

// brokenBodyCases is every way a response body can arrive damaged, built
// in the given encoding: cut in half, cut to almost nothing, never sent at
// all, plaintext where a compressed stream was promised, and a stream whose
// tail has been bit-flipped. None of these may panic, hang, leak a
// goroutine, alter the client's bytes, or invent counters.
func brokenBodyCases(t *testing.T, enc string) []encodedBodyCase {
	t.Helper()
	full := encodeFor(t, enc, sseUsageFixture())
	corrupt := append([]byte(nil), full...)
	for i := len(corrupt) / 3; i < len(corrupt); i++ {
		corrupt[i] ^= 0x5a
	}
	return []encodedBodyCase{
		{name: "truncated-half", body: full[:len(full)/2]},
		{name: "truncated-to-two-bytes", body: full[:2]},
		{name: "empty", body: nil},
		{name: "plaintext-not-a-stream", body: []byte("not compressed at all, just plain text bytes")},
		{name: "corrupt-tail", body: corrupt},
	}
}

// TestUsageSnifferBrokenStreamIsInert: a truncated or corrupt body in ANY
// encoding must neither panic, nor hang, nor produce counters that were not
// actually in the delivered bytes. The bounded wait on usage() is the
// hang check — a decoder blocked on a pipe that will never be written
// again fails here by timing out, not by asserting a wrong value.
func TestUsageSnifferBrokenStreamIsInert(t *testing.T) {
	for _, enc := range allSniffableEncodings() {
		t.Run(encName(enc), func(t *testing.T) {
			for _, tc := range brokenBodyCases(t, enc) {
				t.Run(tc.name, func(t *testing.T) {
					s := newUsageSniffer("text/event-stream", enc, testLogger())
					if _, err := s.Write(tc.body); err != nil {
						t.Fatalf("Write returned an error: %v", err)
					}
					done := make(chan usageTotals, 1)
					go func() { done <- s.usage() }()
					select {
					case got := <-done:
						// A damaged stream may legitimately yield nothing,
						// or exactly what message_start carried if that
						// event survived the cut. It must never yield the
						// complete totals — those only exist in bytes that
						// were never delivered.
						if got != (usageTotals{}) && got != sseUsagePartialWant {
							t.Errorf("usage = %+v, want zero or %+v (nothing beyond the delivered prefix)", got, sseUsagePartialWant)
						}
						if got == sseUsageWant {
							t.Errorf("usage = %+v — the complete totals from a %s body, which means counters were invented", got, tc.name)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("usage() did not return — the decoder hung on a broken stream instead of being unblocked by usage()'s Close")
					}
				})
			}
		})
	}
}

// TestUsageSnifferLeavesClientBytesUntouched: the sniffer tees, it does not
// transform. Assembled exactly as writeResponse does — client writer first
// in an io.MultiWriter, sniffer second, one io.Copy — and compared with a
// raw bytes.Equal against the upstream bytes, deliberately not a normalised
// or decoded comparison: for gzip and br the client's bytes are the
// COMPRESSED ones and must stay so, even though usage is read from their
// decompressed content. Broken bodies are included because a decoder that
// errors must not disturb the copy either.
func TestUsageSnifferLeavesClientBytesUntouched(t *testing.T) {
	for _, enc := range allSniffableEncodings() {
		t.Run(encName(enc), func(t *testing.T) {
			cases := append([]encodedBodyCase{
				{name: "complete-sse", body: encodeFor(t, enc, sseUsageFixture())},
				{name: "complete-json", body: encodeFor(t, enc, jsonUsageFixture)},
			}, brokenBodyCases(t, enc)...)

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					// Kept before the copy: the sniffer is handed the same
					// backing array io.Copy reuses for the client's writer,
					// so "did not alter the stream" also means "did not
					// write into the buffer it was lent".
					want := append([]byte(nil), tc.body...)

					var client bytes.Buffer
					s := newUsageSniffer("text/event-stream", enc, testLogger())
					if _, err := io.Copy(io.MultiWriter(&client, s), bytes.NewReader(tc.body)); err != nil {
						t.Fatalf("io.Copy through the MultiWriter failed: %v", err)
					}
					_ = s.usage()
					if !bytes.Equal(client.Bytes(), want) {
						t.Errorf("client received %d bytes, upstream sent %d — the sniffer altered the stream\n got=%q\nwant=%q",
							client.Len(), len(want), client.Bytes(), want)
					}
					if !bytes.Equal(tc.body, want) {
						t.Errorf("the sniffer mutated the buffer it was handed\n got=%q\nwant=%q", tc.body, want)
					}
				})
			}
		})
	}
}

// TestUsageSnifferDecodeGoroutineJoinedNotLeaked: every decoding sniffer
// starts exactly one goroutine, and usage() must join every one of them —
// including on a truncated stream, where the decoder is parked inside
// pr.Read waiting for input that will never arrive. A decoder that leaks a
// goroutine per response is worse than the bug being fixed: spillway is a
// long-lived daemon and every /v1/messages response builds one of these.
//
// The mid-test count is not decoration: it proves the goroutines really
// were running, so the after-count is measuring something. A tolerance is
// allowed on the way back down only for unrelated runtime/test-harness
// goroutines — a genuine leak is `sniffers` goroutines, two orders of
// magnitude above it.
func TestUsageSnifferDecodeGoroutineJoinedNotLeaked(t *testing.T) {
	const (
		sniffers  = 100
		tolerance = 5
	)
	for _, enc := range decodableEncodingNames() {
		t.Run(enc, func(t *testing.T) {
			// A half-delivered stream: the decoder has a valid start and
			// is blocked waiting for the rest, which is the state that
			// leaks if usage() does not close the pipe and join.
			full := encodeFor(t, enc, sseUsageFixture())
			half := full[:len(full)/2]

			base := runtime.NumGoroutine()
			live := make([]*usageSniffer, 0, sniffers)
			for i := 0; i < sniffers; i++ {
				s := newUsageSniffer("text/event-stream", enc, testLogger())
				if _, err := s.Write(half); err != nil {
					t.Fatal(err)
				}
				live = append(live, s)
			}

			// Give the freshly spawned goroutines a moment to be counted,
			// then assert they exist at all.
			peak := runtime.NumGoroutine()
			for deadline := time.Now().Add(2 * time.Second); peak < base+sniffers/2 && time.Now().Before(deadline); {
				time.Sleep(5 * time.Millisecond)
				peak = runtime.NumGoroutine()
			}
			if peak < base+sniffers/2 {
				t.Fatalf("goroutines only reached %d from a base of %d after %d decoding sniffers — "+
					"the decode goroutines are not running, so the leak check below would prove nothing",
					peak, base, sniffers)
			}

			for _, s := range live {
				if got := s.usage(); got != (usageTotals{}) && got != sseUsagePartialWant {
					t.Fatalf("usage = %+v, want zero or %+v", got, sseUsagePartialWant)
				}
			}
			waitForGoroutines(t, base+tolerance)
		})
	}
}

// BenchmarkUsageSnifferPlainSSE and BenchmarkUsageSnifferGzipSSE measure the
// actual cost of issue #121's fix: decoding sits on the response hot path,
// and the task is to keep that overhead honest, not to eliminate it. Each
// iteration builds a fresh sniffer (matching production, where one is
// constructed per response) and writes a realistic-sized SSE payload
// through it in 4KiB chunks, the same granularity io.Copy's default buffer
// uses in writeResponse.
func benchmarkSSEPayload(n int) string {
	var b strings.Builder
	b.Grow(n)
	for b.Len() < n {
		b.WriteString(`event: content_block_delta` + "\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the quick brown fox jumps over the lazy dog"}}` + "\n\n")
	}
	b.WriteString(sseUsageFixture())
	return b.String()
}

func BenchmarkUsageSnifferPlainSSE(b *testing.B) {
	payload := []byte(benchmarkSSEPayload(64 << 10)) // 64KiB, a representative completion
	logger := testLogger()
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		s := newUsageSniffer("text/event-stream", "", logger)
		for off := 0; off < len(payload); off += 4096 {
			end := off + 4096
			if end > len(payload) {
				end = len(payload)
			}
			_, _ = s.Write(payload[off:end])
		}
		_ = s.usage()
	}
}

func BenchmarkUsageSnifferGzipSSE(b *testing.B) {
	payload := benchmarkSSEPayload(64 << 10)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(payload)); err != nil {
		b.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		b.Fatal(err)
	}
	benchmarkEncodedSniffer(b, "gzip", payload, buf.Bytes())
}

// BenchmarkUsageSnifferBrotliSSE is the issue #126 counterpart. Brotli
// decoding is measurably dearer than gunzip, and this sits on the response
// hot path, so the cost is worth being able to see rather than assume.
func BenchmarkUsageSnifferBrotliSSE(b *testing.B) {
	payload := benchmarkSSEPayload(64 << 10)
	var buf bytes.Buffer
	bw := brotli.NewWriter(&buf)
	if _, err := bw.Write([]byte(payload)); err != nil {
		b.Fatal(err)
	}
	if err := bw.Close(); err != nil {
		b.Fatal(err)
	}
	benchmarkEncodedSniffer(b, "br", payload, buf.Bytes())
}

func benchmarkEncodedSniffer(b *testing.B, enc, payload string, compressed []byte) {
	b.Helper()
	logger := testLogger()
	b.ReportAllocs()
	b.SetBytes(int64(len(payload))) // decoded bytes/op, comparable to the plain benchmark
	for i := 0; i < b.N; i++ {
		s := newUsageSniffer("text/event-stream", enc, logger)
		for off := 0; off < len(compressed); off += 4096 {
			end := off + 4096
			if end > len(compressed) {
				end = len(compressed)
			}
			_, _ = s.Write(compressed[off:end])
		}
		_ = s.usage()
	}
}
