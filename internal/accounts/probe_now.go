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
// windowRejected (#54, corrected by #194) is a different story, and #229
// changed what happens to it here twice.
//
// First pass: #194's exclusion used to lapse on its own after
// windowRejectionTTL, at which point an ordinary request became the
// re-test; #229 found that this cost a real, held request a certain 429
// every TTL, because the rejection's actual reset was still days out. The
// exclusion now lasts that real reset (see pool.MarkWindowRejected's
// comment), so the only way back before it, short of the reset itself
// arriving, is evidence — probeOne now calls
// pool.ClearRecoveredWindowRejections after every probe (this one
// included), which looks at what the response actually measured for the
// rejected window, never at whether THIS probe's own model happened to
// succeed.
//
// Second pass, from a production-live review of the first: #195's premise
// — a probe cannot usefully re-ask a rejection, because it always asks for
// a fixed non-fable model — is exactly the gap that made the first pass
// insufficient on its own. Judged against a live daemon, the ordinary probe
// genuinely never wrote a "7d-fable" row, so ClearRecoveredWindowRejections
// never had anything to clear and a fable rejection sat excluded for its
// full (now much longer) real reset regardless. probeOne now ALSO runs
// probeRejectedFamilies — a second, family-scoped probe, using a model
// that family actually governs (provider.Spec.FamilyProbeModel), for every
// window rejection WindowRejectionNeedsProbeAt says has gone un-measured
// too long. force, threaded down from here, is what lets a forced "check
// now" also bypass THAT probe's own identical §6.21 money refusal — never
// the scheduled sweep, which always passes false. Most calls still carry
// no evidence either way and are a no-op; the one that does (a manual free
// reset, #228, or an ad-hoc provider reset, #135) is exactly the case a
// restart used to be the only way to notice.

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

	perr := probeOne(ctx, p, a, client, defaultUpstream, staleAfter, force, logger)
	if errors.Is(perr, errProbeUnauthorized) {
		// The stored token was superseded by another holder. Recover once
		// and retry, exactly as ProbeIdle does.
		if rerr := p.Recover(ctx, a); rerr == nil {
			perr = probeOne(ctx, p, a, client, defaultUpstream, staleAfter, force, logger)
		}
	}
	if perr != nil {
		return billed, fmt.Errorf("probe of %q failed: %w", name, perr)
	}
	logger.Info("forced quota probe", "account", name, "billed", billed,
		"windows", len(a.QuotaWindows()))
	return billed, nil
}
