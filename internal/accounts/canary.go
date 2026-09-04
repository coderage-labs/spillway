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
	poolAllows := c.Pool.AllowOverage()
	for _, a := range c.Pool.Accounts() {
		if a.State() == pool.StateDisabled || a.Parked() {
			continue
		}
		if !c.idleEnough(a) {
			continue
		}
		// A canary must never cost money. On an account with extra usage
		// available and its quota spent, this "free health check" is a
		// purchase — repeated on every sweep. Where extra usage is NOT
		// permitted the same account refuses the canary for free (issue
		// #152), and a 429 answers the only question a canary asks just as
		// well as a 200 does: the credential authenticated.
		if wouldBill(a, poolAllows, time.Now()) {
			continue
		}
		// The canary reuses probeOne, which now also re-verifies an already
		// -exhausted account (issue #90) — so it must honour the same
		// growing backoff a rejected re-probe sets, rather than bypassing it
		// on its own separate schedule and re-creating the tight loop that
		// backoff exists to prevent.
		if next := a.NextProbeAt(); !next.IsZero() && time.Now().Before(next) {
			continue
		}
		res := CanaryResult{Account: a.Name}
		err := probeOne(ctx, c.Pool, a, c.Client, c.Upstream, c.reprobeBaseInterval())
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

// reprobeBaseInterval is the backoff base a rejected re-probe grows from
// when it arrives via the canary rather than the ordinary probe sweep
// (issue #90). Idle is the canary's own natural cadence; a zero Idle means
// "check every sweep" and carries no usable duration, so this falls back to
// the probe's own 30m default rather than handing MarkReprobeRejected a
// zero baseInterval, which would never grow past zero.
func (c *Canary) reprobeBaseInterval() time.Duration {
	if c.Idle > 0 {
		return c.Idle
	}
	return 30 * time.Minute
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
