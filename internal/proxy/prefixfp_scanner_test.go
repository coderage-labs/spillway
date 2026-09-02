package proxy

// Scanner semantics a reviewer would otherwise have to take on trust: what
// the span locator in prefixfp.go does with the JSON shapes that are legal
// but unusual. None of these can affect the request — nothing here rewrites
// anything — so the standard is "records the honest answer", which is
// sometimes an empty fingerprint.

import (
	"strings"
	"testing"
)

// backslash is built from its code point so the literal below cannot be
// mistaken for a Go escape sequence while still producing a JSON one.
var backslash = string(rune(92))

func TestFingerprintScannerSemantics(t *testing.T) {
	t.Run("duplicate key resolves to the last, as encoding/json does", func(t *testing.T) {
		first := fingerprintPrefix([]byte(`{"tools":[{"name":"A"}]}`))
		last := fingerprintPrefix([]byte(`{"tools":[{"name":"B"}]}`))
		dup := fingerprintPrefix([]byte(`{"tools":[{"name":"A"}],"tools":[{"name":"B"}]}`))
		if dup.ToolsOrderHash != last.ToolsOrderHash {
			t.Errorf("duplicate tools key resolved to %q, want the LAST occurrence %q — the one the upstream decoder keeps",
				dup.ToolsOrderHash, last.ToolsOrderHash)
		}
		if dup.ToolsOrderHash == first.ToolsOrderHash {
			t.Error("duplicate tools key resolved to the first occurrence")
		}
	})

	t.Run("an escaped key spelling records nothing rather than guessing", func(t *testing.T) {
		// "system" decodes to "system", but the scanner compares raw
		// bytes and will not match it. Unlike model.go's rewrite path there
		// is nothing that could go upstream wrong as a result, so the honest
		// answer is an empty fingerprint for that region — and PrefixDrift
		// then skips the comparison instead of inventing a change out of it.
		esc := `{"` + backslash + `u0073ystem":"hidden","tools":[{"name":"Bash"}]}`
		got := fingerprintPrefix([]byte(esc))
		if got.SystemHash != "" {
			t.Errorf("SystemHash = %q for an escaped key spelling, want empty", got.SystemHash)
		}
		if got.ToolsOrderHash == "" {
			t.Error("an unmatched key must not poison the regions that DID parse")
		}
	})

	t.Run("an escaped tool name decodes to the value the upstream sees", func(t *testing.T) {
		plain := fingerprintPrefix([]byte(`{"tools":[{"name":"Bash"}]}`))
		escaped := fingerprintPrefix([]byte(`{"tools":[{"name":"` + backslash + `u0042ash"}]}`))
		if escaped.ToolsOrderHash != plain.ToolsOrderHash {
			t.Errorf("escaped name hashed as %q, plain as %q — the same tool must fingerprint the same, or an escape change would read as a tool-set change",
				escaped.ToolsOrderHash, plain.ToolsOrderHash)
		}
	})

	t.Run("a nested key named model or name is not mistaken for the real one", func(t *testing.T) {
		// input_schema.properties.name sits two object levels below the
		// tools[] element; only the element's own "name" is the tool name.
		a := fingerprintPrefix([]byte(`{"tools":[{"name":"Bash","input_schema":{"properties":{"name":{"type":"string"}}}}]}`))
		b := fingerprintPrefix([]byte(`{"tools":[{"name":"Bash","input_schema":{"properties":{"name":{"type":"integer"}}}}]}`))
		if a.ToolsOrderHash != b.ToolsOrderHash {
			t.Error("a nested property named \"name\" leaked into the tool-name list")
		}
		if a.ToolsRawHash == b.ToolsRawHash {
			t.Error("the raw hash must still see a schema edit even though the name list does not")
		}
	})

	t.Run("nesting beyond the cap is refused, not recursed", func(t *testing.T) {
		// A recursive descent parser would blow the goroutine stack here,
		// and a Go stack overflow is a process abort no recover() catches.
		deep := `{"tools":[` + strings.Repeat(`{"a":[`, maxJSONDepth+10) +
			strings.Repeat(`]}`, maxJSONDepth+10) + `]}`
		got := fingerprintPrefix([]byte(deep))
		if got.ToolsRawHash != "" {
			t.Errorf("a body nested past maxJSONDepth produced %+v, want an empty fingerprint", got)
		}
	})

	t.Run("whitespace between tokens does not change the reading", func(t *testing.T) {
		tight := fingerprintPrefix([]byte(`{"tools":[{"name":"Bash"},{"name":"Read"}]}`))
		loose := fingerprintPrefix([]byte("{ \"tools\" : [ { \"name\" : \"Bash\" } ,\n\t{ \"name\" : \"Read\" } ] }"))
		if tight.ToolsOrderHash != loose.ToolsOrderHash {
			t.Error("reformatting changed the tool-name hash; the name list must track names, not layout")
		}
		if tight.ToolsRawHash == loose.ToolsRawHash {
			t.Error("the raw hash must notice reformatting — the cache keys on bytes, and whitespace is bytes")
		}
	})
}
