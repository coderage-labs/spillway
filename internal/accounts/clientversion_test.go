package accounts

import (
	"strings"
	"testing"
)

func TestCheckClientVersionRange(t *testing.T) {
	for _, tc := range []struct {
		v        string
		warnWord string // "" = no warning expected
	}{
		{"2.1.0", ""},
		{"2.0.0", ""},
		// Patch releases ship weekly and must not warn, or the warning gets
		// ignored — 2.1.239 is what this was actually exercised against.
		{"2.1.239", ""},
		{"2.0.17", ""},
		{"1.9.0", "older"},
		{"3.0.0", "newer"},
		{"2.2.0", "newer"},
		// Pre-release and v-prefix are the same version.
		{"v2.1.0-beta.3", ""},
		// An unparseable version says nothing rather than crying wolf.
		{"nightly", ""},
		{"", ""},
	} {
		got := CheckClientVersion(tc.v)
		if tc.warnWord == "" {
			if got != "" {
				t.Errorf("CheckClientVersion(%q) warned unexpectedly: %s", tc.v, got)
			}
			continue
		}
		if !strings.Contains(got, tc.warnWord) {
			t.Errorf("CheckClientVersion(%q) = %q, want a %q warning", tc.v, got, tc.warnWord)
		}
	}
}

// The warning must say what to do, not just that something is off.
func TestCheckClientVersionWarningIsActionable(t *testing.T) {
	w := CheckClientVersion("9.9.9")
	for _, want := range []string{TestedMin, TestedMax, "request shape"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q should mention %q", w, want)
		}
	}
}
