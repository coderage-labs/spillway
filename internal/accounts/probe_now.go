package accounts

// Issue #192: "look again now".
//
// Everything else in probe.go is a schedule. ProbeIdle sweeps, needsProbe
// decides, and both of them are heuristics standing in for a question nobody
// was there to answer: is this account's stored reading still true? Two of
// those heuristics have teeth.
//
//   - NextProbeAt (#90, bounded by #190) spaces out re-probes of an exhausted
//     account, doubling on every rejection.
//   - billedProbeAge (#152) rations a probe that would be charged to roughly
//     one a day.
//
// Both exist because nothing else knew the state had changed. When a user
// says "check this account now", something does: they bought a reset, or
// fixed a payment method, or watched the provider's own dashboard tick over.
// An explicit human request is the signal those two heuristics approximate,
// so the forced path consults neither — it does not call needsProbe at all.
// The live case this closes: two accounts sat in ~21 hours of #90 backoff
// after a bought reset, and the only way to make spillway look was to
// restart the daemon.
//
// What it does NOT bypass is the money rule, and that is deliberate.
// §6.21 — "a probe must never be a purchase" — does not have an exception
// for an impatient user, because the user asking for a check has not thereby
// agreed to buy one. So a billable probe is refused and needs an explicit
// force, mirroring how `spillway switch` refuses a pin the provider would
// bill and offers --force (#139). Where the probe is free, which is the
// common case, it just runs.
//
// What it also does not touch is windowRejected (#54, TTL'd by #194). A
// rejection is a confirmed provider refusal for one model family, and #195
// concluded a probe cannot usefully re-ask it: the probe always asks for a
// fixed non-fable model, so it never engages the rejected family and comes
// back with no evidence about it either way. Clearing the entry would put
// the account back on tier 1 for that family on no new information, where
// the next real request meets the same refusal. #194 already gave it a
// bounded way back — after 30 minutes the account is merely deprioritised
// for the family and an ordinary request re-tests it on the last-resort
// tier, where a refusal costs nothing. This button cannot improve on that,
// so it leaves it alone.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

// ProbeNow sends one probe to the named account immediately, bypassing both
// scheduling gates. It reports whether the probe was BILLED — which is only
// ever true when force was given, since that is the one case the refusal
// below lets through.
//
// staleAfter is the ordinary probe cadence (ProbeIdle's own argument). It is
// used for nothing but probeOne's re-probe backoff base, so a forced probe
// that is rejected again spaces the NEXT scheduled one exactly as a swept
// probe would have. Passing it does not re-introduce the gates: needsProbe
// is never consulted on this path.
//
// force is the user saying the charge is acceptable. It is not a general
// override: it unlocks the money refusal and nothing else, because nothing
// else on this path refuses.
func ProbeNow(ctx context.Context, p *pool.Pool, client *http.Client, defaultUpstream string,
	staleAfter time.Duration, name string, force bool, logger *slog.Logger) (billed bool, err error) {

	var a *pool.Account
	for _, c := range p.Accounts() {
		if c.Name == name {
			a = c
			break
		}
	}
	if a == nil {
		return false, fmt.Errorf("no account named %q", name)
	}
	// Probing these is not dangerous, it is pointless, and saying so beats
	// spending thirty seconds on a request that cannot inform anything: a
	// disabled account's credential is known dead, and a parked one has been
	// deliberately set aside. Callers that can see the account's state — the
	// admin endpoint does — refuse earlier and more legibly; this is the
	// function's own contract, not their error path.
	if a.Parked() {
		return false, fmt.Errorf("account %q is parked", name)
	}
	if a.State() == pool.StateDisabled {
		return false, fmt.Errorf("account %q is disabled", name)
	}

	// Asked once, here, and reported back: wouldBill is the probe guard's own
	// money question (#152) — extra usage permitted for this account AND its
	// own quota gone — so an account nobody opted in cannot reach the refusal
	// at all, let alone be charged through it (issue #34).
	billed = wouldBill(a, p.AllowOverage(), time.Now())
	if billed && !force {
		return false, fmt.Errorf(
			"%w: %q is out of quota and has extra usage permitted, so this probe is a charged request",
			pool.ErrProbeWouldBill, name)
	}

	// Same credential handling as the sweep: an idle account's token expires
	// with nothing to notice, and probing with a dead one just 401s.
	if err := p.EnsureFresh(ctx, a); err != nil {
		logger.Warn("forced probe: credential refresh failed", "account", name, "err", err)
	}
	if a.State() == pool.StateDisabled {
		return false, fmt.Errorf("account %q was disabled while refreshing its credential", name)
	}

	perr := probeOne(ctx, p, a, client, defaultUpstream, staleAfter)
	if errors.Is(perr, errProbeUnauthorized) {
		// The stored token was superseded by another holder. Recover once
		// and retry, exactly as ProbeIdle does.
		if rerr := p.Recover(ctx, a); rerr == nil {
			perr = probeOne(ctx, p, a, client, defaultUpstream, staleAfter)
		}
	}
	if perr != nil {
		return billed, fmt.Errorf("probe of %q failed: %w", name, perr)
	}
	logger.Info("forced quota probe", "account", name, "billed", billed,
		"windows", len(a.QuotaWindows()))
	return billed, nil
}
