package pool

import (
	"net/http"
	"testing"
	"time"
)

func TestSelectStickyUntilExhausted(t *testing.T) {
	a := &Account{Name: "a"}
	b := &Account{Name: "b"}
	p := New([]*Account{a, b}, time.Now())

	first := p.Select("session-1")
	if first == nil {
		t.Fatal("no account selected")
	}
	second := p.Select("session-1")
	if second != first {
		t.Errorf("sticky violated: %v then %v", first.Name, second.Name)
	}
	p.Done(first)
	p.Done(second)

	p.MarkExhausted(first, time.Now().Add(time.Hour))
	third := p.Select("session-1")
	if third == first {
		t.Error("selected exhausted account")
	}
	if third != a && third != b {
		t.Fatal("nil selection")
	}
	// Sticky must move to the new account.
	fourth := p.Select("session-1")
	if fourth != third {
		t.Errorf("stickiness did not move to replacement account")
	}
}

func TestSelectLeastInFlight(t *testing.T) {
	a := &Account{Name: "a"}
	b := &Account{Name: "b"}
	p := New([]*Account{a, b}, time.Now())

	x := p.Select("s1") // holds one in-flight
	y := p.Select("s2")
	if y == x {
		t.Errorf("contended selection piled onto the busy account")
	}
	p.Done(x)
	p.Done(y)
}

func TestSelectNilWhenAllExhausted(t *testing.T) {
	a := &Account{Name: "a"}
	p := New([]*Account{a}, time.Now())
	p.MarkExhausted(a, time.Now().Add(time.Hour))
	if got := p.Select("s"); got != nil {
		t.Errorf("Select = %v, want nil", got.Name)
	}
}

func TestExhaustedRecoversAfterWindow(t *testing.T) {
	a := &Account{Name: "a"}
	p := New([]*Account{a}, time.Now())
	p.MarkExhausted(a, time.Now().Add(-time.Second)) // already past
	if a.State() != StateOK {
		t.Errorf("state = %v, want OK after window passed", a.State())
	}
}

func TestExpiredTokenStartsDisabled(t *testing.T) {
	expired := time.Now().Add(-time.Hour).UnixMilli()

	// No recovery path → disabled at startup.
	a := NewAccount("a", SourceYAML, "tok", "", expired, "")
	p := New([]*Account{a}, time.Now())
	if a.State() != StateDisabled {
		t.Errorf("state = %v, want disabled", a.State())
	}
	if got := p.Select("s"); got != nil {
		t.Error("disabled account selected")
	}

	// Refresh token present → stays eligible; refresh happens at Select time.
	b := NewAccount("b", SourceYAML, "tok", "refresh", expired, "")
	New([]*Account{b}, time.Now())
	if b.State() != StateOK {
		t.Errorf("state = %v, want OK (refreshable)", b.State())
	}
}

func TestRecordQuota(t *testing.T) {
	a := &Account{Name: "a"}
	p := New([]*Account{a}, time.Now())
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Remaining", "42")
	h.Set("Content-Type", "application/json")
	p.RecordQuota(a, h, time.Now())
	q, _ := a.Quota()
	if q["anthropic-ratelimit-unified-5h-remaining"] != "42" {
		t.Errorf("quota = %v", q)
	}
	if _, ok := q["content-type"]; ok {
		t.Errorf("non-ratelimit header recorded: %v", q)
	}
}

func TestOverThresholdSkipped(t *testing.T) {
	a := &Account{Name: "a"}
	b := &Account{Name: "b"}
	p := New([]*Account{a, b}, time.Now())

	// A at 99% in its 5h window → skipped while B is eligible.
	a.SetQuotaWindows([]QuotaWindow{{Name: "5h", Limit: 1, Used: 0.99, Source: "headers", FetchedAt: time.Now()}})
	got := p.Select("s")
	if got != b {
		t.Errorf("Select = %v, want b (a over threshold)", gotName(got))
	}
}

func TestOverThresholdOnlyAccountFallsThrough(t *testing.T) {
	a := &Account{Name: "a"}
	p := New([]*Account{a}, time.Now())
	a.SetQuotaWindows([]QuotaWindow{{Name: "5h", Limit: 1, Used: 0.99, Source: "headers", FetchedAt: time.Now()}})
	got := p.Select("s")
	if got != a {
		t.Errorf("Select = %v, want a (only account, preference not ban)", gotName(got))
	}
	if a.State() != StateOK {
		t.Errorf("over-threshold must NOT mark exhausted: state = %v", a.State())
	}
}

func TestStickyUntilThreshold(t *testing.T) {
	a := &Account{Name: "a"}
	b := &Account{Name: "b"}
	p := New([]*Account{a, b}, time.Now())

	first := p.Select("s")
	p.Done(first)
	// Sticky holds while under threshold.
	second := p.Select("s")
	if second != first {
		t.Fatalf("sticky broken: %v then %v", first.Name, second.Name)
	}
	p.Done(second)

	// Push the sticky account over threshold → stickiness yields.
	first.SetQuotaWindows([]QuotaWindow{{Name: "5h", Limit: 1, Used: 1.0, Source: "headers", FetchedAt: time.Now()}})
	third := p.Select("s")
	if third == first {
		t.Errorf("sticky account still selected over threshold")
	}
}

func TestHeaderDrivenWindows(t *testing.T) {
	a := &Account{Name: "a"}
	p := New([]*Account{a}, time.Now())
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.985")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "4102444800")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.40")
	p.RecordQuota(a, h, time.Now())

	windows := a.QuotaWindows()
	if len(windows) != 2 {
		t.Fatalf("windows = %+v", windows)
	}
	var fiveH *QuotaWindow
	for i := range windows {
		if windows[i].Name == "5h" {
			fiveH = &windows[i]
		}
	}
	if fiveH == nil || fiveH.Source != "headers" || fiveH.Used != 0.985 || fiveH.ResetAt.IsZero() {
		t.Errorf("5h window = %+v", fiveH)
	}
	if !a.OverThreshold(0.98) {
		t.Error("OverThreshold(0.98) = false at 0.985 utilization")
	}
	if a.OverThreshold(0.99) {
		t.Error("OverThreshold(0.99) = true at 0.985 utilization")
	}
}

func TestPollDrivenWindows(t *testing.T) {
	a := &Account{Name: "a"}
	a.SetQuotaWindows([]QuotaWindow{{Name: "weekly", Limit: 100, Used: 99, Source: "poll", FetchedAt: time.Now()}})
	if !a.OverThreshold(0.98) {
		t.Error("poll-driven window over threshold not detected")
	}
	a.SetQuotaWindows([]QuotaWindow{{Name: "weekly", Limit: 100, Used: 50, Source: "poll", FetchedAt: time.Now()}})
	if a.OverThreshold(0.98) {
		t.Error("poll-driven window under threshold flagged")
	}
}

// Sources must not clobber each other.
func TestWindowsPerSource(t *testing.T) {
	a := &Account{Name: "a"}
	a.SetQuotaWindows([]QuotaWindow{{Name: "weekly", Limit: 100, Used: 50, Source: "poll", FetchedAt: time.Now()}})
	p := New([]*Account{a}, time.Now())
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.10")
	p.RecordQuota(a, h, time.Now())
	windows := a.QuotaWindows()
	if len(windows) != 2 {
		t.Errorf("windows after header update = %+v, want poll + headers", windows)
	}
}

func gotName(a *Account) string {
	if a == nil {
		return "<nil>"
	}
	return a.Name
}

// A freshly logged-in Kimi account must work without hand-writing a map.
// Unmapped is a hard error, so with no defaults every request from a new
// account failed until someone edited the config.
func TestKimiAccountHasWorkingDefaultsWithNoConfig(t *testing.T) {
	a := NewAccount("kimi", SourceYAML, "tok", "", 0, "")
	a.Type = "kimi-oauth"
	if a.ModelMap != nil {
		t.Fatal("precondition: this account is supposed to have no map of its own")
	}
	m := a.EffectiveModelMap()
	if len(m) == 0 {
		t.Fatal("a kimi account with no config has no model mapping at all")
	}
	// Measured from /v1/models: k3 is the only model whose context ceiling
	// (1M) is not a downgrade from what a Claude session may be carrying.
	for pattern, want := range map[string]string{
		"claude-opus-*":   "k3",
		"claude-sonnet-*": "k3",
		"claude-haiku-*":  "kimi-for-coding-highspeed",
	} {
		if m[pattern] != want {
			t.Errorf("default %q = %q, want %q", pattern, m[pattern], want)
		}
	}
	// No catch-all: an id from another vendor must still be refused rather
	// than quietly become k3.
	if _, ok := m["*"]; ok {
		t.Error("a catch-all default throws away the protection that unmapped is an error")
	}
}

// A user who maps one model must not lose the defaults for all the others,
// which is what replacing the map wholesale would do.
func TestAccountModelMapOverridesPerKeyNotWholesale(t *testing.T) {
	a := NewAccount("kimi", SourceYAML, "tok", "", 0, "")
	a.Type = "kimi-oauth"
	a.ModelMap = map[string]string{"claude-haiku-*": "k3-256k"}

	m := a.EffectiveModelMap()
	if m["claude-haiku-*"] != "k3-256k" {
		t.Errorf("account override lost: %q", m["claude-haiku-*"])
	}
	if m["claude-sonnet-*"] != "k3" {
		t.Errorf("overriding one model dropped the default for another: %q", m["claude-sonnet-*"])
	}
}

// Claude speaks its own ids, so there is nothing to map and no rewrite should
// happen at all.
func TestClaudeAccountHasNoModelMap(t *testing.T) {
	a := NewAccount("claude", SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	if m := a.EffectiveModelMap(); len(m) != 0 {
		t.Errorf("claude account carries a model map (%v); rewriting its own ids "+
			"would be a §4 mutation for no reason", m)
	}
}

// Repeated polls must replace an account's windows, not accumulate them.
//
// Found live: a Kimi account had 105 windows — weekly/5h/parallel, 35 copies
// each — because the poller set no Source, so the replace-by-source filter
// compared "poll" against "" and matched nothing. It grows without bound, and
// OverThreshold scans every window, so one stale copy below the threshold
// would pin the account out of rotation for good.
func TestPollingReplacesWindowsRatherThanAppending(t *testing.T) {
	a := NewAccount("kimi", SourceYAML, "tok", "", 0, "")
	a.Type = "kimi-oauth"

	for i := 0; i < 5; i++ {
		a.SetQuotaWindows([]QuotaWindow{
			{Name: "weekly", Limit: 1, Used: 0.5, FetchedAt: time.Now()},
			{Name: "5h", Limit: 1, Used: float64(i) / 10, FetchedAt: time.Now()},
		})
	}
	got := a.QuotaWindows()
	if len(got) != 2 {
		names := make([]string, len(got))
		for i, w := range got {
			names[i] = w.Name
		}
		t.Fatalf("after 5 polls the account has %d windows, want 2: %v", len(got), names)
	}
	// And the newest values win.
	for _, w := range got {
		if w.Name == "5h" && w.Used != 0.4 {
			t.Errorf("5h used = %v, want the last poll's 0.4", w.Used)
		}
	}
}

// Header-sourced and poll-sourced windows coexist: replacing one must not
// disturb the other. That separation is the reason the filter exists.
func TestWindowSourcesReplaceIndependently(t *testing.T) {
	a := NewAccount("mixed", SourceYAML, "tok", "", 0, "")
	a.setWindowsSourced("headers", []QuotaWindow{{Name: "5h", Limit: 1, Used: 0.2}})
	a.SetQuotaWindows([]QuotaWindow{{Name: "weekly", Limit: 1, Used: 0.3}})

	if got := len(a.QuotaWindows()); got != 2 {
		t.Fatalf("got %d windows, want one per source", got)
	}
	// Re-poll: the header window must survive.
	a.SetQuotaWindows([]QuotaWindow{{Name: "weekly", Limit: 1, Used: 0.9}})
	got := a.QuotaWindows()
	if len(got) != 2 {
		t.Fatalf("re-polling changed the window count to %d", len(got))
	}
	for _, w := range got {
		if w.Name == "5h" && w.Used != 0.2 {
			t.Errorf("polling clobbered the header-sourced window: %v", w)
		}
		if w.Name == "weekly" && w.Used != 0.9 {
			t.Errorf("weekly = %v, want the newest poll", w.Used)
		}
	}
}
