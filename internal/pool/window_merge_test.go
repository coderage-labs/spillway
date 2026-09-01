package pool

// Tests for issue #100: setWindowsSourced must merge incoming windows by
// Name, not replace everything sharing a Source wholesale. A response that
// says nothing about a window must never be read as that window going away.

import (
	"net/http"
	"testing"
	"time"
)

func windowNamed(windows []QuotaWindow, name string) *QuotaWindow {
	for i := range windows {
		if windows[i].Name == name {
			return &windows[i]
		}
	}
	return nil
}

// The reported bug: an account holding 7d-fable from an earlier fable
// response must keep it, unchanged, when a later plain response only
// carries [5h, 7d] — exactly what a Sonnet/Opus/Haiku response looks like
// (issue #25: absent prefixes are skipped, not synthesised as zero).
func TestNonFableResponseLeavesFableWindowIntact(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	resetAt := time.Now().Add(6 * 24 * time.Hour)
	fetchedAt := time.Now().Add(-time.Hour)

	a.setWindowsSourced("headers", []QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.1, FetchedAt: fetchedAt},
		{Name: "7d", Limit: 1, Used: 0.2, FetchedAt: fetchedAt},
		{Name: "7d-fable", Limit: 1, Used: 0.77, ResetAt: resetAt, FetchedAt: fetchedAt},
	})

	// A later plain Sonnet/Opus/Haiku response: no fable prefix at all.
	a.setWindowsSourced("headers", []QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.15, FetchedAt: time.Now()},
		{Name: "7d", Limit: 1, Used: 0.25, FetchedAt: time.Now()},
	})

	got := a.QuotaWindows()
	if len(got) != 3 {
		t.Fatalf("windows after non-fable response = %d, want 3 (5h, 7d, 7d-fable survives): %+v", len(got), got)
	}
	fable := windowNamed(got, "7d-fable")
	if fable == nil {
		t.Fatal("7d-fable window vanished after a non-fable response")
	}
	if fable.Used != 0.77 || fable.Limit != 1 || !fable.ResetAt.Equal(resetAt) || !fable.FetchedAt.Equal(fetchedAt) {
		t.Errorf("7d-fable window mutated by an update that never mentioned it: %+v", fable)
	}
}

// When a response DOES carry all three windows, all three must be
// refreshed to the new values.
func TestFullResponseRefreshesAllThreeWindows(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"

	a.setWindowsSourced("headers", []QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.1},
		{Name: "7d", Limit: 1, Used: 0.2},
		{Name: "7d-fable", Limit: 1, Used: 0.3},
	})
	a.setWindowsSourced("headers", []QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.4},
		{Name: "7d", Limit: 1, Used: 0.5},
		{Name: "7d-fable", Limit: 1, Used: 0.6},
	})

	got := a.QuotaWindows()
	if len(got) != 3 {
		t.Fatalf("windows = %d, want 3: %+v", len(got), got)
	}
	want := map[string]float64{"5h": 0.4, "7d": 0.5, "7d-fable": 0.6}
	for name, wantUsed := range want {
		w := windowNamed(got, name)
		if w == nil {
			t.Fatalf("window %q missing after refresh: %+v", name, got)
		}
		if w.Used != wantUsed {
			t.Errorf("window %q used = %v, want %v", name, w.Used, wantUsed)
		}
	}
}

// The 123-entry regression (issue #100's restated version of the earlier
// Kimi bug): repeated identical polls, run exactly the way
// accounts.PollKimiUsages calls SetQuotaWindows (a full window list on
// every tick), must never grow the stored slice.
func TestRepeatedIdenticalPollsDoNotGrowWindows(t *testing.T) {
	a := NewAccount("kimi", SourceYAML, "t", "", 0, "")
	a.Type = "kimi-oauth"

	poll := func() {
		a.SetQuotaWindows([]QuotaWindow{
			{Name: "weekly", Limit: 100, Used: 40, FetchedAt: time.Now()},
			{Name: "5h", Limit: 100, Used: 10, FetchedAt: time.Now()},
		})
	}
	for i := 0; i < 50; i++ {
		poll()
	}
	got := a.QuotaWindows()
	if len(got) != 2 {
		names := make([]string, len(got))
		for i, w := range got {
			names[i] = w.Name
		}
		t.Fatalf("after 50 identical polls: %d windows, want 2: %v", len(got), names)
	}
}

// Issue #54's MarkWindowRejected record must survive a subsequent
// non-fable response — the fix for #100 must not let a plain [5h, 7d]
// update wipe the hard rejection the way it used to wipe the soft window.
func TestMarkWindowRejectedSurvivesNonFableResponse(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())

	until := time.Now().Add(time.Hour)
	p.MarkWindowRejected(a, "7d-fable", until)

	if !a.WindowRejectedFor("7d-fable") {
		t.Fatal("precondition: 7d-fable must be rejected right after MarkWindowRejected")
	}

	// A subsequent plain response, carrying only the account-wide windows.
	p.RecordQuota(a, headersFor(map[string]string{
		"5h": "0.30",
		"7d": "0.40",
	}), time.Now())

	if !a.WindowRejectedFor("7d-fable") {
		t.Error("MarkWindowRejected's hard exclusion was undone by a later non-fable response")
	}
	got := a.QuotaWindows()
	fable := windowNamed(got, "7d-fable")
	if fable == nil {
		t.Fatal("7d-fable window record disappeared after a non-fable response")
	}
	if fable.Used != 1 || fable.Limit != 1 || !fable.ResetAt.Equal(until) {
		t.Errorf("7d-fable window record changed by a response that never mentioned it: %+v", fable)
	}
}

// #24's per-family judgement must still work correctly for fable after a
// run of Opus (non-fable) traffic has passed through the account.
func TestOverThresholdForFableJudgesCorrectlyAfterOpusTraffic(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"

	// Fable bucket spent by an earlier fable response.
	a.setWindowsSourced("headers", []QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.1},
		{Name: "7d", Limit: 1, Used: 0.1},
		{Name: "7d-fable", Limit: 1, Used: 0.99},
	})

	// A run of Opus (non-fable) traffic: headers carry only 5h/7d.
	for i := 0; i < 5; i++ {
		a.setWindowsSourced("headers", []QuotaWindow{
			{Name: "5h", Limit: 1, Used: 0.2 + float64(i)*0.01},
			{Name: "7d", Limit: 1, Used: 0.3},
		})
	}

	if !a.OverThresholdFor(fableModel, 0.98) {
		t.Error("OverThresholdFor(fable) = false after Opus traffic, want true: fable window must survive to be judged")
	}
	if a.OverThresholdFor(sonnetModel, 0.98) {
		t.Error("OverThresholdFor(sonnet) = true, want false: 5h/7d never went near threshold")
	}
}

func headersFor(vals map[string]string) http.Header {
	h := http.Header{}
	for name, v := range vals {
		h.Set("Anthropic-Ratelimit-Unified-"+name+"-Utilization", v)
	}
	return h
}
