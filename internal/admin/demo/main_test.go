package main

import "testing"

// TestHistoryStartNeverExceedsCurrent guards the invariant both seeded
// starting points share: a history point may never exceed the account's
// live Used. A window's usage only ever climbs until its own reset — a
// starting point above cur would draw an impossible dip on the headroom
// chart. historyStartShort needs its own clamp for this (cur*0.35 can land
// below the 0.02 floor); historyStartLong is subtractive so it can't
// overshoot on its own, but the invariant is checked on both so either can
// be changed later without silently reintroducing the dip.
func TestHistoryStartNeverExceedsCurrent(t *testing.T) {
	cases := []float64{0, 0.005, 0.01, 0.02, 0.0571, 0.1, 0.34, 0.5, 0.985, 1}
	for _, cur := range cases {
		for _, fn := range []struct {
			name string
			f    func(float64) float64
		}{
			{"historyStartShort", historyStartShort},
			{"historyStartLong", historyStartLong},
		} {
			start := fn.f(cur)
			if start > cur {
				t.Errorf("%s(%v) = %v, want <= %v (a history point may not exceed the live figure)", fn.name, cur, start, cur)
			}
			if start < 0 {
				t.Errorf("%s(%v) = %v, want >= 0", fn.name, cur, start)
			}
		}
	}
}

// TestHistoryStartLongClimbIsSmall guards the reason historyStartLong
// exists: a window whose reset is days away must climb only a little over
// the seeded history, or the dashboard's burn-rate projection reads a
// multi-day reset as about to be beaten and raises a false "runs dry"
// alarm (what happened to this demo's own healthy-pool screenshot before
// this fix — see historyStartLong's doc comment). A climb bigger than this
// bound is exactly the regression that would reintroduce it.
func TestHistoryStartLongClimbIsSmall(t *testing.T) {
	const maxClimb = 0.011 // historyStartLong's intended 0.01, plus float slack
	for _, cur := range []float64{0, 0.005, 0.01, 0.05, 0.34, 0.53, 0.985, 1} {
		climb := cur - historyStartLong(cur)
		if climb > maxClimb {
			t.Errorf("historyStartLong(%v) climbs by %v, want <= %v (a bigger climb read against a multi-day reset false-alarms)", cur, climb, maxClimb)
		}
	}
}
