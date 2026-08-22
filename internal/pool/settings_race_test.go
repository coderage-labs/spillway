package pool

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDashboardSettingsRaceWithSelector is the regression test for issue
// #13: the dashboard settings closure in cmd/spillway/main.go used to assign
// SwitchThreshold, CrossProvider, AllowOverage (pool-wide) and Label,
// Priority, AllowOverage (per-account) as plain struct field writes from the
// admin HTTP goroutine, while the selector (SelectExcept, better, CanOverage)
// reads every one of them under mu from whatever goroutine is picking an
// account for a request. One goroutine here drives selection in a tight
// loop; the other applies settings the way the dashboard does now, via
// Apply. Confirmed (before Apply/the mu-guarded fields existed) that this
// exact concurrent shape reports WARNING: DATA RACE within the first few
// iterations, at pool.go's threshold()/CrossProvider reads and better()'s
// Priority read — see the fix commit for the captured output. Run with
// -race: must stay clean now that Apply and the locked accessors are the
// only writers/readers.
func TestDashboardSettingsRaceWithSelector(t *testing.T) {
	a := NewAccount("acct", SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	b := NewAccount("acct2", SourceYAML, "tok2", "", 0, "")
	b.Type = "claude-oauth"
	p := New([]*Account{a, b}, time.Now())

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for !stop.Load() {
			if acct := p.SelectFor("sess", nil); acct != nil {
				p.Done(acct)
			}
		}
	}()

	go func() {
		defer wg.Done()
		allow := true
		for i := 0; i < 5000; i++ {
			// Mirrors what the dashboard settings handler does now
			// (cmd/spillway/main.go's poolSettings + p.Apply): the pool-wide
			// money switch and per-account overrides go through Apply
			// instead of direct field writes.
			p.Apply(Settings{
				SwitchThreshold: 0.9,
				CrossProvider:   true,
				AllowOverage:    true,
				Accounts: []AccountSettings{
					{Name: "acct", Label: "renamed", Priority: 1, AllowOverage: &allow},
					{Name: "acct2", Priority: 2},
				},
			})
		}
		stop.Store(true)
	}()

	wg.Wait()
}
