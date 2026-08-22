package proxy

import "testing"

// §6.12: a glob covers a model family without listing every version.
func TestLookupModelGlobFallback(t *testing.T) {
	m := map[string]string{
		"claude-haiku-4-5-20251001": "kimi-for-coding", // exact
		"claude-*":                  "k3",              // family
	}
	for in, want := range map[string]string{
		"claude-haiku-4-5-20251001": "kimi-for-coding", // exact beats glob
		"claude-sonnet-4-6":         "k3",
		"claude-opus-5":             "k3",
	} {
		got, ok := lookupModel(m, in)
		if !ok || got != want {
			t.Errorf("lookupModel(%q) = %q,%v; want %q", in, got, ok, want)
		}
	}
	if _, ok := lookupModel(m, "gpt-5"); ok {
		t.Error("a model matching nothing must stay unmapped, not fall through")
	}
}

// The longest matching pattern wins, so adding a broad catch-all cannot
// silently capture ids a specific pattern already handled.
func TestLookupModelPrefersTheMoreSpecificGlob(t *testing.T) {
	m := map[string]string{
		"claude-*":       "k3",
		"claude-haiku-*": "kimi-for-coding",
	}
	if got, _ := lookupModel(m, "claude-haiku-4-5"); got != "kimi-for-coding" {
		t.Errorf("got %q, want the more specific mapping", got)
	}
	if got, _ := lookupModel(m, "claude-opus-5"); got != "k3" {
		t.Errorf("got %q, want the catch-all", got)
	}
}
