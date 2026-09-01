package pool

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestAddSignalsCapacity is the pool-level half of issue #105's reported
// bug: Add must wake anything reading CapacitySignal, not merely make the
// account selectable (that half is TestAddMakesAccountSelectable, #87).
func TestAddSignalsCapacity(t *testing.T) {
	p := New([]*Account{NewAccount("a", SourceYAML, "tok", "", 0, "")}, time.Now())
	wake := p.CapacitySignal()

	select {
	case <-wake.Ch():
		t.Fatal("capacity signal fired before Add was ever called")
	default:
	}

	p.Add(NewAccount("b", SourceYAML, "tok2", "", 0, ""))

	select {
	case <-wake.Ch():
	case <-time.After(time.Second):
		t.Fatal("Add did not close the capacity-signal channel")
	}
}

// TestClearExhaustedSignalsCapacity covers issue #105's other named
// trigger: a re-probe (issue #90) clearing an account's exhaustion early
// must be visible to anything waiting on CapacitySignal, the same as Add.
func TestClearExhaustedSignalsCapacity(t *testing.T) {
	a := NewAccount("a", SourceYAML, "tok", "", 0, "")
	p := New([]*Account{a}, time.Now())
	p.MarkExhausted(a, time.Now().Add(time.Hour))

	wake := p.CapacitySignal()
	select {
	case <-wake.Ch():
		t.Fatal("capacity signal fired before ClearExhausted was ever called")
	default:
	}

	p.ClearExhausted(a)

	select {
	case <-wake.Ch():
	case <-time.After(time.Second):
		t.Fatal("ClearExhausted did not close the capacity-signal channel")
	}
}

// TestClearExhaustedNoopDoesNotSignal: ClearExhausted on an account that was
// never exhausted (or already cleared) is a documented no-op — it must not
// fire the signal either, or every idle sweep that happens to call it on a
// healthy account would count as "capacity changed".
func TestClearExhaustedNoopDoesNotSignal(t *testing.T) {
	a := NewAccount("a", SourceYAML, "tok", "", 0, "") // never exhausted
	p := New([]*Account{a}, time.Now())
	wake := p.CapacitySignal()

	p.ClearExhausted(a)

	select {
	case <-wake.Ch():
		t.Fatal("ClearExhausted signalled capacity for an account that was never exhausted")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestQuotaWritesDoNotSignalCapacity is the "no busy-wake" design
// constraint from issue #105: a signal firing on every quota-header update
// would wake every held request constantly for nothing. RecordQuota,
// MarkExhausted, and MarkWindowRejected all write account state on every
// ordinary response — none of them may close the capacity channel.
func TestQuotaWritesDoNotSignalCapacity(t *testing.T) {
	a := NewAccount("a", SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	p := New([]*Account{a}, time.Now())
	wake := p.CapacitySignal()

	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Remaining", "10")
	h.Set("Anthropic-Ratelimit-Unified-5h-Limit", "100")
	p.RecordQuota(a, h, time.Now())
	p.MarkExhausted(a, time.Now().Add(time.Hour))
	p.MarkWindowRejected(a, "7d-fable", time.Now().Add(time.Hour))

	select {
	case <-wake.Ch():
		t.Fatal("an ordinary quota write closed the capacity-signal channel — busy-wake")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestCapacityWakeRankIsSequential: many concurrent callers ranking the
// same generation must each get a distinct, sequential position — the
// mechanism the proxy's hold uses to stagger re-selection instead of every
// held request firing in lockstep (issue #105's anti-thunder measure).
func TestCapacityWakeRankIsSequential(t *testing.T) {
	p := New([]*Account{NewAccount("a", SourceYAML, "tok", "", 0, "")}, time.Now())
	wake := p.CapacitySignal()

	const n = 50
	ranks := make([]int64, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			ranks[i] = wake.Rank()
		}(i)
	}
	wg.Wait()

	seen := make(map[int64]bool, n)
	for _, r := range ranks {
		if r < 0 || r >= n {
			t.Fatalf("rank %d out of expected range [0,%d)", r, n)
		}
		if seen[r] {
			t.Fatalf("rank %d handed out twice", r)
		}
		seen[r] = true
	}
}

// TestCapacitySignalConcurrentWaitersAndMutators races many goroutines
// reading CapacitySignal/Rank against many goroutines mutating the pool
// (Add, Remove, MarkExhausted, ClearExhausted) — internal/pool is
// concurrent, and issue #105 explicitly calls for this to run clean under
// -race, not just pass sequentially.
func TestCapacitySignalConcurrentWaitersAndMutators(t *testing.T) {
	seed := make([]*Account, 0, 8)
	for i := range 8 {
		seed = append(seed, NewAccount(fmt.Sprintf("seed-%d", i), SourceYAML, "tok", "", 0, ""))
	}
	p := New(seed, time.Now())

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Waiters: repeatedly grab the current generation, wait briefly on it
	// or time out, and rank it if it fires.
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				wake := p.CapacitySignal()
				select {
				case <-wake.Ch():
					wake.Rank()
				case <-time.After(5 * time.Millisecond):
				}
			}
		}()
	}

	// Mutators: Add/Remove/MarkExhausted/ClearExhausted concurrently.
	for i := range 6 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("mutator-%d", i)
			for j := range 40 {
				a := NewAccount(name, SourceYAML, "tok", "", 0, "")
				p.Add(a)
				p.MarkExhausted(a, time.Now().Add(time.Millisecond))
				if j%2 == 0 {
					p.ClearExhausted(a)
				}
				p.Remove(name)
			}
		}(i)
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
