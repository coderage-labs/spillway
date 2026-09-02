package pool

// A spent reading is evidence only until its own reset (issue #135).
//
// Before this, the three threshold scanners compared Used/Limit with no
// reference to ResetAt, so a spent window counted forever within one
// daemon's life — and a spent 7d-fable could never be re-measured, because
// only a fable response carries that header and the spent reading was
// exactly what kept fable requests away. These tests pin the expiry rule,
// its deliberate edges, the selection outcome the issue reported, and the
// cycle-sibling retirement that covers a reset header lagging the refill it
// announces.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func claudeAccountPrio(name string, prio int) *Account {
	a := NewAccount(name, SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	a.SetPriority(prio)
	return a
}

// All three scanners agree: a spent window counts while its reset is ahead
// or unknown, and stops counting once the reset has passed.
func TestThresholdScannersHonourReset(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		reset time.Time
		want  bool
	}{
		// Unknown reset: the spent reading stays spent. That is the idle
		// probe's rule (accounts.wouldBill never spends to re-learn a window
		// it cannot date) and keeps every zero-ResetAt fixture in this
		// package meaning what it always meant.
		{"zero reset stays spent", time.Time{}, true},
		{"future reset stays spent", now.Add(time.Hour), true},
		{"past reset no longer counts", now.Add(-time.Minute), false},
		{"reset exactly now no longer counts", now, false},
	} {
		a := claudeAccountPrio("a", 0)
		a.SetQuotaWindows([]QuotaWindow{{
			Name: "7d-fable", Limit: 1, Used: 1, Source: "headers",
			ResetAt: tc.reset, FetchedAt: now.Add(-2 * time.Hour),
		}})
		got := map[string]bool{
			"OverThreshold":          a.overThresholdAt(0.98, now),
			"OverThresholdFor":       a.overThresholdForAt(fableModel, 0.98, now),
			"OverThresholdForWindow": a.overThresholdForWindowAt("7d-fable", 0.98, now),
		}
		for scanner, g := range got {
			if g != tc.want {
				t.Errorf("%s: %s = %v, want %v", tc.name, scanner, g, tc.want)
			}
		}
	}
}

// A reading taken at or after its own reset is the newest truth, not a stale
// one: Anthropic can report a reset that has already passed, and treating
// such a fresh header as expired would let a fable request land, bill on
// extra usage, and repeat every turn.
func TestReadingFetchedAfterItsResetStaysCurrent(t *testing.T) {
	now := time.Now()
	a := claudeAccountPrio("a", 0)
	a.SetQuotaWindows([]QuotaWindow{{
		Name: "7d-fable", Limit: 1, Used: 1, Source: "headers",
		ResetAt: now.Add(-time.Hour), FetchedAt: now.Add(-time.Minute),
	}})
	if !a.overThresholdForAt(fableModel, 0.98, now) {
		t.Error("a spent reading fetched AFTER its reset was treated as expired; it is the freshest fact on file")
	}
	if windowNamed(a.QuotaWindows(), "7d-fable").Expired {
		t.Error("QuotaWindows() flagged a post-reset reading expired")
	}
}

// Kimi has no family scoping, so OverThresholdFor falls back to the plain
// scan — which must honour the reset the same way.
func TestKimiFallbackHonoursReset(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		reset time.Time
		want  bool
	}{
		{"future", now.Add(time.Hour), true},
		{"past", now.Add(-time.Minute), false},
	} {
		a := NewAccount("k", SourceYAML, "t", "", 0, "")
		a.Type = "kimi-oauth"
		a.SetQuotaWindows([]QuotaWindow{{
			Name: "weekly", Limit: 100, Used: 100, Source: "poll",
			ResetAt: tc.reset, FetchedAt: now.Add(-2 * time.Hour),
		}})
		if got := a.overThresholdForAt("k2", 0.98, now); got != tc.want {
			t.Errorf("%s reset: kimi OverThresholdFor = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The scenario issue #135 reports: the preferred account's fable window is
// spent but past its reset; the secondary account is healthy. Selection must
// return to the preferred account. The control direction — same reading
// with its reset still ahead — must keep deprioritising it, so an inverted
// comparison cannot pass this test.
func TestSpentFableWindowStopsGatingAfterItsReset(t *testing.T) {
	now := time.Now()
	work := claudeAccountPrio("work", 0)
	personal := claudeAccountPrio("personal", 1)
	work.SetQuotaWindows([]QuotaWindow{
		{Name: "7d", Limit: 1, Used: 0.03, ResetAt: now.Add(15 * time.Hour), FetchedAt: now},
		{Name: "7d-fable", Limit: 1, Used: 1, ResetAt: now.Add(-time.Minute), FetchedAt: now.Add(-40 * time.Hour)},
	})
	personal.SetQuotaWindows([]QuotaWindow{
		{Name: "7d-fable", Limit: 1, Used: 0.08, ResetAt: now.Add(100 * time.Hour), FetchedAt: now},
	})
	// personal listed first, so list order alone cannot explain a pass.
	p := New([]*Account{personal, work}, now)

	got := p.SelectFor("s1", fableBody(fableModel))
	if got != work {
		t.Fatalf("SelectFor(fable) = %v, want work — its spent fable reading is past its reset and must not gate it", gotName(got))
	}
	p.Done(got)

	work.SetQuotaWindows([]QuotaWindow{
		{Name: "7d-fable", Limit: 1, Used: 1, ResetAt: now.Add(time.Hour), FetchedAt: now.Add(-40 * time.Hour)},
	})
	got = p.SelectFor("s2", fableBody(fableModel))
	if got != personal {
		t.Fatalf("SelectFor(fable) with the reset still ahead = %v, want personal — the spent reading is still evidence", gotName(got))
	}
}

// The forged row MarkWindowRejected writes (Used=1, ResetAt=until) expires
// together with the windowRejected entry, instead of outliving it and
// keeping fableSpent true forever.
func TestForgedRejectionRowExpiresWithItsDeadline(t *testing.T) {
	now := time.Now()
	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, now)
	until := now.Add(time.Hour)
	p.MarkWindowRejected(a, "7d-fable", until)

	if !a.overThresholdForWindowAt("7d-fable", 0.98, now) {
		t.Fatal("forged row must read spent while its deadline is ahead")
	}
	later := until.Add(time.Minute)
	if a.overThresholdForWindowAt("7d-fable", 0.98, later) {
		t.Error("forged 7d-fable row still reads spent after its own deadline (issue #135)")
	}
	if a.overThresholdForAt(fableModel, 0.98, later) {
		t.Error("forged row still deprioritises the account for fable after its deadline")
	}
}

// QuotaWindows() stamps Expired on every copy it returns and never trusts a
// stored value; the JSON carries the flag only where it is true.
func TestQuotaWindowsStampsExpired(t *testing.T) {
	now := time.Now()
	a := claudeAccountPrio("a", 0)
	a.SetQuotaWindows([]QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.5, FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 0.5, ResetAt: now.Add(time.Hour), FetchedAt: now},
		{Name: "7d-fable", Limit: 1, Used: 1, ResetAt: now.Add(-time.Minute), FetchedAt: now.Add(-time.Hour)},
	})
	want := map[string]bool{"5h": false, "7d": false, "7d-fable": true}
	for _, w := range a.QuotaWindows() {
		if w.Expired != want[w.Name] {
			t.Errorf("%s: Expired = %v, want %v", w.Name, w.Expired, want[w.Name])
		}
	}

	// A caller round-tripping a window it read back cannot store a stale flag.
	a.SetQuotaWindows([]QuotaWindow{
		{Name: "7d", Limit: 1, Used: 0.5, ResetAt: now.Add(time.Hour), FetchedAt: now, Expired: true},
	})
	if windowNamed(a.QuotaWindows(), "7d").Expired {
		t.Error("a stored Expired=true survived QuotaWindows(); the flag must be recomputed on every read")
	}

	b, err := json.Marshal(a.QuotaWindows())
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), `"expired":true`); n != 1 {
		t.Errorf(`"expired":true appears %d times in %s, want exactly once (omitempty elsewhere)`, n, b)
	}
}

// An expired window no longer makes an overage-permitted account look
// billable, so it is not shoved out of the free tier on a stale reading.
func TestWouldBillIgnoresExpiredWindow(t *testing.T) {
	now := time.Now()
	a := claudeAccountPrio("paid", 0)
	a.allowOverage = boolp(true)
	a.SetQuotaWindows([]QuotaWindow{
		{Name: "7d", Limit: 1, Used: 1, ResetAt: now.Add(-time.Minute), FetchedAt: now.Add(-time.Hour)},
	})
	p := New([]*Account{a}, now)
	p.allowOverage = true
	if p.WouldBill(a) {
		t.Error("WouldBill = true on an expired reading; nothing current says this account is spent")
	}
	a.SetQuotaWindows([]QuotaWindow{
		{Name: "7d", Limit: 1, Used: 1, ResetAt: now.Add(time.Hour), FetchedAt: now.Add(-time.Hour)},
	})
	if !p.WouldBill(a) {
		t.Error("WouldBill = false with the reset still ahead; the spent reading is current")
	}
}

// Measured live 2026-09-01: 7d fell 0.89 -> 0.0 while the reported reset
// stayed thirty-one hours ahead, and 7d-fable — same reported reset, only
// ever re-measured by a fable response — sat at 1.0 for those hours. A
// turnover seen on one window ends the cycle of every stored window that
// shared its reset and was not re-measured by the same response.
func TestSiblingTurnoverRetiresSameCycleWindow(t *testing.T) {
	now := time.Now()
	reset := now.Add(31 * time.Hour) // the provider's lagging reported reset, shared
	a := claudeAccountPrio("a", 0)
	a.SetQuotaWindows([]QuotaWindow{
		{Name: "7d", Limit: 1, Used: 0.89, ResetAt: reset, FetchedAt: now.Add(-3 * time.Hour)},
		{Name: "7d-fable", Limit: 1, Used: 1, ResetAt: reset, FetchedAt: now.Add(-40 * time.Hour)},
		// Same age, different cycle: must be left alone.
		{Name: "other", Limit: 1, Used: 1, ResetAt: reset.Add(time.Hour), FetchedAt: now.Add(-40 * time.Hour)},
	})
	if !a.overThresholdForAt(fableModel, 0.98, now) {
		t.Fatal("precondition: fable is spent before the turnover")
	}

	// A non-fable response: 7d turned over, 7d-fable absent.
	a.setWindowsSourced("headers", []QuotaWindow{
		{Name: "7d", Limit: 1, Used: 0.0, ResetAt: reset, FetchedAt: now},
	})
	if a.overThresholdForAt(fableModel, 0.98, now) {
		t.Error("7d-fable still gates the account after its cycle-sibling 7d demonstrably turned over")
	}
	f := windowNamed(a.QuotaWindows(), "7d-fable")
	if f == nil || !f.Expired {
		t.Errorf("7d-fable = %+v, want retired (Expired) — its reset should now be the observed turnover time", f)
	}
	if !f.ResetAt.Equal(now) {
		t.Errorf("retired window ResetAt = %v, want the turnover time %v (measured, not guessed)", f.ResetAt, now)
	}
	if o := windowNamed(a.QuotaWindows(), "other"); o == nil || o.Expired || !o.ResetAt.Equal(reset.Add(time.Hour)) {
		t.Errorf("window on a different cycle was touched: %+v", o)
	}
}

// What must NOT count as a turnover: a limit boost, a barely-used window, a
// sibling on a different cycle, a window the same response re-measured, an
// undated cycle, and a reading with no timestamp of its own.
func TestSiblingTurnoverNegativeCases(t *testing.T) {
	now := time.Now()
	reset := now.Add(31 * time.Hour)
	type step struct {
		name    string
		stored  []QuotaWindow
		arrive  []QuotaWindow
		retired bool
	}
	fable := func(r time.Time) QuotaWindow {
		return QuotaWindow{Name: "7d-fable", Limit: 1, Used: 1, ResetAt: r, FetchedAt: now.Add(-40 * time.Hour)}
	}
	sevenD := func(used float64, r, fetched time.Time) QuotaWindow {
		return QuotaWindow{Name: "7d", Limit: 1, Used: used, ResetAt: r, FetchedAt: fetched}
	}
	for _, tc := range []step{
		{"boost-style drop is not a turnover",
			[]QuotaWindow{sevenD(0.89, reset, now.Add(-time.Hour)), fable(reset)},
			[]QuotaWindow{sevenD(0.59, reset, now)}, false},
		{"barely-used window proves nothing",
			[]QuotaWindow{sevenD(0.06, reset, now.Add(-time.Hour)), fable(reset)},
			[]QuotaWindow{sevenD(0.0, reset, now)}, false},
		{"sibling on a different cycle is left alone",
			[]QuotaWindow{sevenD(0.89, reset, now.Add(-time.Hour)), fable(reset.Add(time.Hour))},
			[]QuotaWindow{sevenD(0.0, reset, now)}, false},
		{"undated cycle relates to nothing",
			[]QuotaWindow{sevenD(0.89, time.Time{}, now.Add(-time.Hour)), fable(time.Time{})},
			[]QuotaWindow{sevenD(0.0, time.Time{}, now)}, false},
		{"a reading with no timestamp cannot date a turnover",
			[]QuotaWindow{sevenD(0.89, reset, now.Add(-time.Hour)), fable(reset)},
			[]QuotaWindow{sevenD(0.0, reset, time.Time{})}, false},
		{"real turnover retires the sibling",
			[]QuotaWindow{sevenD(0.89, reset, now.Add(-time.Hour)), fable(reset)},
			[]QuotaWindow{sevenD(0.0, reset, now)}, true},
	} {
		a := claudeAccountPrio("a", 0)
		a.SetQuotaWindows(tc.stored)
		a.setWindowsSourced("headers", tc.arrive)
		if got := windowNamed(a.QuotaWindows(), "7d-fable").Expired; got != tc.retired {
			t.Errorf("%s: 7d-fable retired = %v, want %v", tc.name, got, tc.retired)
		}
	}

	// A response that re-measures the family itself simply replaces it —
	// the fresh reading wins, nothing is retired.
	a := claudeAccountPrio("a", 0)
	a.SetQuotaWindows([]QuotaWindow{sevenD(0.89, reset, now.Add(-time.Hour)), fable(reset)})
	a.setWindowsSourced("headers", []QuotaWindow{
		sevenD(0.0, reset, now),
		{Name: "7d-fable", Limit: 1, Used: 0.0, ResetAt: reset, FetchedAt: now},
	})
	if f := windowNamed(a.QuotaWindows(), "7d-fable"); f.Expired || f.Used != 0 || !f.ResetAt.Equal(reset) {
		t.Errorf("re-measured family window was not simply replaced: %+v", f)
	}
}
