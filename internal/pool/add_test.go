package pool

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAddMakesAccountSelectable is the regression test for issue #87: an
// account added to a running pool must be reachable by the selector on the
// very next call, not merely present in a length count. Mirrors
// TestRemoveMakesAccountUnselectable's shape (#83) in the opposite
// direction.
func TestAddMakesAccountSelectable(t *testing.T) {
	existing := NewAccount("existing", SourceYAML, "tok", "", 0, "")
	existing.Type = "claude-oauth"
	p := New([]*Account{existing}, time.Now())

	fresh := NewAccount("fresh", SourceYAML, "tok2", "", 0, "")
	fresh.Type = "claude-oauth"
	if ok := p.Add(fresh); !ok {
		t.Fatal("Add reported the account was not actually added")
	}

	// leastLoaded's tie-break is deterministic (first match in p.accounts
	// wins equal priority/load — see better()), so a plain round of fresh
	// sessions would always land on "existing" regardless of whether Add
	// worked, proving nothing. Pin to "fresh" instead, exactly the way
	// TestRemoveDrainsRatherThanAborts (#83) proves an account is reachable
	// — Pin fails outright for a name the selector's find() cannot resolve,
	// so a successful pinned selection is direct evidence the added account
	// is live in p.accounts, not an artifact of tie-breaking.
	if err := p.Pin("fresh", false); err != nil {
		t.Fatalf("Pin(%q) failed — the added account is not resolvable: %v", "fresh", err)
	}
	got := p.SelectFor("pinned-session", nil)
	if got == nil || got.Name != "fresh" {
		t.Fatalf("SelectFor while pinned to the added account = %v, want fresh", got)
	}
	p.Done(got)
}

// TestAddDuplicateNameIsFalse: Add must not create two entries answering to
// the same name — selection, Remove-by-name and Pin all assume names are
// unique.
func TestAddDuplicateNameIsFalse(t *testing.T) {
	a := NewAccount("dup", SourceYAML, "tok", "", 0, "")
	p := New([]*Account{a}, time.Now())

	b := NewAccount("dup", SourceYAML, "tok2", "", 0, "")
	if ok := p.Add(b); ok {
		t.Error("Add of a name already present reported true")
	}
	if len(p.Accounts()) != 1 {
		t.Fatalf("pool has %d accounts, want 1 (duplicate must not have been appended)", len(p.Accounts()))
	}
}

// TestAddThenRemoveRoundTrips: an added account can be removed again by the
// same name, same as any account present from construction.
func TestAddThenRemoveRoundTrips(t *testing.T) {
	p := New(nil, time.Now())
	a := NewAccount("solo", SourceYAML, "tok", "", 0, "")
	if ok := p.Add(a); !ok {
		t.Fatal("Add reported false for a brand new name")
	}
	if len(p.Accounts()) != 1 {
		t.Fatalf("pool has %d accounts, want 1", len(p.Accounts()))
	}
	if ok := p.Remove("solo"); !ok {
		t.Fatal("Remove reported the just-added account was not present")
	}
	if len(p.Accounts()) != 0 {
		t.Fatalf("pool has %d accounts after remove, want 0", len(p.Accounts()))
	}
}

// TestAddConcurrentWithSelect is the race-detector regression for #87,
// mirroring TestRemoveConcurrentWithSelect (#83): Add now mutates
// p.accounts at runtime, concurrently with the selector and with plain
// readers (Accounts/EarliestReset) that range over the same slice. Run with
// -race.
func TestAddConcurrentWithSelect(t *testing.T) {
	seed := NewAccount("seed", SourceYAML, "tok", "", 0, "")
	seed.Type = "claude-oauth"
	p := New([]*Account{seed}, time.Now())

	var stop atomic.Bool
	var sessionN atomic.Int64
	var addN atomic.Int64
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

	// A second reader that walks the pool the way admin/proxy code does.
	go func() {
		defer wg.Done()
		for !stop.Load() {
			for _, a := range p.Accounts() {
				_ = a.Name
			}
			_, _ = p.EarliestReset()
		}
	}()

	// Adder: keeps adding brand-new names concurrently with selection and
	// reads.
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			n := addN.Add(1)
			a := NewAccount(fmt.Sprintf("added-%d", n), SourceYAML, "tok", "", 0, "")
			a.Type = "claude-oauth"
			p.Add(a)
		}
		stop.Store(true)
	}()

	wg.Wait()
}
