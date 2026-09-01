package pool

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/provider"
)

// TestDashboardSettingsRaceWithSelector is the regression test for issue
// #13: the dashboard settings closure in cmd/spillway/main.go used to assign
// SwitchThreshold, CrossProvider, AllowOverage (pool-wide) and Label,
// Priority, AllowOverage (per-account) as plain struct field writes from the
// admin HTTP goroutine, while the selector and the dashboard's own read path
// read every one of them concurrently. Apply (and the locked
// getters/setters) are now the only writers/readers, and this test drives
// enough concurrent traffic to actually exercise each one — not just the
// pool-wide fields, which the sticky-session fast path in SelectExcept
// covers on the very first call regardless of locking.
//
// Three things had to be true for this test to be worth anything, and an
// earlier version got none of the account-level ones right:
//
//   - A varying session key per selection, so SelectExcept's sticky fast
//     path (`if name, ok := p.sticky[session]; ok { ... return a }`) doesn't
//     short-circuit into leastLoaded/better after the first call.
//   - At least two eligible candidates with different priorities, so
//     leastLoaded actually calls better(), which is the only reader of
//     Account.Priority.
//   - At least one exhausted-but-overage-eligible account, so SelectExcept's
//     third tier actually calls CanOverage, which reads Account.allowOverage
//     — the money switch the issue is named for. The first two tiers never
//     reach it: tier one requires State==StateOK, tier two explicitly
//     excludes accounts CanOverage would bill, so overage is only ever
//     consulted once every free/reserve account has failed.
//
// Two pools are used because the tiers are mutually exclusive within one
// SelectExcept call: if tier one finds any eligible candidate anywhere in
// the pool, tiers two and three never run, so a pool mixing "eligible,
// priority-differentiated" accounts with "exhausted, overage-eligible" ones
// would never actually reach CanOverage.
func TestDashboardSettingsRaceWithSelector(t *testing.T) {
	t.Run("priority via better() on the non-sticky path", func(t *testing.T) {
		a := NewAccount("pa", SourceYAML, "tok", "", 0, "")
		a.Type = "claude-oauth"
		b := NewAccount("pb", SourceYAML, "tok2", "", 0, "")
		b.Type = "claude-oauth"
		p := New([]*Account{a, b}, time.Now())

		var stop atomic.Bool
		var sessionN atomic.Int64
		var wg sync.WaitGroup
		wg.Add(3)

		// Selector: a fresh session per call defeats the sticky fast path,
		// forcing leastLoaded (and therefore better(), and therefore
		// Account.Priority) to run every time.
		go func() {
			defer wg.Done()
			for !stop.Load() {
				sess := fmt.Sprintf("sess-%d", sessionN.Add(1))
				if acct := p.SelectFor(sess, nil); acct != nil {
					p.Done(acct)
				}
			}
		}()

		// Direct concurrent reader of Label/Priority: Label has no reader
		// inside this package (only internal/admin's dashboard JSON calls
		// it), so exercising it via SelectFor alone would prove nothing.
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_ = a.Label()
				_ = b.Label()
				_ = a.Priority()
				_ = b.Priority()
				// The proxy reads this per response (issue #103), so it
				// races with Apply exactly like the fields above.
				_ = p.HideOverageFromClient()
			}
		}()

		go func() {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				p.Apply(Settings{
					SwitchThreshold:       0.9,
					CrossProvider:         true,
					HideOverageFromClient: i%2 == 0,
					Accounts: []AccountSettings{
						{Name: "pa", Label: fmt.Sprintf("L%d", i), Priority: i % 3},
						{Name: "pb", Label: fmt.Sprintf("M%d", i), Priority: (i + 1) % 3},
					},
				})
			}
			stop.Store(true)
		}()

		wg.Wait()
	})

	t.Run("allowOverage via CanOverage on the exhausted tier", func(t *testing.T) {
		now := time.Now()
		c := NewAccount("pc", SourceYAML, "tok", "", 0, "")
		c.Type = "claude-oauth"
		d := NewAccount("pd", SourceYAML, "tok2", "", 0, "")
		d.Type = "claude-oauth"
		p := New([]*Account{c, d}, now)
		// Both accounts exhausted with the provider confirming overage is
		// available: tiers one and two require State==StateOK, so both fail
		// for both accounts and tier three's CanOverage(a) actually runs.
		p.MarkExhausted(c, now.Add(time.Hour))
		p.MarkExhausted(d, now.Add(time.Hour))
		c.SetOverageForTest(provider.Overage{Known: true, Available: true})
		d.SetOverageForTest(provider.Overage{Known: true, Available: true})
		p.Apply(Settings{AllowOverage: true})

		var stop atomic.Bool
		var sessionN atomic.Int64
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			for !stop.Load() {
				sess := fmt.Sprintf("sess-%d", sessionN.Add(1))
				// The result may legitimately be nil or non-nil depending on
				// the in-flight AllowOverage value; either way, leastLoaded
				// evaluates CanOverage(c) and CanOverage(d) on every call.
				if acct := p.SelectFor(sess, nil); acct != nil {
					p.Done(acct)
				}
			}
		}()

		go func() {
			defer wg.Done()
			trueVal, falseVal := true, false
			for i := 0; i < 5000; i++ {
				var v *bool
				if i%2 == 0 {
					v = &trueVal
				} else {
					v = &falseVal
				}
				p.Apply(Settings{
					AllowOverage: true,
					Accounts: []AccountSettings{
						{Name: "pc", AllowOverage: v},
						{Name: "pd", AllowOverage: v},
					},
				})
			}
			stop.Store(true)
		}()

		wg.Wait()
	})
}
