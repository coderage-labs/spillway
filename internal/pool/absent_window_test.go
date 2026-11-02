package pool

// Issue #262: Max 20x ("Ultra") has no 5h limit. A response that carries the
// unified quota headers but no 5h drops the window and marks it absent; the
// header reappearing restores it. Driven through RecordQuota, the one function
// both live responses and probes merge their headers with.

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func unifiedHeaders(status bool, wins ...string) http.Header {
	h := http.Header{}
	if status {
		h.Set("anthropic-ratelimit-unified-status", "allowed")
	}
	reset := strconv.FormatInt(time.Now().Add(3*time.Hour).Unix(), 10)
	for _, w := range wins {
		h.Set("anthropic-ratelimit-unified-"+w+"-utilization", "0.30")
		h.Set("anthropic-ratelimit-unified-"+w+"-reset", reset)
	}
	return h
}

func absentTestAccount(t *testing.T) (*Pool, *Account) {
	t.Helper()
	a := NewAccount("ultra", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	return New([]*Account{a}, time.Now()), a
}

func hasWindow(a *Account, name string) bool { return windowNamed(a.QuotaWindows(), name) != nil }

func TestResponseWithoutFiveHourDropsItAndMarksAbsent(t *testing.T) {
	p, a := absentTestAccount(t)
	now := time.Now()
	p.RecordQuota(a, unifiedHeaders(true, "5h", "7d"), now.Add(-time.Hour))
	if !hasWindow(a, "5h") || len(a.AbsentWindows()) != 0 {
		t.Fatalf("precondition: 5h should be live, absent=%v", a.AbsentWindows())
	}

	p.RecordQuota(a, unifiedHeaders(true, "7d"), now)

	if hasWindow(a, "5h") {
		t.Error("stale 5h kept after a unified response without it")
	}
	if !hasWindow(a, "7d") {
		t.Error("7d lost: only the missing window may be dropped")
	}
	if got := a.AbsentWindows(); len(got) != 1 || got[0] != "5h" {
		t.Errorf("AbsentWindows = %v, want [5h]", got)
	}
}

func TestFiveHourReappearingRestoresItAndClearsAbsent(t *testing.T) {
	p, a := absentTestAccount(t)
	now := time.Now()
	p.RecordQuota(a, unifiedHeaders(true, "7d"), now.Add(-time.Hour))
	if len(a.AbsentWindows()) != 1 {
		t.Fatalf("precondition: 5h should be absent, got %v", a.AbsentWindows())
	}

	p.RecordQuota(a, unifiedHeaders(true, "5h", "7d"), now)

	if !hasWindow(a, "5h") {
		t.Error("5h not restored when its headers came back")
	}
	if got := a.AbsentWindows(); len(got) != 0 {
		t.Errorf("absent mark not cleared: %v", got)
	}
}

// An error body or partial response carries no overall status; it says
// nothing about which windows the plan has.
func TestResponseWithoutUnifiedStatusChangesNothing(t *testing.T) {
	p, a := absentTestAccount(t)
	now := time.Now()
	p.RecordQuota(a, unifiedHeaders(true, "5h", "7d"), now.Add(-time.Hour))

	p.RecordQuota(a, unifiedHeaders(false, "7d"), now)
	p.RecordQuota(a, http.Header{}, now)

	if !hasWindow(a, "5h") {
		t.Error("5h dropped by a response with no unified status header")
	}
	if got := a.AbsentWindows(); len(got) != 0 {
		t.Errorf("absent marked from a response with no unified status: %v", got)
	}
}

// A status header with no window data at all is malformed, not a plan with
// neither window: it must not wipe the account.
func TestStatusOnlyResponseDropsNothing(t *testing.T) {
	p, a := absentTestAccount(t)
	now := time.Now()
	p.RecordQuota(a, unifiedHeaders(true, "5h", "7d"), now.Add(-time.Hour))

	p.RecordQuota(a, unifiedHeaders(true), now)

	if !hasWindow(a, "5h") || !hasWindow(a, "7d") || len(a.AbsentWindows()) != 0 {
		t.Errorf("status-only response changed state: %+v absent=%v", a.QuotaWindows(), a.AbsentWindows())
	}
}

// 7d-fable rides only on fable responses; its absence means nothing.
func TestFableLessResponseNeverDropsFableWindow(t *testing.T) {
	p, a := absentTestAccount(t)
	now := time.Now()
	p.RecordQuota(a, unifiedHeaders(true, "5h", "7d", "7d_oi"), now.Add(-time.Hour))
	if !hasWindow(a, "7d-fable") {
		t.Fatal("precondition: 7d-fable should be live")
	}

	p.RecordQuota(a, unifiedHeaders(true, "7d"), now) // plain Sonnet-style, and 5h absent too

	if !hasWindow(a, "7d-fable") {
		t.Error("7d-fable dropped by a response that never carries it")
	}
	for _, n := range a.AbsentWindows() {
		if n == "7d-fable" {
			t.Error("7d-fable marked absent")
		}
	}
}

// Providers without the guarantee (Kimi) never get windows marked absent.
func TestNonClaudeProviderNeverMarksAbsent(t *testing.T) {
	a := NewAccount("k", SourceYAML, "t", "", 0, "")
	a.Type = "kimi-oauth"
	p := New([]*Account{a}, time.Now())
	p.RecordQuota(a, unifiedHeaders(true, "7d"), time.Now())
	if got := a.AbsentWindows(); len(got) != 0 {
		t.Errorf("absent on a provider with no such guarantee: %v", got)
	}
}
