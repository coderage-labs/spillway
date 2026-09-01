package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

// usageSniffMax bounds how many bytes the sniffer will ever hold: a partial
// SSE line that never terminates, or a non-streaming body, each stop
// growing past this and give up quietly rather than buffer an unbounded
// amount of a very large response. It bounds memory ONLY for the sniffer's
// own bookkeeping — it never affects what reaches the client, which flows
// through io.Copy's ordinary small buffer regardless of response size (see
// writeResponse).
const usageSniffMax = 1 << 20 // 1 MiB

// usageTotals is the four counters from the response's usage block (issue
// #110). Zero value on any parse failure, truncation, or unsupported
// encoding — this is telemetry, and it must never affect the response the
// client receives.
type usageTotals struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

// usageFields is the `usage` object shape, wherever it appears. Only these
// four integers are ever decoded — the struct has no other fields to decode
// into, so nothing else in the payload (content, tool inputs, stop reason)
// can end up here even by accident.
type usageFields struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// usageEnvelope matches every shape usage arrives in:
//   - a non-streaming Messages response: {"type":"message", ..., "usage": {...}}
//   - an SSE message_start event:        {"type":"message_start","message":{"usage": {...}}}
//   - an SSE message_delta event:        {"type":"message_delta","usage": {"output_tokens": N}}
//
// Both Usage and Message.Usage are decoded unconditionally; whichever the
// payload actually has ends up populated, the other stays zero.
type usageEnvelope struct {
	Type    string `json:"type"`
	Message struct {
		Usage usageFields `json:"usage"`
	} `json:"message"`
	Usage usageFields `json:"usage"`
}

// usageSniffer observes response bytes as they stream past, without ever
// holding more than usageSniffMax of them, and without being able to fail
// the copy it's attached to: Write always reports success. It exists to be
// the second destination of an io.MultiWriter alongside the real client
// writer — see writeResponse — so parsing happens on the same bytes as they
// pass through, not on a buffered copy.
type usageSniffer struct {
	// skip is set once sniffing has given up for this response: an encoded
	// body (Content-Encoding present — spillway forwards the client's own
	// Accept-Encoding unmodified, so an upstream gzip/br response streams
	// through unparsed by design; decoding it just to sniff would be its
	// own source of bugs) or a body larger than this sniffer is willing to
	// hold.
	skip bool
	sse  bool

	// SSE mode: the current, possibly partial, line. Each complete line is
	// parsed and then discarded — never the whole stream.
	line []byte
	got  usageTotals

	// Non-streaming mode: the whole body, up to the cap, parsed once after
	// the copy finishes.
	buf  []byte
	over bool
}

// newUsageSniffer decides how to read the body from its headers alone,
// before any bytes have arrived.
func newUsageSniffer(contentType, contentEncoding string) *usageSniffer {
	enc := strings.ToLower(strings.TrimSpace(contentEncoding))
	if enc != "" && enc != "identity" {
		// Not decoded here (see the skip field's doc): recorded as zero,
		// same as any other parse failure.
		return &usageSniffer{skip: true}
	}
	ct := strings.ToLower(strings.TrimSpace(contentType))
	return &usageSniffer{sse: strings.HasPrefix(ct, "text/event-stream")}
}

// Write implements io.Writer. It never returns an error and never returns
// fewer bytes than it was given — a MultiWriter aborts the whole copy on
// any writer error, and this one must never be the reason a client's bytes
// stop moving.
func (s *usageSniffer) Write(b []byte) (int, error) {
	n := len(b)
	if s.skip {
		return n, nil
	}
	if s.sse {
		s.line = append(s.line, b...)
		for {
			i := bytes.IndexByte(s.line, '\n')
			if i < 0 {
				if len(s.line) > usageSniffMax {
					// A single line longer than this has stopped looking
					// like SSE; give up rather than buffer forever.
					s.skip = true
					s.line = nil
				}
				break
			}
			line := s.line[:i]
			s.line = s.line[i+1:]
			s.consumeSSELine(line)
		}
		return n, nil
	}
	if !s.over {
		if len(s.buf)+len(b) > usageSniffMax {
			s.over = true
			s.buf = nil
		} else {
			s.buf = append(s.buf, b...)
		}
	}
	return n, nil
}

// consumeSSELine parses one complete SSE line (no trailing newline). Only
// "data: " lines carry a payload; everything else (event:, id:, blank
// keep-alive lines, ": comment" lines) is silently ignored. A malformed
// data line — truncated mid-object by a cut connection, or simply not JSON
// — is skipped, never treated as an error: whatever was already parsed from
// earlier events in this same stream is kept.
func (s *usageSniffer) consumeSSELine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	const prefix = "data: "
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return
	}
	var env usageEnvelope
	if err := json.Unmarshal(line[len(prefix):], &env); err != nil {
		return
	}
	switch env.Type {
	case "message_start":
		// input_tokens and the two cache counters are set once here and
		// never change; message_start's own output_tokens is a small
		// placeholder (typically 1), kept only as a fallback in case the
		// stream is cut before any message_delta arrives.
		s.got.InputTokens = env.Message.Usage.InputTokens
		s.got.CacheCreationInputTokens = env.Message.Usage.CacheCreationInputTokens
		s.got.CacheReadInputTokens = env.Message.Usage.CacheReadInputTokens
		if env.Message.Usage.OutputTokens > 0 {
			s.got.OutputTokens = env.Message.Usage.OutputTokens
		}
	case "message_delta":
		// Cumulative running total; the last one seen before message_stop
		// (or before truncation) is the final count.
		if env.Usage.OutputTokens > 0 {
			s.got.OutputTokens = env.Usage.OutputTokens
		}
	}
}

// usage returns what was observed. Called once, after the copy loop this
// sniffer was attached to has finished — success, client disconnect, or
// upstream EOF make no difference: whatever was parsed by that point is
// final.
func (s *usageSniffer) usage() usageTotals {
	if s.skip || s.sse {
		return s.got // zero value if skipped or nothing ever parsed
	}
	if s.over || len(s.buf) == 0 {
		return usageTotals{}
	}
	var env usageEnvelope
	if err := json.Unmarshal(s.buf, &env); err != nil {
		return usageTotals{}
	}
	return usageTotals{
		InputTokens:              env.Usage.InputTokens,
		OutputTokens:             env.Usage.OutputTokens,
		CacheCreationInputTokens: env.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     env.Usage.CacheReadInputTokens,
	}
}
