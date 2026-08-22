package pool

import (
	"testing"
	"time"
)

func kimiAccount(name string) *Account {
	a := NewAccount(name, SourceYAML, "tok", "", 0, "")
	a.Type = "kimi-oauth"
	return a
}

func claudeAccount(name string) *Account {
	a := NewAccount(name, SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	return a
}

// The measured interaction (doc §6.19): Kimi reasons by default, and a forced
// tool_choice is rejected while it does — so a Claude Code flow that forces a
// tool fails on Kimi with no configuration at all.
func TestCanServeForcedToolChoiceOnKimi(t *testing.T) {
	forced := []byte(`{"model":"k3","tool_choice":{"type":"tool","name":"x"},"messages":[]}`)

	if err := CanServe(kimiAccount("kimi"), forced); err == nil {
		t.Error("forced tool_choice must be rejected on kimi while thinking defaults on")
	}
	// Explicitly disabling thinking is the documented escape hatch.
	withThinkingOff := []byte(`{"model":"k3","tool_choice":{"type":"tool","name":"x"},
		"thinking":{"type":"disabled"},"messages":[]}`)
	if err := CanServe(kimiAccount("kimi"), withThinkingOff); err != nil {
		t.Errorf("forced tool_choice with thinking disabled should be servable: %v", err)
	}
	// Anthropic supports both together.
	if err := CanServe(claudeAccount("work"), forced); err != nil {
		t.Errorf("forced tool_choice must be fine on claude: %v", err)
	}
}

func TestCanServeAllowsUnforcedToolChoice(t *testing.T) {
	for _, tc := range []string{"auto", "any"} {
		body := []byte(`{"model":"k3","tool_choice":{"type":"` + tc + `"},"messages":[]}`)
		if err := CanServe(kimiAccount("kimi"), body); err != nil {
			t.Errorf("tool_choice=%s should be servable on kimi: %v", tc, err)
		}
	}
}

// Only positively measured incompatibilities are checked — guessing would
// route around problems that do not exist.
func TestCanServeIgnoresUnknownAndMalformedBodies(t *testing.T) {
	for _, body := range [][]byte{nil, {}, []byte("not json"), []byte(`{"messages":[]}`)} {
		if err := CanServe(kimiAccount("kimi"), body); err != nil {
			t.Errorf("CanServe(%q) = %v, want nil", body, err)
		}
	}
}

func TestProviderOf(t *testing.T) {
	for in, want := range map[string]string{
		"claude-oauth": "claude", "kimi-oauth": "kimi", "": "claude",
	} {
		if got := ProviderOf(in); got != want {
			t.Errorf("ProviderOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// A session stays on the provider it started with, so the capabilities the
// client fixed at startup keep holding (§6.18).
func TestSelectPinsSessionToItsProvider(t *testing.T) {
	claude, kimi := claudeAccount("work"), kimiAccount("kimi")
	p := New([]*Account{claude, kimi}, time.Now())

	first := p.SelectFor("sess", nil)
	if first == nil {
		t.Fatal("no account selected")
	}
	p.Done(first)
	wantProvider := ProviderOf(first.Type)

	// Take the first account out of the running; without pinning the pool
	// would happily rotate to the other provider.
	p.MarkExhausted(first, time.Now().Add(time.Hour))

	next := p.SelectFor("sess", nil)
	if next != nil && ProviderOf(next.Type) != wantProvider {
		t.Errorf("session rotated from %q to %q with CrossProvider off",
			wantProvider, ProviderOf(next.Type))
	}
}

func TestCrossProviderAllowsRotationWhenEnabled(t *testing.T) {
	claude, kimi := claudeAccount("work"), kimiAccount("kimi")
	p := New([]*Account{claude, kimi}, time.Now())
	p.crossProvider = true

	first := p.SelectFor("sess", nil)
	p.Done(first)
	p.MarkExhausted(first, time.Now().Add(time.Hour))

	next := p.SelectFor("sess", nil)
	if next == nil {
		t.Fatal("CrossProvider should have allowed the other provider")
	}
	if next.Name == first.Name {
		t.Error("expected rotation to the other account")
	}
}

// A request no account can serve must be distinguishable from an exhausted
// pool: it is not a rate limit, and reporting it as one sends the client into
// a pointless backoff.
func TestWhyUnavailableNamesTheCapability(t *testing.T) {
	p := New([]*Account{kimiAccount("kimi")}, time.Now())
	forced := []byte(`{"model":"k3","tool_choice":{"type":"tool","name":"x"},"messages":[]}`)

	if got := p.SelectFor("s", forced); got != nil {
		t.Fatalf("kimi cannot serve a forced tool call, got %q", got.Name)
	}
	err := p.WhyUnavailable(forced)
	if err == nil {
		t.Fatal("expected a capability explanation")
	}
	var inc *Incompatibility
	if !asIncompatibility(err, &inc) {
		t.Fatalf("want *Incompatibility, got %T", err)
	}
	if inc.Account != "kimi" {
		t.Errorf("explanation should name the account, got %q", inc.Account)
	}
}

// A genuinely exhausted pool must NOT be reported as a capability problem.
func TestWhyUnavailableSilentWhenPoolMerelyExhausted(t *testing.T) {
	a := claudeAccount("work")
	p := New([]*Account{a}, time.Now())
	p.MarkExhausted(a, time.Now().Add(time.Hour))
	if err := p.WhyUnavailable([]byte(`{"model":"claude-sonnet-4-6","messages":[]}`)); err != nil {
		t.Errorf("exhaustion is not a capability failure, got %v", err)
	}
}

func asIncompatibility(err error, target **Incompatibility) bool {
	inc, ok := err.(*Incompatibility)
	if ok {
		*target = inc
	}
	return ok
}

// Priority orders selection ahead of load, so a reserve account stays unused
// while a preferred one can serve.
func TestPriorityPrefersLowerNumber(t *testing.T) {
	reserve := claudeAccount("reserve")
	reserve.priority = 100
	primary := claudeAccount("primary")
	primary.priority = 0
	p := New([]*Account{reserve, primary}, time.Now())

	got := p.SelectFor("s1", nil)
	if got == nil || got.Name != "primary" {
		t.Fatalf("selected %v, want primary", got)
	}
}

// It is a preference, not a queue: an exhausted preferred account must not
// block a lower-priority one.
func TestPriorityFallsThroughWhenPreferredIsSpent(t *testing.T) {
	reserve := claudeAccount("reserve")
	reserve.priority = 100
	primary := claudeAccount("primary")
	primary.priority = 0
	p := New([]*Account{reserve, primary}, time.Now())
	p.MarkExhausted(primary, time.Now().Add(time.Hour))

	got := p.SelectFor("s1", nil)
	if got == nil || got.Name != "reserve" {
		t.Fatalf("selected %v, want reserve once primary is spent", got)
	}
}

// Equal priority keeps the previous behaviour: fewest in-flight wins.
func TestEqualPriorityFallsBackToLeastLoaded(t *testing.T) {
	busy := claudeAccount("busy")
	idle := claudeAccount("idle")
	p := New([]*Account{busy, idle}, time.Now())

	first := p.SelectFor("s1", nil) // takes one of them, +1 in flight
	if first == nil {
		t.Fatal("nothing selected")
	}
	second := p.SelectFor("s2", nil)
	if second == nil {
		t.Fatal("nothing selected for the second session")
	}
	if second.Name == first.Name {
		t.Errorf("both sessions landed on %q; load should have spread them", first.Name)
	}
}
