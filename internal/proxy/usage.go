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
)

// usageSniffMax bounds how many bytes the sniffer will ever hold: a partial
// SSE line that never terminates, or a non-streaming body, each stop
// growing past this and give up quietly rather than buffer an unbounded
// amount of a very large response. It bounds memory ONLY for the sniffer's
// own bookkeeping — it never affects what reaches the client, which flows
// through io.Copy's ordinary small buffer regardless of response size (see
// writeResponse).
//
// For a gzip-encoded response (issue #121) this is the cap on DECODED
// bytes, not compressed ones — a compressed stream expands, so capping the
// compressed input would stop meaning anything. See drainGzip.
const usageSniffMax = 1 << 20 // 1 MiB

// unsupportedEncLogged records, process-lifetime, which Content-Encoding
// values have already produced the "unsupported" warning below — so a
// stream of responses in an encoding this sniffer can't read logs it once,
// not once per request. Issue #121 happened because a skip like this was
// silent; the fix is visibility, not necessarily decoding everything.
var unsupportedEncLogged sync.Map // map[string]struct{}

// logUnsupportedEncodingOnce warns exactly once per distinct
// Content-Encoding value that the usage sniffer is giving up on it and
// will record zero counters for every such response. Anthropic is only
// ever known to send gzip or identity today; this exists for br (issue
// #121 asks for it by name) and anything else that shows up later.
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

// errUsageSniffDone is the error drainGzip's read end closes with once it
// has stopped reading, for any reason (clean end, decode error, or the
// decoded-size cap). It is never inspected — only its presence matters: it
// makes any Write on the other end of the pipe that is blocked, or arrives
// later, fail immediately instead of hanging. See drainGzip and Write.
var errUsageSniffDone = errors.New("usage sniffer: done reading")

// usageSniffer observes response bytes as they stream past, without ever
// holding more than usageSniffMax of them, and without being able to fail
// the copy it's attached to: Write always reports success. It exists to be
// the second destination of an io.MultiWriter alongside the real client
// writer — see writeResponse — so parsing happens on the same bytes as they
// pass through, not on a buffered copy.
//
// Ownership (issue #121's gzip path adds a second goroutine, so this needs
// spelling out): when gz is false, every field below is touched only by
// whatever goroutine calls Write and then usage — normally the same
// goroutine, since writeResponse calls both in sequence. When gz is true,
// Write only ever forwards bytes into the pipe; line, lineGaveUp, got, buf
// and over are owned exclusively by drainGzip's goroutine until it closes
// done, and usage() will not read any of them before receiving from done —
// that receive is what makes the handoff race-free (see usage()).
type usageSniffer struct {
	// skip is set once, at construction, for an encoding this sniffer does
	// not know how to read (anything but gzip/identity/""). Immutable
	// after that — safe to read from Write without synchronization even
	// while a decode goroutine may be running for a *different* instance.
	skip bool
	sse  bool

	// gzip decoding (issue #121). Write forwards bytes into pw; drainGzip
	// reads the other end, decompresses, and feeds the plaintext through
	// exactly the same parsing (feed) that an unencoded body goes through.
	// done closes when drainGzip returns, however it returns — usage()
	// waits on it before reading anything drainGzip wrote.
	gz   bool
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
func newUsageSniffer(contentType, contentEncoding string, logger *slog.Logger) *usageSniffer {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	sse := strings.HasPrefix(ct, "text/event-stream")

	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		return &usageSniffer{sse: sse}
	case "gzip":
		s := &usageSniffer{sse: sse, gz: true, done: make(chan struct{})}
		pr, pw := io.Pipe()
		s.pw = pw
		go s.drainGzip(pr)
		return s
	default:
		// br, deflate, zstd, or anything else Anthropic might one day send:
		// not decoded. Recorded as zero, same as any other parse failure —
		// but logged, once, so an unsupported encoding is visible rather
		// than silently producing the exact symptom issue #121 reported.
		logUnsupportedEncodingOnce(logger, strings.ToLower(strings.TrimSpace(contentEncoding)))
		return &usageSniffer{skip: true}
	}
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
	case s.gz:
		// Hand off to drainGzip via the pipe. The error is never inspected:
		// once drainGzip has stopped reading — cleanly, on a decode error,
		// or past the decoded-size cap — it closes the read end (see
		// drainGzip), which makes this fail immediately rather than block.
		// Either way, this Write must never be the reason client bytes
		// stop moving, so the outcome here is always success.
		_, _ = s.pw.Write(b)
		return n, nil
	default:
		s.feed(b)
		return n, nil
	}
}

// drainGzip is the sole reader of pr and, for as long as it runs, the sole
// owner of every field usage() eventually reads (see usageSniffer's doc).
// It runs gzip-decompressed bytes through feed() exactly as an unencoded
// body would go through Write — same SSE/JSON parsing, same truncation
// handling, same usageSniffMax cap, now measured in decoded bytes.
//
// The deferred close is the actual fix for the failure mode issue #121
// calls out ("a naive io.Pipe plus goroutine can leak or block if the
// stream ends early or the client disconnects"): closing the read end,
// unconditionally, on every return path — clean EOF, a corrupt or
// truncated gzip member, or the cap being hit — guarantees that any Write
// on the other end of this pipe that is currently blocked, or arrives
// later, fails immediately instead of waiting for a Read that will never
// come. Nothing here can block forever: gzip.Reader.Read only ever blocks
// on pr.Read, and closing pw (see usage()) unblocks that immediately with
// io.EOF, so this goroutine cannot outlive the writeResponse call that
// created it.
func (s *usageSniffer) drainGzip(pr *io.PipeReader) {
	defer func() {
		_ = pr.CloseWithError(errUsageSniffDone)
		close(s.done)
	}()

	zr, err := gzip.NewReader(pr)
	if err != nil {
		// Not actually gzip, or the connection died before the header ever
		// completed — recorded as zero, same as any other parse failure.
		return
	}
	defer zr.Close()

	buf := make([]byte, 32*1024)
	total := 0
	for {
		n, rerr := zr.Read(buf)
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
			// io.EOF (clean end), io.ErrUnexpectedEOF (cut off mid-member),
			// or a corrupt trailer/CRC: all three just stop here and keep
			// whatever was already parsed — never treated as a hard
			// failure this sniffer needs to report.
			return
		}
	}
}

// feed runs the SSE/whole-body parser against bytes already known to be
// plaintext: either the response body itself (identity encoding, via
// Write) or gzip's decompressed output (via drainGzip). Exactly one of
// those two callers ever calls this for a given sniffer instance — see the
// ownership note on usageSniffer — so it needs no locking.
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
// For the gzip path this first closes pw and waits for drainGzip to finish:
// closing signals "no more input" (a normal completion, a disconnected
// client, and a cancelled request context all reach this the same way —
// see writeResponse — and Close never blocks), and the receive from done
// is what happens-before the reads of got/buf/over below, making them safe
// without a mutex despite drainGzip having written them from a different
// goroutine.
func (s *usageSniffer) usage() usageTotals {
	if s.gz {
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
