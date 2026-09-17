package main

// Issue #183's width measurement. The line is trimmed rather than wrapped,
// and a wrap costs a terminal row on every render — so the measurement has
// to be columns. Two ways to get it wrong, both of which wrap the very line
// the trimming exists to save:
//
//   - counting runes, which makes every emoji one column instead of two;
//   - counting bytes of a painted string, which charges for SGR escapes
//     that occupy no columns at all.

import (
	"strings"
	"testing"
	"time"
)

// The exact glyphs #183 assigns, each with the number of columns a terminal
// gives it. Written out one by one rather than derived, because the whole
// point is that the answer is not "one per rune".
func TestDisplayWidthOfEveryStatuslineGlyph(t *testing.T) {
	for glyph, want := range map[string]int{
		"📁": 2, // cwd
		"🌿": 2, // branch
		"🤖": 2, // the model the CLI selected
		"🧠": 2, // context window
		"📊": 2, // session tokens
		"🎯": 2, // the model spillway served
		"⏳": 2, // the 5h window
		"📆": 2, // the 7d window
		"💀": 2, // spent
		"⛁": 1, // the pool marker, unchanged since before #183
		"✈": 1, // in flight
		"█": 1, // bar, filled
		"░": 1, // bar, empty
		"↻": 1, // reset countdown
		"·": 1,
		"⚠": 1,
		"£": 1,
		"—": 1, // the unreachable marker's dash
		"…": 1, // the truncation mark
	} {
		if got := displayWidth(glyph); got != want {
			t.Errorf("displayWidth(%q) = %d, want %d", glyph, got, want)
		}
	}
}

// The measurement that matters: a whole row, and proof that a rune count
// would have given a different — smaller, therefore wrapping — answer.
func TestDisplayWidthOfARowIsColumnsNotRunes(t *testing.T) {
	row := "📁 ~/Repos/spillway  🌿 main  🤖 Opus 5 · high  🧠 34% ctx  📊 128k (2.1k tpm)"
	const want = 78 // counted by hand: five two-column glyphs, the rest ASCII
	if got := displayWidth(row); got != want {
		t.Errorf("displayWidth(row) = %d, want %d", got, want)
	}
	if runes := len([]rune(row)); runes >= want {
		t.Errorf("rune count %d is not smaller than the column count %d — this test "+
			"is no longer distinguishing the two, so it cannot catch the bug it exists for", runes, want)
	}
}

// Colour must be free. A painted line measures exactly as wide as the same
// line in NO_COLOR, or every coloured render trims segments that fitted.
func TestDisplayWidthIgnoresANSIEscapes(t *testing.T) {
	plain := render(noColour, []slAccount{okAccount()}, slState{Total: 1, Usable: 1}, time.Now())
	painted := render(truecolour, []slAccount{okAccount()}, slState{Total: 1, Usable: 1}, time.Now())
	if !strings.Contains(painted, "\033") {
		t.Fatal("the truecolour render emitted no escapes — this test is not measuring a painted string")
	}
	if got, want := displayWidth(painted), displayWidth(plain); got != want {
		t.Errorf("painted line measures %d columns, plain measures %d — escapes are being counted", got, want)
	}
	// And the same for the 256-colour and dim-only forms.
	if got, want := displayWidth(render(ansi256, []slAccount{okAccount()}, slState{Total: 1, Usable: 1}, time.Now())),
		displayWidth(plain); got != want {
		t.Errorf("ansi256 line measures %d columns, plain measures %d", got, want)
	}
}

func TestTruncateToWidthNeverExceedsAndClosesColour(t *testing.T) {
	painted := "\033[2m📁 \033[0m~/Repos/spillway  \033[2m🌿 \033[0mmain"
	if w := displayWidth(painted); w != 28 {
		t.Fatalf("the fixture measures %d columns, not the 28 these cases assume", w)
	}
	for _, max := range []int{5, 10, 21, 27} {
		got := truncateToWidth(painted, max)
		if w := displayWidth(got); w > max {
			t.Errorf("truncateToWidth(.., %d) produced %d columns: %q", max, w, got)
		}
		if !strings.HasSuffix(got, "\033[0m") {
			t.Errorf("truncateToWidth(.., %d) left the terminal painted: %q", max, got)
		}
		if !strings.Contains(got, "…") {
			t.Errorf("truncateToWidth(.., %d) did not mark the cut: %q", max, got)
		}
	}
	// A string that already fits is returned untouched — no stray ellipsis.
	if got := truncateToWidth("main", 10); got != "main" {
		t.Errorf("truncateToWidth of a fitting string = %q, want it unchanged", got)
	}
	// An unknown width (0) means no trimming at all.
	if got := truncateToWidth(painted, 0); got != painted {
		t.Errorf("truncateToWidth(.., 0) trimmed to an unknown width: %q", got)
	}
}

// Unknown width must mean "do not trim". Guessing 80 would permanently strip
// segments on a wide terminal, and the segments are the feature.
func TestTerminalColumnsIsZeroWhenUnknown(t *testing.T) {
	t.Setenv("COLUMNS", "")
	t.Setenv("SPILLWAY_STATUSLINE_COLUMNS", "")
	if got := terminalColumns(); got != 0 {
		t.Errorf("terminalColumns with nothing set = %d, want 0 (unknown)", got)
	}
	t.Setenv("COLUMNS", "132")
	if got := terminalColumns(); got != 132 {
		t.Errorf("terminalColumns with COLUMNS=132 = %d, want 132", got)
	}
	// The override wins: the status line command is not run from an
	// interactive shell, so COLUMNS may be absent or stale.
	t.Setenv("SPILLWAY_STATUSLINE_COLUMNS", "96")
	if got := terminalColumns(); got != 96 {
		t.Errorf("terminalColumns with the override set = %d, want 96", got)
	}
	// Nonsense and absurdly narrow values are ignored rather than obeyed.
	t.Setenv("SPILLWAY_STATUSLINE_COLUMNS", "not-a-number")
	t.Setenv("COLUMNS", "3")
	if got := terminalColumns(); got != 0 {
		t.Errorf("terminalColumns with junk = %d, want 0", got)
	}
}
