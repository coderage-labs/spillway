package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func renderToString(t *table) string {
	var b strings.Builder
	t.render(&b)
	return b.String()
}

// The bug this replaces: tab-separated fields line up only when values happen
// to be the same length, because a terminal renders a tab as "jump to the
// next multiple of eight", not "next column".
func TestTableAlignsColumnsRegardlessOfWidth(t *testing.T) {
	tb := newTable("account", "state")
	tb.add("a@short.example", "ok")
	tb.add("someone@a-much-longer-domain.example", "exhausted")

	lines := strings.Split(strings.TrimRight(renderToString(tb), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want header + 2 rows:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	// The second column must start at the same offset on every line.
	col := strings.Index(lines[0], "STATE")
	for _, l := range lines[1:] {
		got := strings.Index(l, strings.Fields(l)[1])
		if got != col {
			t.Errorf("second column starts at %d, header has it at %d:\n%s", got, col, l)
		}
	}
}

func TestTableUppercasesOnlyTheHeader(t *testing.T) {
	tb := newTable("account")
	tb.add("Mixed@Case.example")
	out := renderToString(tb)
	if !strings.HasPrefix(out, "ACCOUNT") {
		t.Errorf("header not uppercased:\n%s", out)
	}
	if !strings.Contains(out, "Mixed@Case.example") {
		t.Errorf("a value was case-folded; account names are identifiers:\n%s", out)
	}
}

// Right alignment is for numbers, where a ragged left edge is harder to scan
// than a ragged right one.
func TestTableRightAlignsMarkedColumns(t *testing.T) {
	tb := newTable("account", "in flight").rightAlign(1)
	tb.add("a", "7")
	tb.add("b", "1234")
	out := renderToString(tb)
	if !strings.Contains(out, "   7") {
		t.Errorf("narrow number not right-aligned:\n%s", out)
	}
}

// Copying a line out of a terminal should not drag whitespace with it.
func TestTableDoesNotPadTheLastColumn(t *testing.T) {
	tb := newTable("a", "b")
	tb.add("x", "short")
	tb.add("y", "much longer value")
	for _, l := range strings.Split(strings.TrimRight(renderToString(tb), "\n"), "\n") {
		if l != strings.TrimRight(l, " ") {
			t.Errorf("line has trailing whitespace: %q", l)
		}
	}
}

// Width is measured in runes. Counting bytes over-pads any non-ASCII name by
// the length of its encoding.
func TestTableMeasuresRunesNotBytes(t *testing.T) {
	tb := newTable("name", "x")
	tb.add("José", "1")
	tb.add("Bob", "2")
	lines := strings.Split(strings.TrimRight(renderToString(tb), "\n"), "\n")
	// Rune offsets, not byte offsets: strings.Index counts bytes, so on this
	// very input it reports a false misalignment for correctly rendered
	// output. That is the same confusion the renderer has to avoid.
	col := func(line, marker string) int {
		return utf8.RuneCountInString(line[:strings.Index(line, marker)])
	}
	a := col(lines[1], "1")
	b := col(lines[2], "2")
	if a != b {
		t.Errorf("columns misaligned by multibyte name (%d vs %d):\n%s", a, b, strings.Join(lines, "\n"))
	}
}

func TestTableWithNoRowsPrintsNothing(t *testing.T) {
	if out := renderToString(newTable("a", "b")); out != "" {
		t.Errorf("empty table printed %q; the caller says 'no accounts' itself", out)
	}
}
