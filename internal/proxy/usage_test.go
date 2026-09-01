package proxy

import (
	"bytes"
	"testing"
)

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
	s := newUsageSniffer("text/event-stream", "")
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
	s := newUsageSniffer("text/event-stream", "")
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
	s := newUsageSniffer("application/json", "")
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
	s := newUsageSniffer("application/json", "")
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
	s := newUsageSniffer("text/event-stream", "")
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
	s := newUsageSniffer("application/json", "")
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
	s := newUsageSniffer("text/event-stream", "")
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

// TestUsageSnifferSkipsEncodedBody: spillway forwards the client's own
// Accept-Encoding header unmodified, so an upstream response can legitimately
// arrive gzip/br-encoded. This sniffer never decodes it — recorded as zero,
// same as any other parse failure, and never touching the bytes it's fed.
func TestUsageSnifferSkipsEncodedBody(t *testing.T) {
	for _, enc := range []string{"gzip", "br", "deflate"} {
		s := newUsageSniffer("application/json", enc)
		if _, err := s.Write([]byte("\x1f\x8b\x08\x00binarygarbage")); err != nil {
			t.Fatal(err)
		}
		if got := s.usage(); got != (usageTotals{}) {
			t.Errorf("Content-Encoding=%s: usage = %+v, want zero", enc, got)
		}
	}
}

// TestUsageSnifferIdentityEncodingStillParsed: "identity" is a real,
// explicit value for "not encoded" — must not be treated like gzip/br.
func TestUsageSnifferIdentityEncodingStillParsed(t *testing.T) {
	s := newUsageSniffer("application/json", "identity")
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
	s := newUsageSniffer("application/json", "")
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
	s := newUsageSniffer("text/event-stream", "")
	big := bytes.Repeat([]byte("x"), usageSniffMax+1)
	if _, err := s.Write(big); err != nil {
		t.Fatal(err)
	}
	if !s.skip {
		t.Error("skip = false, want true once a single unterminated line exceeds usageSniffMax")
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
		newUsageSniffer("text/event-stream", ""),
		newUsageSniffer("application/json", ""),
		newUsageSniffer("application/json", "gzip"),
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
	}
}
