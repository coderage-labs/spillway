package main

// Display width for the status line.
//
// The line is trimmed to fit the terminal rather than wrapped, and a wrap
// costs a terminal row on EVERY render — so the measurement has to be the
// number of columns the terminal will actually use, not the number of runes
// in the string. Two things break a rune count here:
//
//   - The line is full of SGR escapes (colour, dim). Those occupy no columns
//     at all, so counting them makes every coloured line look far wider than
//     it is and trims segments that would have fitted.
//   - Emoji are two columns wide. Issue #183 puts nine of them on the line;
//     counting each as one under-measures by nine columns, which is exactly
//     the wrap the width-awareness exists to prevent.
//
// The width table is an approximation of Unicode's East Asian Width: the
// genuinely wide ranges (CJK, Hangul) plus the emoji blocks. It errs wide
// inside the emoji planes — a handful of text-presentation glyphs in
// U+1F300..U+1F5FF are narrow — because over-measuring trims one segment too
// early while under-measuring wraps, and only one of those is recoverable.

import (
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// wideRanges are the code point ranges rendered two columns wide. Sorted by
// low bound; isWideRune relies on that to stop early.
var wideRanges = [][2]rune{
	{0x1100, 0x115F},   // Hangul Jamo
	{0x231A, 0x231B},   // ⌚⌛
	{0x2329, 0x232A},   // 〈〉
	{0x23E9, 0x23EC},   // ⏩⏪⏫⏬
	{0x23F0, 0x23F0},   // ⏰
	{0x23F3, 0x23F3},   // ⏳ — the 5h window glyph
	{0x25FD, 0x25FE},   //
	{0x2614, 0x2615},   //
	{0x2648, 0x2653},   // zodiac
	{0x267F, 0x267F},   //
	{0x2693, 0x2693},   //
	{0x26A1, 0x26A1},   //
	{0x26AA, 0x26AB},   //
	{0x26BD, 0x26BE},   //
	{0x26C4, 0x26C5},   //
	{0x26CE, 0x26CE},   //
	{0x26D4, 0x26D4},   //
	{0x26EA, 0x26EA},   //
	{0x26F2, 0x26F3},   //
	{0x26F5, 0x26F5},   //
	{0x26FA, 0x26FA},   //
	{0x26FD, 0x26FD},   //
	{0x2705, 0x2705},   //
	{0x270A, 0x270B},   //
	{0x2728, 0x2728},   //
	{0x274C, 0x274C},   //
	{0x274E, 0x274E},   //
	{0x2753, 0x2755},   //
	{0x2757, 0x2757},   //
	{0x2795, 0x2797},   //
	{0x27B0, 0x27B0},   //
	{0x27BF, 0x27BF},   //
	{0x2B1B, 0x2B1C},   //
	{0x2B50, 0x2B50},   //
	{0x2B55, 0x2B55},   //
	{0x2E80, 0x303E},   // CJK radicals .. CJK symbols
	{0x3041, 0x33FF},   // kana, Hangul compat, CJK compat
	{0x3400, 0x4DBF},   // CJK ext A
	{0x4E00, 0x9FFF},   // CJK unified
	{0xA000, 0xA4CF},   // Yi
	{0xA960, 0xA97F},   // Hangul Jamo ext A
	{0xAC00, 0xD7A3},   // Hangul syllables
	{0xF900, 0xFAFF},   // CJK compat ideographs
	{0xFE10, 0xFE19},   // vertical forms
	{0xFE30, 0xFE6F},   // CJK compat forms
	{0xFF00, 0xFF60},   // fullwidth forms
	{0xFFE0, 0xFFE6},   // fullwidth signs
	{0x16FE0, 0x16FE4}, //
	{0x17000, 0x18AFF}, // Tangut
	{0x1B000, 0x1B12F}, //
	{0x1B150, 0x1B16F}, //
	{0x1F004, 0x1F004}, //
	{0x1F0CF, 0x1F0CF}, //
	{0x1F18E, 0x1F18E}, //
	{0x1F191, 0x1F19A}, //
	{0x1F1E6, 0x1F1FF}, // regional indicators
	{0x1F200, 0x1F2FF}, //
	{0x1F300, 0x1F64F}, // pictographs + emoticons: 📁 🌿 📊 📆 🎯 💀
	{0x1F680, 0x1F6FF}, // transport
	{0x1F7E0, 0x1F7EB}, //
	{0x1F90C, 0x1F9FF}, // supplemental: 🤖 🧠
	{0x1FA70, 0x1FAFF}, //
	{0x20000, 0x3FFFD}, // CJK ext B..
}

func isWideRune(r rune) bool {
	for _, rg := range wideRanges {
		if r < rg[0] {
			return false // table is sorted: nothing further can match
		}
		if r <= rg[1] {
			return true
		}
	}
	return false
}

// runeWidth is the number of terminal columns one rune occupies. Combining
// marks, joiners and variation selectors are zero: they modify the glyph
// before them rather than adding a cell of their own.
func runeWidth(r rune) int {
	switch {
	case r == 0x200d, r == 0xfe0f, r == 0xfe0e:
		return 0
	case r < 0x20, r >= 0x7f && r < 0xa0:
		return 0 // control characters
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	case isWideRune(r):
		return 2
	}
	return 1
}

// displayWidth is the column count of s with ANSI escape sequences excluded.
func displayWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if n := escapeLen(s[i:]); n > 0 {
			i += n
			continue
		}
		r, sz := utf8.DecodeRuneInString(s[i:])
		i += sz
		w += runeWidth(r)
	}
	return w
}

// escapeLen returns the byte length of the ANSI escape sequence at the start
// of s, or 0 if there is none. Only the two forms this line can produce are
// recognised — CSI (colour, dim, reset) and OSC — which is enough to keep a
// painted string measurable.
func escapeLen(s string) int {
	if len(s) == 0 || s[0] != 0x1b {
		return 0
	}
	if len(s) == 1 {
		return 1
	}
	switch s[1] {
	case '[':
		i := 2
		for i < len(s) && (s[i] < '@' || s[i] > '~') {
			i++
		}
		if i < len(s) {
			i++
		}
		return i
	case ']':
		i := 2
		for i < len(s) && s[i] != 0x07 {
			i++
		}
		if i < len(s) {
			i++
		}
		return i
	}
	return 2
}

// truncateToWidth is the last resort when every documented reduction has
// been applied and the line still does not fit: cut it at max columns and
// mark the cut. Escapes are copied through rather than counted, so a colour
// is never severed mid-sequence, and a reset is appended when any was seen
// so the terminal is not left painted.
func truncateToWidth(s string, max int) string {
	if max <= 0 || displayWidth(s) <= max {
		return s
	}
	var b strings.Builder
	painted, w := false, 0
	for i := 0; i < len(s); {
		if n := escapeLen(s[i:]); n > 0 {
			painted = true
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		r, sz := utf8.DecodeRuneInString(s[i:])
		if w+runeWidth(r) > max-1 { // -1 leaves room for the ellipsis
			break
		}
		w += runeWidth(r)
		b.WriteString(s[i : i+sz])
		i += sz
	}
	b.WriteString("…")
	if painted {
		b.WriteString("\033[0m")
	}
	return b.String()
}

// terminalColumns is how wide the line may be, or 0 for "unknown".
//
// Unknown means no trimming at all. Assuming 80 would be worse than not
// trimming: a wrong guess permanently strips segments on a wide terminal,
// and the segments being dropped are the whole point of the feature.
// SPILLWAY_STATUSLINE_COLUMNS exists because the status line command is not
// run from an interactive shell, so COLUMNS is not guaranteed to reach it.
func terminalColumns() int {
	for _, k := range []string{"SPILLWAY_STATUSLINE_COLUMNS", "COLUMNS"} {
		if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k))); err == nil && n > 20 {
			return n
		}
	}
	return 0
}
