package pool

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRemoveMakesAccountUnselectable is the regression test for issue #83:
// `accounts remove` used to edit the yaml and stop, leaving the removed
// account reachable by the running pool's selector — served by a credential
// that had just been deleted from the keychain. The bug is precisely that an
// account can be absent from a list (e.g. what `/api/accounts` would report
// once the CLI fixes the config) while still being selectable, so this
// asserts against selection directly rather than against Accounts()'s
// length.
func TestRemoveMakesAccountUnselectable(t *testing.T) {
	a := NewAccount("gone", SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	b := NewAccount("stays", SourceYAML, "tok2", "", 0, "")
	b.Type = "claude-oauth"
	p := New([]*Account{a, b}, time.Now())

	if ok := p.Remove("gone"); !ok {
		t.Fatal("Remove reported the account was not present")
	}

	// Drive enough fresh sessions that a selector bug (e.g. Remove not
	// actually taking effect on the tier the selector reaches) has a real
	// chance to surface "gone" rather than hiding behind sticky affinity.
	for i := 0; i < 20; i++ {
		got := p.SelectFor(fmt.Sprintf("session-%d", i), nil)
		if got == nil {
			t.Fatal("nothing selected — the surviving account should still serve")
		}
		if got.Name == "gone" {
			t.Fatalf("selected the removed account %q — it must not be reachable", got.Name)
		}
		p.Done(got)
	}
}

// TestRemoveUnknownAccountIsFalse: Remove reports whether the name was
// actually present, so a caller (the admin endpoint) can tell "removed" from
// "was already gone" without a second lookup.
func TestRemoveUnknownAccountIsFalse(t *testing.T) {
	a := NewAccount("only", SourceYAML, "tok", "", 0, "")
	p := New([]*Account{a}, time.Now())
	if ok := p.Remove("nope"); ok {
		t.Error("Remove of an unknown name reported true")
	}
	if ok := p.Remove("only"); !ok {
		t.Fatal("Remove of the real account reported false")
	}
	if ok := p.Remove("only"); ok {
		t.Error("removing the same name twice reported true the second time")
	}
}

// TestRemoveDrainsRatherThanAborts documents and pins the in-flight decision
// (issue #83): a request already selected onto an account keeps the
// *Account pointer it was handed, not a name to re-resolve, so removing the
// account from the pool does not disturb it. Done still works, in-flight
// accounting still works, and nothing panics or blocks — the request drains
// to completion exactly as if nothing had happened, while a brand new
// selection made in the same instant never sees the removed account again.
func TestRemoveDrainsRatherThanAborts(t *testing.T) {
	a := NewAccount("draining", SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	b := NewAccount("fresh", SourceYAML, "tok2", "", 0, "")
	b.Type = "claude-oauth"
	p := New([]*Account{a, b}, time.Now())

	// Pin so the in-flight request is guaranteed to land on "draining"
	// rather than depending on tie-breaking.
	if err := p.Pin("draining", false); err != nil {
		t.Fatal(err)
	}
	inFlight := p.SelectFor("live-session", nil)
	if inFlight == nil || inFlight.Name != "draining" {
		t.Fatalf("expected to select draining, got %+v", inFlight)
	}
	p.Unpin()

	if !p.Remove("draining") {
		t.Fatal("Remove reported the account was not present")
	}

	// The in-flight request's own view of the account: still there, still
	// accepting Done, in-flight count still accurate.
	if got := inFlight.InFlight(); got != 1 {
		t.Fatalf("removed account's in-flight count = %d, want 1 (the request still in progress)", got)
	}
	p.Done(inFlight) // must not panic — the account is drained, not yanked
	if got := inFlight.InFlight(); got != 0 {
		t.Fatalf("after Done, in-flight = %d, want 0", got)
	}

	// A brand new selection must never reach the removed account, pinned or
	// not — pinning a removed name should behave as "no such account", the
	// same as pinning a name that was never real.
	if err := p.Pin("draining", false); err == nil {
		t.Fatal("pinning a removed account name should fail; Remove must clear a stale pin")
	}
	for i := 0; i < 10; i++ {
		got := p.SelectFor(fmt.Sprintf("new-session-%d", i), nil)
		if got != nil && got.Name == "draining" {
			t.Fatalf("new selection reached the removed account")
		}
	}
}

// TestRemoveConcurrentWithSelect is the race-detector regression for #83:
// SelectExcept/leastLoaded/WhyUnavailable/Accounts all range over
// p.accounts, and Remove now mutates that slice at runtime — something
// nothing in this package did before. Run with -race.
func TestRemoveConcurrentWithSelect(t *testing.T) {
	accts := make([]*Account, 0, 8)
	for i := 0; i < 8; i++ {
		a := NewAccount(fmt.Sprintf("acct-%d", i), SourceYAML, "tok", "", 0, "")
		a.Type = "claude-oauth"
		accts = append(accts, a)
	}
	p := New(accts, time.Now())

	var stop atomic.Bool
	var sessionN atomic.Int64
	var wg sync.WaitGroup
	wg.Add(3)

	// Selector, hammering fresh sessions so it actually walks p.accounts
	// instead of hitting the sticky fast path.
	go func() {
		defer wg.Done()
		for !stop.Load() {
			sess := fmt.Sprintf("sess-%d", sessionN.Add(1))
			if acct := p.SelectFor(sess, nil); acct != nil {
				p.Done(acct)
			}
		}
	}()

	// A second reader that walks the pool the way admin/proxy code does —
	// Accounts() and EarliestReset() were unlocked reads of the same slice
	// before this issue and would race a concurrent Remove immediately.
	go func() {
		defer wg.Done()
		for !stop.Load() {
			for _, a := range p.Accounts() {
				_ = a.Name
			}
			_, _ = p.EarliestReset()
		}
	}()

	// Remover: repeatedly removes and re-adds nothing (Remove is
	// idempotent-safe on an absent name), concentrating the race window on
	// the two accounts most likely to be mid-selection.
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			p.Remove(fmt.Sprintf("acct-%d", i%8))
		}
		stop.Store(true)
	}()

	wg.Wait()
}
