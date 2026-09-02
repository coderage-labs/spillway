package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
)

// usageSniffMax bounds how many bytes the sniffer will ever hold: a partial
// SSE line that never terminates, or a non-streaming body, each stop
// growing past this and give up quietly rather than buffer an unbounded
// amount of a very large response. It bounds memory ONLY for the sniffer's
// own bookkeeping — it never affects what reaches the client, which flows
// through io.Copy's ordinary small buffer regardless of response size (see
// writeResponse).
//
// For a compressed response (gzip, issue #121; brotli, issue #126) this is
// the cap on DECODED bytes, not compressed ones — a compressed stream
// expands, so capping the compressed input would stop meaning anything.
// See drainDecoded.
const usageSniffMax = 1 << 20 // 1 MiB

// bodyDecoder wraps the sniffer's own copy of an encoded response body in a
// reader yielding plaintext. It is only ever handed the read end of an
// io.Pipe, so a decoder wanting more input than has arrived blocks inside
// Read — which is what makes closing that pipe (drainDecoded's deferred
// cleanup, and usage()'s Close of the write end) the universal unblocker,
// whatever the codec.
//
// Construction is allowed to fail because some decoders validate a header
// eagerly, and a Close is always returned because some hold resources.
// Neither is true of both of today's codecs, which is exactly why the
// signature covers both rather than the union of what they happen to need.
type bodyDecoder func(io.Reader) (io.ReadCloser, error)

// decodableEncodings is the entire set of Content-Encoding values the usage
// sniffer can read, keyed by the lower-cased token. Anything else — bar
// identity, which is not an encoding — is skipped and logged once; see
// newUsageSniffer.
//
// A table rather than a switch, deliberately. Issue #110 shipped this
// measurement and it recorded nothing, because Anthropic gzips SSE and the
// sniffer skipped every encoded body. Issue #121 added a gzip branch.
// Issue #126 was the same bug wearing a different codec: Anthropic started
// returning Content-Encoding: br and 3.4% of /v1/messages rows went back to
// zeros, because `br` was a branch nobody had written. Three incidents in,
// the shape of the fix matters more than the codec — the next one (zstd is
// the realistic candidate) is one entry here and nothing else. The pipe,
// the decode goroutine and its unconditional teardown, the decoded-byte
// cap, the truncation handling and the SSE/JSON parse path are all shared
// and all already tested.
//
// Nothing here ever touches the bytes the client receives: the sniffer is
// the second leg of an io.MultiWriter, so decoding happens on a copy and
// the client's stream stays byte-identical to the upstream's. See
// writeResponse.
var decodableEncodings = map[string]bodyDecoder{
	"gzip": func(r io.Reader) (io.ReadCloser, error) {
		// gzip.NewReader validates the member header eagerly, so it can
		// fail right here — a decoder that will not construct records
		// zero, same as any other parse failure.
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		return zr, nil
	},
	"br": func(r io.Reader) (io.ReadCloser, error) {
		// Brotli has no stdlib implementation, hence the one dependency.
		// brotli.Reader is lazy — it reads nothing until the first Read —
		// so construction cannot fail and cannot block, and it holds
		// nothing that needs closing.
		return io.NopCloser(brotli.NewReader(r)), nil
	},
}

// unsupportedEncLogged records, process-lifetime, which Content-Encoding
// values have already produced the "unsupported" warning below — so a
// stream of responses in an encoding this sniffer can't read logs it once,
// not once per request. Issue #121 happened because a skip like this was
// silent; the fix is visibility, not necessarily decoding everything.
var unsupportedEncLogged sync.Map // map[string]struct{}

// logUnsupportedEncodingOnce warns exactly once per distinct
// Content-Encoding value that the usage sniffer is giving up on it and
// will record zero counters for every such response. Anthropic is known to
// send gzip, br or identity today, all of which are handled; this warning
// is the only reason issue #126 was ever noticed, and it stays for
// whatever comes next.
func logUnsupportedEncodingOnce(logger *slog.Logger, enc string) {
	if _, loaded := unsupportedEncLogged.LoadOrStore(enc, struct{}{}); loaded {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("usage sniffer: unsupported Content-Encoding, usage will not be recorded for these responses",
		"encoding", enc)
}

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

// errUsageSniffDone is the error drainDecoded's read end closes with once it
// has stopped reading, for any reason (clean end, decode error, or the
// decoded-size cap). It is never inspected — only its presence matters: it
// makes any Write on the other end of the pipe that is blocked, or arrives
// later, fail immediately instead of hanging. See drainDecoded and Write.
var errUsageSniffDone = errors.New("usage sniffer: done reading")

// usageSniffer observes response bytes as they stream past, without ever
// holding more than usageSniffMax of them, and without being able to fail
// the copy it's attached to: Write always reports success. It exists to be
// the second destination of an io.MultiWriter alongside the real client
// writer — see writeResponse — so parsing happens on the same bytes as they
// pass through, not on a buffered copy.
//
// Ownership (the decode path adds a second goroutine, so this needs
// spelling out): when dec is false, every field below is touched only by
// whatever goroutine calls Write and then usage — normally the same
// goroutine, since writeResponse calls both in sequence. When dec is true,
// Write only ever forwards bytes into the pipe; line, lineGaveUp, got, buf
// and over are owned exclusively by drainDecoded's goroutine until it
// closes done, and usage() will not read any of them before receiving from
// done — that receive is what makes the handoff race-free (see usage()).
type usageSniffer struct {
	// skip is set once, at construction, for an encoding with no entry in
	// decodableEncodings (and which is not identity). Immutable after that
	// — safe to read from Write without synchronization even while a
	// decode goroutine may be running for a *different* instance.
	skip bool
	sse  bool

	// Compressed-body decoding (gzip: issue #121; br: issue #126). Write
	// forwards bytes into pw; drainDecoded reads the other end, decodes,
	// and feeds the plaintext through exactly the same parsing (feed) an
	// unencoded body goes through. done closes when drainDecoded returns,
	// however it returns — usage() waits on it before reading anything
	// drainDecoded wrote.
	dec  bool
	pw   *io.PipeWriter
	done chan struct{}

	// SSE mode: the current, possibly partial, line. Each complete line is
	// parsed and then discarded — never the whole stream. lineGaveUp marks
	// a line that grew past usageSniffMax without ever terminating: parsing
	// stops for the rest of this response, but whatever was already parsed
	// into got is kept.
	line       []byte
	lineGaveUp bool
	got        usageTotals

	// Non-streaming mode: the whole body, up to the cap, parsed once after
	// the copy finishes.
	buf  []byte
	over bool
}

// newUsageSniffer decides how to read the body from its headers alone,
// before any bytes have arrived. logger is used only for the one
// unsupported-encoding warning below (issue #121) — nil is fine (falls
// back to slog.Default), kept optional so unit tests that don't care about
// logging can omit it.
//
// Note what this deliberately does NOT do: it never influences what
// spillway asks upstream for. The client's Accept-Encoding is forwarded
// verbatim — buildRequest clones the inbound headers, and Accept-Encoding
// is neither hop-by-hop nor one of the permitted request mutations (see
// README, "a proxy, never a client") — so the encoding arriving here is
// the one the client itself negotiated. Narrowing Accept-Encoding to dodge
// a codec this sniffer cannot read would be a mutation outside that set,
// and telemetry does not get to change what the client asked for. The
// sniffer adapts to the encoding; the encoding is never adapted to the
// sniffer.
func newUsageSniffer(contentType, contentEncoding string, logger *slog.Logger) *usageSniffer {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	sse := strings.HasPrefix(ct, "text/event-stream")

	enc := strings.ToLower(strings.TrimSpace(contentEncoding))
	if enc == "" || enc == "identity" {
		// "identity" is a real, explicit spelling of "not encoded".
		return &usageSniffer{sse: sse}
	}
	newDecoder, ok := decodableEncodings[enc]
	if !ok {
		// zstd, deflate, a multi-codec list, or anything else Anthropic
		// might one day send: not decoded. Recorded as zero, same as any
		// other parse failure — but logged, once, so an unsupported
		// encoding is visible rather than silently producing the exact
		// symptom issues #110, #121 and #126 all reported.
		logUnsupportedEncodingOnce(logger, enc)
		return &usageSniffer{skip: true}
	}
	s := &usageSniffer{sse: sse, dec: true, done: make(chan struct{})}
	pr, pw := io.Pipe()
	s.pw = pw
	go s.drainDecoded(newDecoder, pr)
	return s
}

// Write implements io.Writer. It never returns an error and never returns
// fewer bytes than it was given — a MultiWriter aborts the whole copy on
// any writer error, and this one must never be the reason a client's bytes
// stop moving.
func (s *usageSniffer) Write(b []byte) (int, error) {
	n := len(b)
	switch {
	case s.skip:
		return n, nil
	case s.dec:
		// Hand off to drainDecoded via the pipe. The error is never
		// inspected: once drainDecoded has stopped reading — cleanly, on a
		// decode error, or past the decoded-size cap — it closes the read
		// end (see drainDecoded), which makes this fail immediately rather
		// than block. Either way, this Write must never be the reason
		// client bytes stop moving, so the outcome here is always success.
		_, _ = s.pw.Write(b)
		return n, nil
	default:
		s.feed(b)
		return n, nil
	}
}

// drainDecoded is the sole reader of pr and, for as long as it runs, the
// sole owner of every field usage() eventually reads (see usageSniffer's
// doc). It runs decompressed bytes through feed() exactly as an unencoded
// body would go through Write — same SSE/JSON parsing, same truncation
// handling, same usageSniffMax cap, now measured in decoded bytes.
//
// The deferred close is the actual fix for the failure mode issue #121
// calls out ("a naive io.Pipe plus goroutine can leak or block if the
// stream ends early or the client disconnects"): closing the read end,
// unconditionally, on every return path — clean EOF, a corrupt or
// truncated stream, or the cap being hit — guarantees that any Write on
// the other end of this pipe that is currently blocked, or arrives later,
// fails immediately instead of waiting for a Read that will never come.
//
// Nothing here can block forever, and that is a property of the pipe
// rather than of any particular codec: a decoder can only ever block
// inside pr.Read, and closing pw (see usage()) unblocks that immediately
// with io.EOF, so this goroutine cannot outlive the writeResponse call
// that created it. That is what makes adding an entry to
// decodableEncodings safe without re-reasoning about goroutine lifetime.
func (s *usageSniffer) drainDecoded(newDecoder bodyDecoder, pr *io.PipeReader) {
	defer func() {
		_ = pr.CloseWithError(errUsageSniffDone)
		close(s.done)
	}()

	dr, err := newDecoder(pr)
	if err != nil {
		// Not actually the encoding the header claimed, or the connection
		// died before the decoder's header ever completed — recorded as
		// zero, same as any other parse failure.
		return
	}
	defer dr.Close()

	buf := make([]byte, 32*1024)
	total := 0
	for {
		// A (0, nil) return is legal for an io.Reader, and brotli's does
		// it when the pipe hands it a zero-length write. Looping is the
		// correct response and cannot spin: every such iteration costs
		// exactly one pr.Read, which blocks until there is either another
		// write or a close.
		n, rerr := dr.Read(buf)
		if n > 0 {
			total += n
			if total > usageSniffMax {
				// Bounded on DECODED bytes, not compressed ones (issue
				// #121) — a compressed stream expands, so capping the
				// input would stop meaning anything. Whatever was already
				// fed into got/buf is kept; nothing further is decoded.
				return
			}
			s.feed(buf[:n])
		}
		if rerr != nil {
			// io.EOF (clean end), io.ErrUnexpectedEOF (cut off mid-stream),
			// or a corrupt trailer/CRC/bitstream: all just stop here and
			// keep whatever was already parsed — never treated as a hard
			// failure this sniffer needs to report.
			return
		}
	}
}

// feed runs the SSE/whole-body parser against bytes already known to be
// plaintext: either the response body itself (identity encoding, via
// Write) or a decoder's output (via drainDecoded). Exactly one of those two
// callers ever calls this for a given sniffer instance — see the ownership
// note on usageSniffer — so it needs no locking.
func (s *usageSniffer) feed(b []byte) {
	if s.sse {
		if s.lineGaveUp {
			return
		}
		s.line = append(s.line, b...)
		for {
			i := bytes.IndexByte(s.line, '\n')
			if i < 0 {
				if len(s.line) > usageSniffMax {
					// A single line longer than this has stopped looking
					// like SSE; give up on the rest of this response
					// rather than buffer forever.
					s.line = nil
					s.lineGaveUp = true
				}
				return
			}
			line := s.line[:i]
			s.line = s.line[i+1:]
			s.consumeSSELine(line)
		}
	}
	if s.over {
		return
	}
	if len(s.buf)+len(b) > usageSniffMax {
		s.over = true
		s.buf = nil
		return
	}
	s.buf = append(s.buf, b...)
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
//
// For a decoded path this first closes pw and waits for drainDecoded to
// finish: closing signals "no more input" (a normal completion, a
// disconnected client, and a cancelled request context all reach this the
// same way — see writeResponse — and Close never blocks), and the receive
// from done is what happens-before the reads of got/buf/over below, making
// them safe without a mutex despite drainDecoded having written them from
// a different goroutine.
func (s *usageSniffer) usage() usageTotals {
	if s.dec {
		_ = s.pw.Close()
		<-s.done
	}
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
