package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
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

// TestUsageSnifferSkipsUnsupportedEncodedBody: an encoding this sniffer has
// no decoder for (issue #121 asks specifically about br; deflate stands in
// for "anything else") is never decoded — recorded as zero, same as any
// other parse failure, and never touching the bytes it's fed. gzip is NOT
// in this list any more: see TestUsageSnifferDecodesGzippedSSE.
func TestUsageSnifferSkipsUnsupportedEncodedBody(t *testing.T) {
	for _, enc := range []string{"br", "deflate"} {
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
func TestUsageSnifferWriteNeverErrors(t *testing.T) {
	cases := []*usageSniffer{
		newUsageSniffer("text/event-stream", "", testLogger()),
		newUsageSniffer("application/json", "", testLogger()),
		newUsageSniffer("application/json", "br", testLogger()),
		newUsageSniffer("text/event-stream", "gzip", testLogger()),
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
		_ = s.usage() // drains/joins the gzip case's decode goroutine
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

// TestUsageSnifferGzipCapCountsDecodedBytes: the whole reason a streaming
// gunzip is needed (issue #121) is that the size cap must bound DECODED
// bytes, not compressed ones. This body compresses from well over
// usageSniffMax down to a few KB (highly repetitive input) — if the cap
// were still being applied to the compressed size, this would sail through
// it and parsing would (wrongly) succeed.
//
// Sized to comfortably exceed the cap mid-stream (three times over, not an
// exact multiple of the decoder's internal read-chunk size): once total
// crosses usageSniffMax, drainGzip can return on the read call that
// crossed it without needing anything more from the pipe, so it terminates
// on its own — proof the cap is enforced during decode, not only checked
// afterwards, and proof the goroutine cannot be left running. usage() is
// still the authoritative check (it works either way, since it always
// closes the pipe and waits — see the next assertion and
// TestUsageSnifferGzipUnaffectedByExactChunkBoundary for the case where
// self-termination alone can't be relied on).
func TestUsageSnifferGzipCapCountsDecodedBytes(t *testing.T) {
	decodedSize := usageSniffMax*3 + 12345
	huge := strings.Repeat("x", decodedSize)
	compressed := gzipBytes(t, huge)
	if len(compressed) >= usageSniffMax {
		t.Fatalf("test fixture is not actually small when compressed (%d bytes) — cap test would be meaningless", len(compressed))
	}

	s := newUsageSniffer("application/json", "gzip", testLogger())
	if _, err := s.Write(compressed); err != nil {
		t.Fatal(err)
	}

	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainGzip did not terminate on its own after the decoded-size cap was exceeded")
	}

	if got := s.usage(); got != (usageTotals{}) {
		t.Errorf("usage = %+v, want zero once the decoded size exceeds usageSniffMax", got)
	}
}

// TestUsageSnifferGzipUnaffectedByExactChunkBoundary: the pathological case
// where the cap happens to land exactly on a multiple of the decoder's
// internal read-chunk size, so the read that would cross it needs to touch
// the pipe once more — and there is nothing more to give it yet. This must
// NOT deadlock: usage() (always called by writeResponse, success, error,
// disconnect or cancellation alike — see its doc) closes the pipe first,
// which is what unblocks a read in exactly this situation, and only then
// waits. The bound below is what actually proves it — a real deadlock
// would hang the whole test binary.
func TestUsageSnifferGzipUnaffectedByExactChunkBoundary(t *testing.T) {
	compressed := gzipBytes(t, strings.Repeat("x", usageSniffMax+4096))
	s := newUsageSniffer("application/json", "gzip", testLogger())
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
	case <-time.After(2 * time.Second):
		t.Fatal("usage() did not return — decode goroutine deadlocked waiting on the pipe " +
			"instead of being unblocked by usage()'s Close")
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
	compressed := buf.Bytes()
	logger := testLogger()
	b.ReportAllocs()
	b.SetBytes(int64(len(payload))) // decoded bytes/op, comparable to the plain benchmark
	for i := 0; i < b.N; i++ {
		s := newUsageSniffer("text/event-stream", "gzip", logger)
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
