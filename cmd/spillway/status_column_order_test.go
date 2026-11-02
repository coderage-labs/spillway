package main

import (
	"strings"
	"testing"
)

// Issue #262 follow-up: columns followed first-seen order, so an Ultra account
// (no 5h window) at the top of the table pushed 5H LEFT to the far right.
func TestStatusColumnsUseCanonicalWindowOrder(t *testing.T) {
	const accounts = `[
		{"name":"ultra","type":"claude-oauth","state":"ok","inFlight":0,
		 "absentWindows":["5h"],
		 "quotaWindows":[{"name":"7d","limit":1,"used":0.5},{"name":"7d-fable","limit":1,"used":0.1}]},
		{"name":"team","type":"claude-oauth","state":"ok","inFlight":0,
		 "quotaWindows":[{"name":"5h","limit":1,"used":0.2},{"name":"7d","limit":1,"used":0.4}]}
	]`
	out := renderStatusWith(t, accounts, `{}`)
	h5 := strings.Index(out, "5H LEFT")
	h7 := strings.Index(out, "7D LEFT")
	hf := strings.Index(out, "7D-FABLE LEFT")
	if h5 < 0 || h7 < 0 || hf < 0 {
		t.Fatalf("missing a window column:\n%s", out)
	}
	if !(h5 < h7 && h7 < hf) {
		t.Errorf("columns not in 5h, 7d, 7d-fable order:\n%s", out)
	}
}
