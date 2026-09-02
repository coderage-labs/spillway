package proxy

// Prefix fingerprints — issue #111, PHASE 1: measurement only.
//
// Issue #111 proposes rewriting requests so Claude Code's prompt cache stops
// busting (sorting tools, relocating attachment blocks, and four more
// transforms), on the strength of a third party's numbers. Nothing in this
// file rewrites anything. §4 still permits exactly four mutations and this
// adds no fifth: the functions here are pure, take a []byte and return
// hashes and counts, and are never wired into buildRequest. Phase 2 does not
// get built until this measurement says which instability is real in OUR
// traffic.
//
// The question the recorded row has to be able to answer is: when
// cache_creation_input_tokens (issue #110) spiked, which part of the request
// PREFIX had just changed? So per request we record short hashes of the
// prefix regions that the cache keys on, and nothing else:
//
//	tools_order_hash      ordered list of tool names (order-sensitive)
//	tools_sorted_hash     the same list, sorted (order-insensitive)
//	tools_raw_hash        the tools array's verbatim bytes
//	system_hash           the system block's verbatim bytes
//	first_msg_shape_hash  messages[0]'s content-block TYPE sequence
//	tool_count            how many tools
//	first_msg_blocks      how many content blocks in messages[0]
//	prefix_bytes          system + tools + messages[0], in bytes
//
// The ordered/sorted PAIR is the point of the exercise. Ordered changed
// while sorted did not means the tool set was identical and only its order
// jittered — the one instability #111's "deterministically sort tools" would
// fix. Nothing else can distinguish that from the tool set genuinely
// changing.
//
// # No content, ever
//
// Every value produced here is either an integer or a truncated SHA-256 in
// hex. No prompt text, no tool description, no tool INPUT, no file path from
// an attachment, no system prompt — not in the return value, not in a log
// line, not in an error. The only strings that are read at all are tool
// names and content-block type tags ("text", "image", …), and even those are
// hashed rather than stored. A reviewer can check this by reading
// prefixFingerprint's fields: there is nowhere for content to go.
//
// SHA-256 rather than fnv64: a fingerprint of a small, guessable region
// (a system block with a handful of plausible spellings) under a
// non-cryptographic hash invites the argument that the value is
// reconstructable by brute force. Truncated SHA-256 costs a few hundred
// microseconds on a 500 KB body and ends that argument.
//
// # Never a new failure mode
//
// This runs on the inference hot path, so it must be incapable of failing a
// request. Three separate guards:
//
//  1. It only ever reads the body the proxy has ALREADY buffered for
//     failover (§6.10) — it never re-reads, re-buffers or copies it. The
//     region hashes are taken over sub-slices; the only allocations are the
//     tool-name slice and a couple of hashers.
//  2. Every scan is bounds-checked and depth-capped, and anything
//     unexpected — malformed JSON, a truncated body, a key spelled with an
//     escape, a shape we did not anticipate — returns an EMPTY fingerprint
//     rather than a guess. Empty is a legitimate recorded value meaning
//     "not measurable", and the query side knows to skip it.
//  3. A recover() backstop, because "telemetry crashed a paid inference
//     request" is not a trade this package gets to make even if the scanner
//     is wrong.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync/atomic"
)

// maxJSONDepth caps container nesting during the scan. Recursion is not used
// anywhere here precisely because a deeply nested body would otherwise be a
// stack-overflow bomb, and a Go stack overflow is a process abort that no
// recover() can catch. 512 is far past anything Claude Code sends (a tool's
// input_schema nests a handful of levels) and far below anything dangerous.
const maxJSONDepth = 512

// maxToolsScanned bounds the tool-name list. The body is already bounded by
// bodyCap, so this only exists to keep a pathological array of 100k empty
// objects from producing a pointlessly large slice on the hot path.
const maxToolsScanned = 4096

// prefixFingerprintPanics counts how many times the recover() backstop in
// fingerprintPrefix has actually fired. It exists so the hostile-body tests
// can assert that the scanner did not merely SURVIVE a nasty body but never
// panicked on it in the first place — without it, a scanner riddled with
// unchecked indexes would pass those tests silently, and the backstop would
// be hiding exactly the bug it is there to contain.
var prefixFingerprintPanics atomic.Int64

// prefixFingerprint is the structural summary of one request's cacheable
// prefix. Hashes are hex, or "" when that region was absent or not
// measurable — never a hash of nothing, because "absent" and "empty" answer
// different questions on the query side.
type prefixFingerprint struct {
	ToolCount         int
	ToolsOrderHash    string
	ToolsSortedHash   string
	ToolsRawHash      string
	SystemHash        string
	FirstMsgShapeHash string
	FirstMsgBlocks    int
	PrefixBytes       int64
}

// fingerprintPrefix computes the fingerprint of an already-buffered
// POST /v1/messages body. body is READ ONLY and is never retained: the
// returned struct holds no reference to it.
//
// Any failure — malformed JSON, truncation, an unexpected shape, a panic in
// the scanner — yields the zero fingerprint. It cannot return an error
// because there is no caller that would be allowed to act on one.
func fingerprintPrefix(body []byte) (fp prefixFingerprint) {
	defer func() {
		// Backstop only; the scanners below are bounds-checked and
		// depth-capped and no test has ever reached this. It exists so that
		// a bug here can never be the reason a user's request fails. The
		// recovered value is deliberately not logged: it could carry body
		// bytes into a log line, which is exactly what this package must
		// never do.
		if recover() != nil {
			prefixFingerprintPanics.Add(1)
			fp = prefixFingerprint{}
		}
	}()
	if len(body) == 0 {
		return prefixFingerprint{}
	}

	// One walk of the root object for all three regions. Three separate
	// objectMember calls would each re-scan the whole body, and on a 500 KB
	// request that is two entirely wasted passes on the hot path.
	spans := topLevelMembers(body, "system", "tools", "messages")
	systemStart, systemEnd := spans[0][0], spans[0][1]
	toolsStart, toolsEnd := spans[1][0], spans[1][1]
	msgsStart, msgsEnd := spans[2][0], spans[2][1]

	if systemStart >= 0 {
		fp.SystemHash = shortHash(body[systemStart:systemEnd])
		fp.PrefixBytes += int64(systemEnd - systemStart)
	}

	if toolsStart >= 0 {
		tools := body[toolsStart:toolsEnd]
		fp.ToolsRawHash = shortHash(tools)
		fp.PrefixBytes += int64(len(tools))

		if names, ok := toolNames(tools); ok {
			fp.ToolCount = len(names)
			fp.ToolsOrderHash = shortHashList(names)
			// Sort a COPY. Sorting names in place would be harmless today
			// (it is our own slice) but would make the ordered hash depend
			// on evaluation order, which is the exact class of bug this
			// measurement exists to detect.
			sorted := make([]string, len(names))
			copy(sorted, names)
			sort.Strings(sorted)
			fp.ToolsSortedHash = shortHashList(sorted)
		}
	}

	if msgsStart >= 0 {
		if fs, fe := firstArrayElement(body[msgsStart:msgsEnd]); fs >= 0 {
			first := body[msgsStart+fs : msgsStart+fe]
			fp.PrefixBytes += int64(len(first))
			if shape, n, ok := contentShape(first); ok {
				fp.FirstMsgShapeHash = shortHash([]byte(shape))
				fp.FirstMsgBlocks = n
			}
		}
	}
	return fp
}

// toolNames returns the "name" of each element of the tools array, in array
// order, never sorted and never built from a map — Go's map iteration order
// is randomised per range, so a name list assembled from a map would produce
// a different "ordered" hash on every identical request and make this whole
// measurement lie in the exact direction #111 already believes. ok is false
// if the array is not an array, is too long, or any element is not an object
// with a decodable string name.
func toolNames(tools []byte) (names []string, ok bool) {
	i := skipWS(tools, 0)
	if i >= len(tools) || tools[i] != '[' {
		return nil, false
	}
	i++
	for {
		i = skipWS(tools, i)
		if i >= len(tools) {
			return nil, false // truncated before the closing bracket
		}
		if tools[i] == ']' {
			return names, true
		}
		if tools[i] == ',' {
			i++
			continue
		}
		start := i
		end := skipValue(tools, i)
		if end < 0 {
			return nil, false
		}
		i = end
		if len(names) >= maxToolsScanned {
			return nil, false
		}
		ns, ne := objectMember(tools[start:end], "name")
		if ns < 0 {
			return nil, false
		}
		var name string
		// The name span is a short JSON string; encoding/json is what makes
		// escapes ("Bash") decode to the same value the upstream sees,
		// which a literal byte comparison would get wrong.
		if err := json.Unmarshal(tools[start+ns:start+ne], &name); err != nil {
			return nil, false
		}
		names = append(names, name)
	}
}

// contentShape summarises messages[0]: the comma-joined sequence of its
// content blocks' "type" tags (e.g. "text,image,text") and how many there
// are. This is #111's "block scatter" instability made observable — if
// attachment blocks migrate between turns, this sequence changes while the
// tools and system hashes hold still.
//
// A string content ("content": "hi") is reported as the single shape token
// "string": semantically it is one text block, but it is a different WIRE
// shape and conflating the two would hide a real prefix change.
func contentShape(msg []byte) (shape string, blocks int, ok bool) {
	cs, ce := objectMember(msg, "content")
	if cs < 0 {
		return "", 0, false
	}
	content := msg[cs:ce]
	i := skipWS(content, 0)
	if i >= len(content) {
		return "", 0, false
	}
	if content[i] == '"' {
		return "string", 1, true
	}
	if content[i] != '[' {
		return "", 0, false
	}
	i++
	// Types only — never the block's text, source, or tool input.
	types := make([]byte, 0, 64)
	for {
		i = skipWS(content, i)
		if i >= len(content) {
			return "", 0, false
		}
		if content[i] == ']' {
			return string(types), blocks, true
		}
		if content[i] == ',' {
			i++
			continue
		}
		start := i
		end := skipValue(content, i)
		if end < 0 {
			return "", 0, false
		}
		i = end
		if blocks >= maxToolsScanned {
			return "", 0, false
		}
		tag := "?"
		if ts, te := objectMember(content[start:end], "type"); ts >= 0 {
			var s string
			if json.Unmarshal(content[start+ts:start+te], &s) == nil {
				tag = s
			}
		}
		if blocks > 0 {
			types = append(types, ',')
		}
		types = append(types, tag...)
		blocks++
	}
}

// shortHash is SHA-256 truncated to 8 bytes, hex. Truncation is fine for a
// change detector: this answers "are these the same bytes", never "what were
// the bytes".
func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// shortHashList hashes an ordered list of strings, NUL-separated so that
// ["ab","c"] and ["a","bc"] cannot collide.
func shortHashList(items []string) string {
	h := sha256.New()
	for _, s := range items {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return hex.EncodeToString(sum[:8])
}

// --- JSON span scanning -----------------------------------------------
//
// These locate the VERBATIM byte spans of the regions above without
// unmarshalling the body: a json.RawMessage decode of a 500 KB tools array
// copies 500 KB per request, and encoding/json would parse every tool's
// entire input_schema to reach a field we do not want. They are permissive
// locators, not validators — anything they actually decode (a tool name, a
// block type) goes through encoding/json, and anything they cannot make
// sense of yields "" rather than a guess.

func skipWS(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// skipString expects b[i] == '"' and returns the index just past the closing
// quote, or -1 if the string never terminates.
func skipString(b []byte, i int) int {
	i++ // opening quote
	for i < len(b) {
		switch b[i] {
		case '\\':
			i += 2
		case '"':
			return i + 1
		default:
			i++
		}
	}
	return -1
}

// skipValue returns the index just past the JSON value beginning at i (which
// must be the value's first byte, whitespace already skipped), or -1 if the
// value is malformed, truncated, or nested deeper than maxJSONDepth.
func skipValue(b []byte, i int) int {
	depth := 0
	for i < len(b) {
		switch c := b[i]; c {
		case ' ', '\t', '\r', '\n', ',', ':':
			i++
		case '{', '[':
			depth++
			if depth > maxJSONDepth {
				return -1
			}
			i++
		case '}', ']':
			depth--
			if depth < 0 {
				return -1
			}
			i++
			if depth == 0 {
				return i
			}
		case '"':
			j := skipString(b, i)
			if j < 0 {
				return -1
			}
			i = j
			if depth == 0 {
				return i
			}
		default:
			// A literal: number, true, false, null. Consumed up to the next
			// structural byte; encoding/json validates the few of these we
			// actually decode, and we never decode a number.
			j := i
			for j < len(b) && !isStructural(b[j]) {
				j++
			}
			if j == i {
				return -1
			}
			i = j
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func isStructural(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', ',', ':', '{', '}', '[', ']', '"':
		return true
	}
	return false
}

// objectMember returns the byte span [start,end) of the value of key in the
// JSON object at the start of b, or (-1,-1) if b is not an object, is
// truncated, or has no such key.
//
// Duplicate keys resolve to the LAST occurrence, matching what encoding/json
// (and therefore Anthropic's own decoder) keeps. A key spelled with an escape
// is simply not matched — the fingerprint for that region comes out empty,
// which is the honest answer, and unlike model.go's rewrite path there is
// nothing here that could go upstream wrong as a result.
func objectMember(b []byte, key string) (int, int) {
	spans := topLevelMembers(b, key)
	return spans[0][0], spans[0][1]
}

// topLevelMembers is objectMember for several keys in a single walk, which
// is what the hot path uses: one pass over a 500 KB body instead of one per
// region. Returns a span per key in the order given, (-1,-1) for any key not
// found (or for a body that is not a well-formed object).
func topLevelMembers(b []byte, keys ...string) [][2]int {
	found := make([][2]int, len(keys))
	for i := range found {
		found[i] = [2]int{-1, -1}
	}
	i := skipWS(b, 0)
	if i >= len(b) || b[i] != '{' {
		return found
	}
	i++
	for {
		i = skipWS(b, i)
		if i >= len(b) {
			return blankSpans(len(keys)) // truncated before the object closed
		}
		switch b[i] {
		case '}':
			return found
		case ',':
			i++
			continue
		case '"':
		default:
			return blankSpans(len(keys))
		}
		keyStart := i
		keyEnd := skipString(b, i)
		if keyEnd < 0 {
			return blankSpans(len(keys))
		}
		i = skipWS(b, keyEnd)
		if i >= len(b) || b[i] != ':' {
			return blankSpans(len(keys))
		}
		i = skipWS(b, i+1)
		if i >= len(b) {
			return blankSpans(len(keys))
		}
		valStart := i
		valEnd := skipValue(b, i)
		if valEnd < 0 {
			return blankSpans(len(keys))
		}
		i = valEnd
		// keyStart+1 / keyEnd-1 strips the quotes; an escaped spelling
		// deliberately fails this comparison (see the doc comment).
		name := b[keyStart+1 : keyEnd-1]
		for k, key := range keys {
			if string(name) == key {
				found[k] = [2]int{valStart, valEnd}
			}
		}
	}
}

// blankSpans is the "nothing usable here" answer: a body that stops making
// sense partway through yields no spans at all rather than the ones scanned
// before the trouble, because a truncated object's earlier members are not
// evidence that the request the client sent looked like that.
func blankSpans(n int) [][2]int {
	out := make([][2]int, n)
	for i := range out {
		out[i] = [2]int{-1, -1}
	}
	return out
}

// firstArrayElement returns the span of element 0 of the JSON array at the
// start of b, or (-1,-1) for a non-array, an empty array, or a truncated one.
func firstArrayElement(b []byte) (int, int) {
	i := skipWS(b, 0)
	if i >= len(b) || b[i] != '[' {
		return -1, -1
	}
	i = skipWS(b, i+1)
	if i >= len(b) || b[i] == ']' {
		return -1, -1
	}
	end := skipValue(b, i)
	if end < 0 {
		return -1, -1
	}
	return i, end
}
