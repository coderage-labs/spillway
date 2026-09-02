package main

// Issue #135: a window past its reset with nothing re-measured since is
// unknown, not dry. The status line must neither draw it as a 0% bar with a
// refill countdown nor let it sink the account's ranking.

import (
	"strings"
	"testing"
	"time"
)

func TestRenderOmitsExpiredWindow(t *testing.T) {
	a := okAccount()
	a.Windows = append(a.Windows, slWindow{
		Name: "7d-fable", Limit: 1, Used: 1, ResetAt: time.Now().Add(-time.Hour), Expired: true,
	})
	got := render(noColour, []slAccount{a}, slState{Total: 1}, time.Now())
	if !strings.Contains(got, "94%") {
		t.Errorf("live window missing from %q", got)
	}
	if strings.Contains(got, "7d-fable") || strings.Contains(got, "↻") {
		t.Errorf("expired window rendered as a level with a countdown: %q", got)
	}
}

// An account is only as good as its worst LIVE window: an expired spent
// reading is not a window at all for ranking purposes.
func TestPickAccountIgnoresExpiredWindow(t *testing.T) {
	poor := slAccount{Name: "poor", State: "ok", Windows: []slWindow{win("5h", 0.9, 1)}}
	mixed := slAccount{Name: "mixed", State: "ok", Windows: []slWindow{
		win("5h", 0, 1),
		{Name: "7d", Used: 1, Limit: 1, ResetAt: time.Now().Add(-time.Hour), Expired: true},
	}}
	if got := pickAccount([]slAccount{poor, mixed}); got.Name != "mixed" {
		t.Errorf("an expired spent window sank the account; got %q, want mixed", got.Name)
	}
}
