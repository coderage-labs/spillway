package main

// Issue #262: a plan with no 5h limit says "no limit" where the 5h reading
// would be, on both the status table and the status line.

import (
	"strings"
	"testing"
	"time"
)

func TestStatusShowsNoLimitForAnAbsentFiveHour(t *testing.T) {
	out := renderStatus(t, `[
	 {"name":"normal","type":"claude-oauth","state":"ok","quotaWindows":[
	   {"name":"5h","limit":1,"used":0.2},{"name":"7d","limit":1,"used":0.1}]},
	 {"name":"ultra","type":"claude-oauth","state":"ok","absentWindows":["5h"],"quotaWindows":[
	   {"name":"7d","limit":1,"used":0.27}]},
	 {"name":"fresh","type":"claude-oauth","state":"ok"}]`)
	rows := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			rows[f[0]] = l
		}
	}
	if !strings.Contains(rows["ultra"], "no limit") {
		t.Errorf("ultra row lacks \"no limit\":\n%s", out)
	}
	if strings.Contains(rows["fresh"], "no limit") || strings.Contains(rows["normal"], "no limit") {
		t.Errorf("\"no limit\" leaked onto a row that has not shown an absent window:\n%s", out)
	}
}

func TestStatuslineShowsNoLimitForAnAbsentFiveHour(t *testing.T) {
	a := slAccount{Name: "me@ultra.com", State: "ok", AbsentWindows: []string{"5h"},
		Windows: []slWindow{{Name: "7d", Limit: 1, Used: 0.27, ResetAt: time.Now().Add(96 * time.Hour)}}}
	got := render(noColour, []slAccount{a}, slState{Total: 1}, time.Now())
	if !strings.Contains(got, "no limit 5h") {
		t.Errorf("status line lacks the absent 5h: %q", got)
	}
	if !strings.Contains(got, "73%") {
		t.Errorf("status line lost the 7d window: %q", got)
	}
	if strings.Contains(got, "no quota signal") {
		t.Errorf("an absent 5h plus a live 7d is not \"no quota signal\": %q", got)
	}

	fresh := slAccount{Name: "me@fresh.com", State: "ok"}
	if got := render(noColour, []slAccount{fresh}, slState{Total: 1}, time.Now()); strings.Contains(got, "no limit") {
		t.Errorf("never-measured account shows \"no limit\": %q", got)
	}
}
