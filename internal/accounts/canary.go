package accounts

// Live canary (design doc §6.8, decision (b)).
//
// The probe fills quota for an idle account. This is the other half: it
// watches for the failures that arrive without any request of yours failing
// first — a refresh token quietly revoked, an account downgraded, an upstream
// that has started rejecting the client shape. Those surface as an unexpected
// 401/403 on a request you did not make, hours before you would otherwise
// notice, and the alternative is discovering it mid-session.
//
// It reuses the probe's request rather than inventing traffic of its own, so
// the cost is the same one small call, and only for accounts that have not
// been exercised recently.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

// CanaryResult is one account's health check.
type CanaryResult struct {
	Account string
	Err     error
	// Alarm is true for failures that mean the account is not usable —
	// distinct from a transient network blip, which says nothing.
	Alarm bool
}

// Canary checks accounts that have not served recently. Accounts in active
// use need no canary: their real traffic is the check.
type Canary struct {
	Pool     *pool.Pool
	Client   *http.Client
	Upstream string
	Logger   *slog.Logger
	Events   *events.Broker
	// Idle is how long an account must have gone unused before it is worth
	// checking.
	Idle time.Duration
}

// Run checks every eligible idle account once.
func (c *Canary) Run(ctx context.Context) []CanaryResult {
	var out []CanaryResult
	for _, a := range c.Pool.Accounts() {
		if a.State() == pool.StateDisabled || a.Parked() {
			continue
		}
		if !c.idleEnough(a) {
			continue
		}
		// A canary must never cost money. On an account with extra usage
		// available and its quota spent, this "free health check" is a
		// purchase — repeated on every sweep.
		if wouldBill(a, time.Now()) {
			continue
		}
		res := CanaryResult{Account: a.Name}
		err := probeOne(ctx, c.Pool, a, c.Client, c.Upstream)
		switch {
		case err == nil:
			// Healthy; nothing to say.
		case isAuthFailure(err):
			// The credential is gone. This is the case worth waking someone
			// for: nothing of theirs failed, and nothing will until they try.
			res.Err, res.Alarm = err, true
		default:
			// A transient failure says nothing about the account.
			res.Err = err
		}
		if res.Err != nil {
			c.report(res)
		}
		out = append(out, res)
	}
	return out
}

func (c *Canary) idleEnough(a *pool.Account) bool {
	if c.Idle <= 0 {
		return true
	}
	if a.InFlight() > 0 {
		return false
	}
	wins := a.QuotaWindows()
	newest := time.Time{}
	for _, w := range wins {
		if w.FetchedAt.After(newest) {
			newest = w.FetchedAt
		}
	}
	if newest.IsZero() {
		return true
	}
	return time.Since(newest) > c.Idle
}

func (c *Canary) report(res CanaryResult) {
	if c.Logger != nil {
		if res.Alarm {
			c.Logger.Error("canary: account is not usable — re-login required",
				"account", res.Account, "err", res.Err)
		} else {
			c.Logger.Warn("canary: check failed (may be transient)",
				"account", res.Account, "err", res.Err)
		}
	}
	if res.Alarm && c.Events != nil {
		c.Events.Publish(events.Event{
			Type: "canary", Account: res.Account,
			Detail: "account is not usable: " + res.Err.Error(),
		})
	}
}

// isAuthFailure reports a credential problem rather than a transient one.
func isAuthFailure(err error) bool {
	return errors.Is(err, errProbeUnauthorized)
}
