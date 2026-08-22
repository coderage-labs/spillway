package main

// Aligned terminal tables, over text/tabwriter.
//
// The first version of this hand-rolled the column measuring and padding,
// which is exactly what tabwriter has done in the standard library since Go
// 1.0 — about ninety lines to reimplement, badly, something already present
// and better tested. This wrapper exists only for the two things tabwriter
// deliberately leaves to the caller: uppercase headers, and right-alignment
// of individual columns (its AlignRight flag is all-or-nothing for the whole
// writer, and account names must stay left-aligned next to numbers).

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"unicode/utf8"
)

type table struct {
	head  []string
	right map[int]bool
	rows  [][]string
}

func newTable(head ...string) *table {
	return &table{head: head, right: map[int]bool{}}
}

// rightAlign marks numeric columns, where a ragged left edge is harder to
// compare than a ragged right one.
func (t *table) rightAlign(cols ...int) *table {
	for _, c := range cols {
		t.right[c] = true
	}
	return t
}

func (t *table) add(cells ...string) { t.rows = append(t.rows, cells) }

func (t *table) render(w io.Writer) {
	if len(t.rows) == 0 {
		return
	}
	// Pre-pad the right-aligned columns ourselves, then let tabwriter do the
	// rest. Padding to the widest cell in the column makes each of them a
	// fixed-width string, which tabwriter then left-aligns — the same result.
	width := map[int]int{}
	note := func(cells []string) {
		for i, c := range cells {
			if t.right[i] {
				if n := utf8.RuneCountInString(c); n > width[i] {
					width[i] = n
				}
			}
		}
	}
	note(t.head)
	for _, r := range t.rows {
		note(r)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	line := func(cells []string, upper bool) {
		out := make([]string, len(cells))
		for i, c := range cells {
			if upper {
				c = strings.ToUpper(c)
			}
			if pad := width[i] - utf8.RuneCountInString(c); t.right[i] && pad > 0 {
				c = strings.Repeat(" ", pad) + c
			}
			out[i] = c
		}
		// No trailing tab: tabwriter pads every cell that is followed by one,
		// and a padded final column drags whitespace into anything copied out
		// of the terminal.
		fmt.Fprintln(tw, strings.Join(out, "\t"))
	}
	if len(t.head) > 0 {
		line(t.head, true)
	}
	for _, r := range t.rows {
		line(r, false)
	}
	_ = tw.Flush()
}
