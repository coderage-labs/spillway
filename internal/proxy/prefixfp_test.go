package proxy

// Tests for issue #111 phase 1's prefix fingerprints.
//
// The claim these have to actually prove, not merely exercise:
//
//   - a reordered but otherwise identical tool set yields the SAME sorted
//     hash and a DIFFERENT ordered hash — the pair is the whole measurement,
//     and if it cannot separate those two cases the report is worthless;
//   - identical bodies yield identical hashes, run after run, with no
//     map-iteration nondeterminism anywhere in the derivation;
//   - a malformed, truncated, adversarial or enormous body produces an empty
//     fingerprint and never panics, because this runs on the paid inference
//     path;
//   - nothing but hex and integers ever comes out.

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// fpTool renders one tool with a description big enough that the tools array
// dominates the body, as it does in real Claude Code traffic.
func fpTool(name, desc string) string {
	return fmt.Sprintf(`{"name":%q,"description":%q,"input_schema":{"type":"object","properties":{"model":{"type":"string"},"command":{"type":"string"}},"required":["command"]}}`,
		name, desc)
}

// fpBody builds a /v1/messages body with the given tool names, system text
// and first-message content blocks.
func fpBody(toolNames []string, systemText string, blocks string) string {
	tools := make([]string, 0, len(toolNames))
	for _, n := range toolNames {
		tools = append(tools, fpTool(n, "Run the "+n+" tool. "+strings.Repeat("guidance. ", 20)))
	}
	return fmt.Sprintf(`{"model":"claude-sonnet-4-6","max_tokens":8192,
"system":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"}}],
"messages":[{"role":"user","content":[%s]},{"role":"assistant","content":[{"type":"text","text":"ok"}]}],
"tools":[%s],
"metadata":{"user_id":"user_abc__session_def"},"stream":true}`,
		systemText, blocks, strings.Join(tools, ","))
}

const fpTextBlock = `{"type":"text","text":"list the files here"}`
const fpImageBlock = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}`

// The central claim. Same tools, different order: the sorted hash must hold
// still (so the report can say "the set did not change") while the ordered
// hash must move (so it can say "the order did"). Without both halves,
// nothing distinguishes tool-ordering jitter — the one thing #111's proposed
// sorting would fix — from the tool set genuinely changing.
func TestFingerprintSeparatesToolOrderFromToolSet(t *testing.T) {
	a := fingerprintPrefix([]byte(fpBody([]string{"Bash", "Read", "Edit"}, "sys", fpTextBlock)))
	reordered := fingerprintPrefix([]byte(fpBody([]string{"Read", "Edit", "Bash"}, "sys", fpTextBlock)))
	differentSet := fingerprintPrefix([]byte(fpBody([]string{"Bash", "Read", "Glob"}, "sys", fpTextBlock)))

	if a.ToolsSortedHash == "" || a.ToolsOrderHash == "" {
		t.Fatalf("baseline fingerprint is empty: %+v", a)
	}
	if reordered.ToolsSortedHash != a.ToolsSortedHash {
		t.Errorf("sorted hash changed under pure reordering: %q -> %q — the report cannot tell ordering jitter from a changed tool set",
			a.ToolsSortedHash, reordered.ToolsSortedHash)
	}
	if reordered.ToolsOrderHash == a.ToolsOrderHash {
		t.Errorf("ordered hash %q survived a reordering — ordering jitter would be invisible", a.ToolsOrderHash)
	}
	if differentSet.ToolsSortedHash == a.ToolsSortedHash {
		t.Errorf("sorted hash %q survived a genuinely different tool set — every change would look like mere ordering",
			a.ToolsSortedHash)
	}
	if a.ToolCount != 3 || reordered.ToolCount != 3 {
		t.Errorf("tool counts = %d, %d, want 3", a.ToolCount, reordered.ToolCount)
	}
	// A reordering moves the verbatim bytes too, so the raw hash must move.
	// If it did not, the raw hash would not be detecting byte changes at all.
	if reordered.ToolsRawHash == a.ToolsRawHash {
		t.Errorf("raw tools hash %q survived a reordering of the array's bytes", a.ToolsRawHash)
	}
}

// Determinism. Go randomises map iteration order per range, so a name list
// assembled from a map would produce a different "ordered" hash on every
// identical request and make the whole measurement report ordering jitter
// that does not exist. Many tools, many repeats: if anything in the
// derivation touched a map, this fails within a few iterations.
func TestFingerprintIsDeterministicAcrossRuns(t *testing.T) {
	names := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		names = append(names, fmt.Sprintf("Tool%02d", i))
	}
	body := []byte(fpBody(names, "you are claude code", fpTextBlock+","+fpImageBlock))

	want := fingerprintPrefix(body)
	if want.ToolsOrderHash == "" || want.ToolsSortedHash == "" || want.SystemHash == "" ||
		want.FirstMsgShapeHash == "" || want.ToolsRawHash == "" {
		t.Fatalf("baseline has an empty hash, nothing is being tested: %+v", want)
	}
	for i := 0; i < 500; i++ {
		// A fresh copy of the bytes each time: identical content, different
		// backing array, exactly as two separate requests would arrive.
		got := fingerprintPrefix(append([]byte(nil), body...))
		if got != want {
			t.Fatalf("iteration %d differs — the fingerprint is not deterministic\n got  %+v\n want %+v", i, got, want)
		}
	}
}

// Each region must be independently observable: the report's whole job is
// saying WHICH part of the prefix moved, so a change in one region must not
// smear across the others.
func TestFingerprintIsolatesRegions(t *testing.T) {
	base := fingerprintPrefix([]byte(fpBody([]string{"Bash", "Read"}, "sys A", fpTextBlock)))

	t.Run("system text", func(t *testing.T) {
		got := fingerprintPrefix([]byte(fpBody([]string{"Bash", "Read"}, "sys B", fpTextBlock)))
		if got.SystemHash == base.SystemHash {
			t.Errorf("system hash %q survived a changed system block", base.SystemHash)
		}
		if got.ToolsOrderHash != base.ToolsOrderHash || got.ToolsRawHash != base.ToolsRawHash ||
			got.FirstMsgShapeHash != base.FirstMsgShapeHash {
			t.Errorf("a system-only change moved another region:\n got  %+v\n base %+v", got, base)
		}
	})

	t.Run("tool description only", func(t *testing.T) {
		body := fpBody([]string{"Bash", "Read"}, "sys A", fpTextBlock)
		changed := strings.Replace(body, "Run the Bash tool.", "Run the Bash tool!!", 1)
		if changed == body {
			t.Fatal("test setup failed to alter the description")
		}
		got := fingerprintPrefix([]byte(changed))
		if got.ToolsRawHash == base.ToolsRawHash {
			t.Errorf("raw tools hash %q survived an edited description — description churn would be invisible",
				base.ToolsRawHash)
		}
		if got.ToolsOrderHash != base.ToolsOrderHash || got.ToolsSortedHash != base.ToolsSortedHash {
			t.Error("a description edit moved the tool-NAME hashes; they must track names only")
		}
		if got.SystemHash != base.SystemHash {
			t.Error("a description edit moved the system hash")
		}
	})

	t.Run("first message block shape", func(t *testing.T) {
		got := fingerprintPrefix([]byte(fpBody([]string{"Bash", "Read"}, "sys A", fpImageBlock+","+fpTextBlock)))
		if got.FirstMsgShapeHash == base.FirstMsgShapeHash {
			t.Errorf("shape hash %q survived blocks changing from text to image,text — block scatter would be invisible",
				base.FirstMsgShapeHash)
		}
		if got.FirstMsgBlocks != 2 {
			t.Errorf("FirstMsgBlocks = %d, want 2", got.FirstMsgBlocks)
		}
		if got.ToolsRawHash != base.ToolsRawHash || got.SystemHash != base.SystemHash {
			t.Error("a first-message change moved the tools or system hash")
		}
	})

	t.Run("block order within the first message", func(t *testing.T) {
		ab := fingerprintPrefix([]byte(fpBody([]string{"Bash"}, "s", fpTextBlock+","+fpImageBlock)))
		ba := fingerprintPrefix([]byte(fpBody([]string{"Bash"}, "s", fpImageBlock+","+fpTextBlock)))
		if ab.FirstMsgShapeHash == ba.FirstMsgShapeHash {
			t.Error("text,image and image,text hash the same — the shape hash is order-insensitive, so #111's block scatter is unmeasurable")
		}
	})

	t.Run("prefix bytes tracks prefix size", func(t *testing.T) {
		small := fingerprintPrefix([]byte(fpBody([]string{"Bash"}, "s", fpTextBlock)))
		big := fingerprintPrefix([]byte(fpBody([]string{"Bash", "Read", "Edit", "Glob"}, "s", fpTextBlock)))
		if small.PrefixBytes <= 0 {
			t.Fatalf("PrefixBytes = %d for a well-formed body", small.PrefixBytes)
		}
		if big.PrefixBytes <= small.PrefixBytes {
			t.Errorf("PrefixBytes did not grow with the prefix: %d -> %d", small.PrefixBytes, big.PrefixBytes)
		}
	})
}

// A body missing a region records an empty hash for it, not a hash of
// nothing: "absent" and "present but empty" answer different questions, and
// PrefixDrift skips a comparison it cannot make rather than inventing a
// change out of missing data.
func TestFingerprintAbsentRegionsAreEmptyNotHashed(t *testing.T) {
	got := fingerprintPrefix([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if got.ToolsOrderHash != "" || got.ToolsSortedHash != "" || got.ToolsRawHash != "" {
		t.Errorf("tool hashes populated for a body with no tools: %+v", got)
	}
	if got.SystemHash != "" {
		t.Errorf("system hash populated for a body with no system block: %q", got.SystemHash)
	}
	if got.ToolCount != 0 {
		t.Errorf("ToolCount = %d for a body with no tools", got.ToolCount)
	}
	// A string content is a real, different wire shape and must still be
	// reported — as one block.
	if got.FirstMsgShapeHash == "" || got.FirstMsgBlocks != 1 {
		t.Errorf("string content not reported: hash=%q blocks=%d", got.FirstMsgShapeHash, got.FirstMsgBlocks)
	}
	if arr := fingerprintPrefix([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)); arr.FirstMsgShapeHash == got.FirstMsgShapeHash {
		t.Error(`"content":"hi" and "content":[{"type":"text"...}] hash the same; they are different prefix bytes and must be distinguishable`)
	}
	// An empty tools array is present-but-empty: hashed, count zero.
	empty := fingerprintPrefix([]byte(`{"tools":[],"messages":[]}`))
	if empty.ToolsRawHash == "" || empty.ToolsOrderHash == "" {
		t.Errorf("an empty tools array is present and must hash: %+v", empty)
	}
	if empty.ToolCount != 0 {
		t.Errorf("ToolCount = %d for an empty tools array", empty.ToolCount)
	}
}

// The hard constraint: parsing must never break a request. Every input here
// goes through the same function the inference path calls, and the only
// acceptable outcomes are a fingerprint or an empty fingerprint — never a
// panic, never a hang.
func TestFingerprintSurvivesHostileBodies(t *testing.T) {
	// Surviving is not enough. fingerprintPrefix has a recover() backstop,
	// so a scanner full of unchecked indexes would pass every assertion
	// below while panicking on each input — and would then be one
	// unrecoverable stack overflow away from taking a request down for
	// real. The counter makes the difference between "did not panic" and
	// "panicked and was caught" visible.
	before := prefixFingerprintPanics.Load()
	defer func() {
		if n := prefixFingerprintPanics.Load() - before; n != 0 {
			t.Errorf("the scanner panicked %d times and was only saved by the recover() backstop", n)
		}
	}()

	good := fpBody([]string{"Bash", "Read", "Edit"}, "you are claude code", fpTextBlock+","+fpImageBlock)

	cases := map[string]string{
		"empty":                "",
		"not json":             "this is not json at all",
		"just a quote":         `"`,
		"unterminated string":  `{"system":"abc`,
		"unterminated object":  `{"tools":[{"name":"Bash"`,
		"unterminated array":   `{"tools":[`,
		"null body":            "null",
		"array root":           `["a","b"]`,
		"number root":          "12345",
		"nul bytes":            "{\x00\x00\x00}",
		"tools not an array":   `{"tools":{"name":"Bash"}}`,
		"tool not an object":   `{"tools":["Bash","Read"]}`,
		"tool without a name":  `{"tools":[{"description":"x"}]}`,
		"tool name not string": `{"tools":[{"name":42}]}`,
		"messages not array":   `{"messages":{"role":"user"}}`,
		"content not array":    `{"messages":[{"role":"user","content":42}]}`,
		"duplicate tools key":  `{"tools":[{"name":"A"}],"tools":[{"name":"B"}]}`,
		"minimal valid body":   `{"\\u0073ystem":"hidden","tools":[{"name":"Bash"}]}`,
		"deeply nested array":  strings.Repeat("[", 200000) + strings.Repeat("]", 200000),
		"deeply nested object": `{"tools":` + strings.Repeat(`{"a":`, 100000) + `1` + strings.Repeat("}", 100000) + `}`,
		"huge single string":   `{"system":"` + strings.Repeat("x", 4<<20) + `"}`,
		"huge tools array":     `{"tools":[` + strings.Repeat(fpTool("T", strings.Repeat("d", 200))+",", 5000) + fpTool("Z", "z") + `]}`,
		"trailing garbage":     good + `!!!!`,
		"bom prefixed":         "\xef\xbb\xbf" + good,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			// A panic here fails the test rather than the process only
			// because fingerprintPrefix recovers; either way the request
			// must survive, which is what is being asserted.
			got := fingerprintPrefix([]byte(body))
			assertNoContentLeak(t, got)
		})
	}

	// Truncation at every byte offset of a real body: the single most
	// effective way to find an unchecked index, and the shape a cut
	// connection actually produces.
	for i := 0; i <= len(good); i++ {
		got := fingerprintPrefix([]byte(good[:i]))
		assertNoContentLeak(t, got)
	}

	// Single-byte corruption at every offset. Slower than truncation but
	// finds the cases where the scanner walks off a valid-looking prefix.
	for i := 0; i < len(good); i++ {
		b := []byte(good)
		b[i] = '{'
		assertNoContentLeak(t, fingerprintPrefix(b))
		b[i] = '"'
		assertNoContentLeak(t, fingerprintPrefix(b))
		b[i] = 0
		assertNoContentLeak(t, fingerprintPrefix(b))
	}
}

var fpHexOnly = regexp.MustCompile(`^[0-9a-f]{16}$`)

// assertNoContentLeak is the redaction check, applied to every fingerprint
// any test in this file produces: each hash is either empty or exactly 16
// hex characters, so there is no shape in which prompt text, a tool
// description or an attachment path could be riding along.
func assertNoContentLeak(t *testing.T, fp prefixFingerprint) {
	t.Helper()
	for name, h := range map[string]string{
		"ToolsOrderHash":    fp.ToolsOrderHash,
		"ToolsSortedHash":   fp.ToolsSortedHash,
		"ToolsRawHash":      fp.ToolsRawHash,
		"SystemHash":        fp.SystemHash,
		"FirstMsgShapeHash": fp.FirstMsgShapeHash,
	} {
		if h != "" && !fpHexOnly.MatchString(h) {
			t.Errorf("%s = %q — not a hash; content must never reach a fingerprint field", name, h)
		}
	}
	if fp.ToolCount < 0 || fp.FirstMsgBlocks < 0 || fp.PrefixBytes < 0 {
		t.Errorf("negative count in %+v", fp)
	}
}

// Content must not be reconstructable from the output, and the cheapest way
// to keep that reviewable is that the output has no room for it. Distinctive
// strings from the body must appear nowhere in the rendered fingerprint.
func TestFingerprintEmitsNoContent(t *testing.T) {
	const secret = "SUPERSECRETPROMPTTEXT"
	body := fpBody([]string{"Bash_" + secret}, "system says "+secret,
		`{"type":"text","text":"`+secret+` /Users/someone/private/file.go"}`)
	got := fingerprintPrefix([]byte(body))
	assertNoContentLeak(t, got)
	rendered := fmt.Sprintf("%+v", got)
	for _, needle := range []string{secret, "/Users/someone", "system says", "Bash_", "guidance"} {
		if strings.Contains(rendered, needle) {
			t.Errorf("fingerprint %q contains %q from the request body", rendered, needle)
		}
	}
}

// benchBody is ~500 KB with 30 tools: the shape of a real Claude Code
// request with a full tool set and a couple of MCP servers attached.
func benchBody() []byte {
	names := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		names = append(names, fmt.Sprintf("Tool%02d", i))
	}
	tools := make([]string, 0, len(names))
	for _, n := range names {
		tools = append(tools, fpTool(n, strings.Repeat("Detailed tool guidance. ", 640)))
	}
	body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","max_tokens":8192,
"system":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"}}],
"messages":[{"role":"user","content":[%s,%s]}],
"tools":[%s],
"metadata":{"user_id":"user_abc__session_def"},"stream":true}`,
		strings.Repeat("You are Claude Code. ", 400), fpTextBlock, fpImageBlock, strings.Join(tools, ","))
	return []byte(body)
}

func BenchmarkFingerprintPrefix(b *testing.B) {
	body := benchBody()
	b.Logf("body = %d bytes", len(body))
	if fp := fingerprintPrefix(body); fp.ToolCount != 30 {
		b.Fatalf("benchmark body is not being parsed: %+v", fp)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = fingerprintPrefix(body)
	}
}
