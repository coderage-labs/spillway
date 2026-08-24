package main

import "testing"

// TestHistoryStartNeverExceedsCurrent guards the one piece of real logic in
// this command: the seeded quota history must climb into the account's live
// Used, never start above it. A window's usage only ever climbs until its
// own reset — a starting point above cur would draw an impossible dip on
// the headroom chart for any account whose live figure is small enough to
// hit the 0.02 floor before the *0.35 scale-down does.
func TestHistoryStartNeverExceedsCurrent(t *testing.T) {
	cases := []float64{0, 0.005, 0.01, 0.02, 0.0571, 0.1, 0.34, 0.5, 0.985, 1}
	for _, cur := range cases {
		start := historyStart(cur)
		if start > cur {
			t.Errorf("historyStart(%v) = %v, want <= %v (a history point may not exceed the live figure)", cur, start, cur)
		}
		if start < 0 {
			t.Errorf("historyStart(%v) = %v, want >= 0", cur, start)
		}
	}
}
