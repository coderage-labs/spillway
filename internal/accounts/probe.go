package accounts

// Quota probe: the one place spillway acts as a client rather than proxying
// one. An idle account reports no quota until something is routed to it, so a
// standby account's tank stays blank — possibly for days, which reads as a
// broken dashboard rather than an unused account.
//
// Sending a provider traffic nobody asked for is the most ToS-provocative
// thing here, so the limits want to be findable, and this is where they are:
// the design doc states no non-goal covering synthetic traffic. (This comment
// used to cite "§9, no fake traffic". There is no §9 — the doc runs 1-6, then
// 12a and 13 — and no section of it rules this out. §6.21 is the one probe
// rule it does state, and that one is about money; see below.)
//
// The schedule: opt-out via `probeOnStart`, one cheap request per account, at
// startup for accounts with no reading at all, then on a `probeInterval`
// ticker — 30m by default, `0` for startup only — for readings older than
// that interval. An account that is serving gets fresh quota headers for free
// and never goes stale, so steady state costs one request per *idle* account
// per tick.
//
// Two suppressions, both deliberate. Disabled accounts are skipped outright.
// So is an account that would be CHARGED for the probe (wouldBill): its quota
// spent, its stored reset still ahead, AND extra usage permitted for it, so
// the provider answers with a 200 and a bill rather than a refusal (design
// doc §6.21, "a probe must never be a purchase"). All three parts matter.
// Until issue #152 the third was missing and the guard asked only "is it
// spent?": with allowOverage off — the default, and the only setting that
// spends money — a probe on a spent account cannot be charged, the provider
// refuses it with a free 429, and that 429 carries the very quota headers a
// wrong stored reading needs. The guard was declining a free request to avoid
// a charge that could not occur. needsProbe returns true for an account with
// no windows at all before ever reaching this guard, so on its own that would
// let the first probe after a daemon restart bill (issue #34) — fixed not
// here but one level up, by never actually starting a restart with zero
// windows: SeedQuota (seed.go) installs last-known state from quota_samples
// before the probe loop runs, so "no windows" means genuinely unknown again.
//
// Issue #90 reuses this exact machinery for a second purpose: an exhausted
// account is never selected for real traffic, so nothing else ever learns
// that its bench was wrong (transient, spurious, or a stale reset value) —
// before this, only a daemon restart, which clears exhaustion in memory
// unconditionally, ever discovered that. ProbeIdle already walks every
// non-disabled account each tick, including exhausted ones, and needsProbe's
// wouldBill guard already never lets it bill; the only piece added for #90
// is what happens to the RESULT: probeOne now checks whether the account
// was exhausted going in, and if so hands the response to reprobeOutcome,
// which clears the bench on success or extends it (growing, never
// resetting, its own re-probe backoff) on a confirmed rejection. No new
// ticker, no new config setting — the same probeOnStart/probeInterval
// schedule now also re-verifies exhausted accounts while they are
// exhausted, not just idle ones.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
)

// probeClassifyCap bounds how much of a probe response body is read for
// classification (issue #90's reprobeOutcome) — mirrors the proxy's own
// max429Body cap for the same reason: a provider error body has no business
// being large, and there's no case for buffering more of one.
const probeClassifyCap = 1 << 16

// probeBody is the cheapest request that still returns quota headers: one
// token, no thinking, shortest viable prompt.
const probeBody = `{"model":"%s","max_tokens":1,"messages":[{"role":"user","content":"."}]}`

// probeModel asks the provider which model a quota probe should name.
func probeModel(a *pool.Account) string {
	return provider.For(a.Type).ProbeModel(a.ModelMap)
}

// ProbeIdle sends one minimal request per account whose quota data is missing
// or older than staleAfter, so a standby tank shows a current level instead of
// "awaiting signal" or a reading from hours ago. Accounts that are serving get
// fresh headers for free and are skipped, as are disabled ones and any where
// the probe would be BILLED rather than refused — spent, unreset and with
// extra usage permitted (see wouldBill). A spent account that cannot bill is
// probed like any other: the worst it can cost is a 429, which refreshes the
// reading rather than re-charging for it.
//
// The pool's allowOverage default is read once here and passed down rather
// than being fetched inside needsProbe, so one sweep decides against one
// consistent value of the money switch — the dashboard can flip it from
// another goroutine mid-sweep.
func ProbeIdle(ctx context.Context, p *pool.Pool, client *http.Client, defaultUpstream string,
	staleAfter time.Duration, logger *slog.Logger) {
	poolAllows := p.AllowOverage()
	for _, a := range p.Accounts() {
		if a.State() == pool.StateDisabled || !needsProbe(a, poolAllows, staleAfter) {
			continue
		}
		// An idle account's token expires with nothing to notice: no request
		// is routed to it, so nothing refreshes it. Probing with a dead token
		// just 401s and leaves the tank blank — which is exactly what happened
		// to a standby account overnight.
		if err := p.EnsureFresh(ctx, a); err != nil {
			logger.Warn("probe: credential refresh failed", "account", a.Name, "err", err)
		}
		if a.State() == pool.StateDisabled {
			continue
		}
		err := probeOne(ctx, p, a, client, defaultUpstream, staleAfter)
		if errors.Is(err, errProbeUnauthorized) {
			// The stored token was superseded (another holder refreshed it).
			// Recover once and retry before giving up.
			if rerr := p.Recover(ctx, a); rerr == nil {
				err = probeOne(ctx, p, a, client, defaultUpstream, staleAfter)
			}
		}
		if err != nil {
			// Never fatal: a probe failing says nothing about whether the
			// account can serve real traffic.
			logger.Warn("quota probe failed", "account", a.Name, "err", err)
			continue
		}
		logger.Info("quota probe", "account", a.Name, "windows", len(a.QuotaWindows()))
	}
}

// wouldBill reports that probing this account would be charged, rather than
// refused for free or covered by the subscription.
//
// Two halves, both required.
//
// CanOverage — can this account spend at all? Extra usage has to be permitted
// for it, by its own opt-in or by pool.allowOverage. With that off, which is
// the default and the only setting documented as spending money, a probe on a
// spent account is answered with a 429: free, and carrying fresh quota
// headers. Skipping it buys nothing and forgoes the one reading that can
// correct a wrong one (issue #152).
//
// readsSpent — is the subscription's own quota gone? While it is not, the
// probe is covered whatever the overage setting says.
//
// The billing case is not hypothetical. An account whose weekly window is
// spent but which has extra usage available answers a probe with a 200 and a
// bill — so `probeOnStart` plus `probeInterval` quietly bought a request every
// thirty minutes, forever, to re-learn a fact already on file.
func wouldBill(a *pool.Account, poolAllows bool, now time.Time) bool {
	if !a.CanOverage(poolAllows) {
		return false // the provider refuses instead of charging: free
	}
	return readsSpent(a, now)
}

// readsSpent reports what the stored reading CLAIMS: that the probe's own
// quota is gone and has not yet come back — a governing window at or past its
// limit, with the reset it named still ahead (or never reported at all).
//
// Only the windows the probe's OWN model draws on count. A spent family the
// probe never engages says nothing about what the probe would meet: the probe
// asks for a small non-fable model, so a spent fable bucket cannot charge it,
// and treating that as spent would park an otherwise healthy account's tank
// until that family reset — up to a week — while nothing refreshed it. That
// was reachable before quota windows were retained across readings and is the
// ordinary case now that they are.
func readsSpent(a *pool.Account, now time.Time) bool {
	governing := map[string]bool{}
	if gw := provider.For(a.Type).GoverningWindows; gw != nil {
		for _, name := range gw(probeModel(a)) {
			governing[name] = true
		}
	}
	spent, reset := false, time.Time{}
	for _, w := range a.QuotaWindows() {
		if len(governing) > 0 && !governing[w.Name] {
			continue // a family this probe never engages cannot charge it
		}
		if w.Limit <= 0 || w.Used/w.Limit < 1 {
			continue
		}
		spent = true
		if reset.IsZero() || w.ResetAt.Before(reset) {
			reset = w.ResetAt
		}
	}
	if !spent {
		return false
	}
	// now.Before(reset): still inside the window that made it spent. That is
	// the reading's own account of itself, and where a probe would be billed
	// it is the only account of itself available for free — but it is not
	// proof. A window can refill before the reset it announced: Anthropic ran
	// an ad-hoc reset for every user on 2026-09-04, outside the reset times
	// its own headers had given, and #135 records a 7d falling to 0.0 while
	// its reported reset stayed thirty-one hours ahead. So this is what the
	// reading claims, not what is known. needsProbe re-measures the claim for
	// free wherever a probe cannot be charged, and caps how long it goes
	// unchallenged where one can (billedProbeAge).
	//
	// reset.IsZero(): no reset was ever recorded for the spent window (a
	// provider that didn't report one), so there is no time to wait for and
	// no way to tell "still spent" from "reset already happened but nobody
	// looked". This deliberately still counts as spent — never risk a bill on
	// a guess — which, where a probe WOULD bill, means such an account's tank
	// leans on the age cap and on ordinary proxied traffic (which bills only
	// because the request itself was real) to move at all. The alternative,
	// treating an unknown reset as refilled, would turn "unknown" into "bill
	// every staleAfter tick forever" for any provider window that omits a
	// reset — worse than a slow tank against the one hard rule here (never
	// spend uninvited), so this is the deliberate choice, not a latent bug.
	// Where the probe is free the distinction costs nothing either way:
	// needsProbe re-measures on the ordinary cadence regardless.
	return reset.IsZero() || now.Before(reset)
}

// billedProbeAge is how long an account whose probe WOULD be billed may sit on
// one stored reading before a single probe is bought to re-check it (issue
// #152, the age cap).
//
// Waiting for the reported reset assumes a window can only refill at the
// moment its own header predicted, which readsSpent's comment records is
// false. Where the probe is free, dropping that assumption costs nothing.
// Where it is billed, dropping it entirely would reinstate issue #34's
// charge-every-tick, and keeping it can park real capacity for days — three
// accounts sat wrongly spent for up to five days after the 2026-09-04 ad-hoc
// reset, with a fourth carrying all the traffic. So the wait is bounded
// instead: one bought request per this interval is a far smaller cost than
// days of wrongly-deprioritised capacity, and #138's FetchedAt is what makes
// the reading's true age knowable in the first place.
//
// A multiple of the ordinary probe cadence, clamped at both ends so no
// configured interval turns "about once a day" into "every few minutes" or
// "not this week". staleAfter <= 0 is startup-only probing, which has no
// cadence to scale from, so it takes the ceiling.
func billedProbeAge(staleAfter time.Duration) time.Duration {
	const (
		factor  = 48 // 48 x the 30m default = one billed probe a day
		floor   = 6 * time.Hour
		ceiling = 24 * time.Hour
	)
	if staleAfter <= 0 || staleAfter >= ceiling {
		return ceiling
	}
	d := factor * staleAfter
	switch {
	case d < floor:
		return floor
	case d > ceiling:
		return ceiling
	}
	return d
}

// needsProbe is true when we have no quota reading, or the one we have is
// worth re-measuring: aged past staleAfter, or claiming the account is spent
// when finding out otherwise costs nothing.
//
// The money guard sits after the no-reading case, not before it, so on its own
// it would only stop repeat probes: an account with no windows at all would be
// probed even when that bills, which used to be one charge per daemon restart
// (issue #34). Deliberately not fixed by moving the check above the
// empty-windows return — that would leave a never-yet-probed account's tank
// blank forever, since nothing else refreshes an idle account. The actual fix
// is that "no windows" no longer happens on a normal restart: SeedQuota
// (seed.go) installs last-known state from quota_samples before this ever
// runs, so len(wins) == 0 here means the account genuinely has never had a
// reading, not "we just restarted".
func needsProbe(a *pool.Account, poolAllows bool, staleAfter time.Duration) bool {
	wins := a.QuotaWindows()
	if len(wins) == 0 {
		return true
	}
	now := time.Now()
	newest := time.Time{}
	for _, w := range wins {
		if w.FetchedAt.After(newest) {
			newest = w.FetchedAt
		}
	}
	if wouldBill(a, poolAllows, now) {
		// This probe is a purchase. Hold off until the reading is old
		// enough that one bought request beats going on believing it.
		//
		// newest.IsZero() is an UNKNOWN measurement time, not an infinitely
		// old one — a sample seeded from a row written before #138 added
		// fetched_at carries exactly that. Spending against it would buy a
		// request on every daemon restart, which is issue #34 verbatim, so
		// the cap holds off until there is a real measurement time to
		// measure against. Elsewhere in this function an unknown FetchedAt
		// counts as maximally stale, because there the mistake is free.
		if newest.IsZero() || now.Sub(newest) < billedProbeAge(staleAfter) {
			return false
		}
	}
	// A rejected re-probe (issue #90) sets NextProbeAt to enforce its own
	// growing backoff, separately from — and possibly longer than —
	// staleAfter; check it before the staleness comparison below so a
	// repeatedly-rejected exhausted account is spaced out rather than
	// re-probed every tick at the base interval.
	if next := a.NextProbeAt(); !next.IsZero() && now.Before(next) {
		return false
	}
	// Not the same question as wouldBill above: this is what the reading
	// claims regardless of who would pay, and below it is a reason to
	// re-measure rather than a reason not to.
	spent := readsSpent(a, now)
	if staleAfter <= 0 {
		// Periodic probing disabled: startup gap only. A reading that says
		// "spent" is still a gap. It is the one reading nothing else can
		// correct — no traffic is routed to a spent account, and #137's
		// expiry cannot fire while its reset is ahead — and after a restart
		// it is last-known state seeded from a previous run, not a
		// measurement of now. Re-measuring it is what makes restarting the
		// daemon the escape hatch users already expect it to be; where that
		// would be billed, the age cap above has already had its say.
		return spent
	}
	// Ordinary cadence, spent or not: a spent account is re-measured because
	// its reading is the one that can be wrong in the direction nothing else
	// checks, but never harder than an idle healthy one.
	return now.Sub(newest) > staleAfter
}

// errProbeUnauthorized marks a 401 so the caller can try a credential
// recovery before writing the account off.
var errProbeUnauthorized = errors.New("probe unauthorized")

// probeOne sends one probe request. reprobeBackoff is the base spacing
// (issue #90) used only when the account was already exhausted going in
// and this probe is rejected again — the same value as ProbeIdle's own
// staleAfter, so a rejected re-probe is never spaced closer than the
// ordinary probe cadence.
func probeOne(ctx context.Context, p *pool.Pool, a *pool.Account, client *http.Client, defaultUpstream string, reprobeBackoff time.Duration) error {
	upstream := a.Upstream
	if upstream == "" {
		upstream = defaultUpstream
	}
	model := probeModel(a)
	reqBody := fmt.Sprintf(probeBody, model)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(upstream, "/")+"/v1/messages", bytes.NewReader([]byte(reqBody)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Authorization", "Bearer "+a.Token())

	// Captured before the request: this probe's own RecordQuota call below
	// updates the account's windows, and reading state only afterward could
	// no longer tell "was exhausted, now recovering" from "was already
	// fine".
	wasExhausted := a.State() == pool.StateExhausted

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, probeClassifyCap))

	p.RecordQuota(a, resp.Header, time.Now())
	// The probe really did serve this model, so the status line has something
	// truthful to show before the first real request.
	if resp.StatusCode < 400 {
		a.SetLastModel(model)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errProbeUnauthorized
	}
	if resp.StatusCode >= 400 && len(a.QuotaWindows()) == 0 {
		return fmt.Errorf("probe returned %d with no quota headers", resp.StatusCode)
	}

	if wasExhausted {
		reprobeOutcome(p, a, provider.For(a.Type), resp.StatusCode, resp.Header, respBody, reprobeBackoff)
	}
	return nil
}

// resetFor mirrors internal/proxy/proxy.go's quotaReset: the provider
// decides the deadline from its own response headers, restricted to
// `windows`, falling back to the account's last known quota reset when the
// headers don't say. Kept as its own small copy here (rather than exported
// from proxy, which this package must not import) because it is a
// three-line wrapper around Spec.ResetHint with no independent judgment
// call of its own to drift out of sync.
func resetFor(a *pool.Account, spec provider.Spec, h http.Header, windows []string) time.Time {
	now := time.Now()
	fallback := time.Time{}
	if reset, ok := a.EarliestQuotaReset(); ok {
		fallback = reset
	}
	return spec.ResetHint(h, windows, now, fallback)
}

// reprobeOutcome interprets a probe response for an account that was
// already StateExhausted before this probe ran (issue #90): a probe is the
// one place spillway acts as a client on an exhausted account's behalf, so
// it is also the one place that can discover a bench was wrong before its
// recorded exhaustedUntil says so.
//
// A response with no confirmed quota rejection clears the exhaustion
// immediately (ClearExhausted) — matching the live-observed case of a
// daemon restart finding an account StateOK and its weekly window at 17%
// remaining, days before its recorded deadline.
//
// A response still rejected re-derives `until` the same way the live
// rejection path does — #54's family scoping via provider.ScopeRejection,
// #90's soonest-not-longest fix inside anthropicReset — and extends the
// bench via MarkReprobeRejected, which also grows the re-probe backoff
// instead of resetting it, so a genuinely spent account is probed less and
// less often rather than every tick forever.
//
// Anything else — ErrRate, ErrModelID, or a rejection that turns out not to
// be account-wide (`wide` false) — is inconclusive: the probe always asks
// for a fixed, known-good, non-fable model, so neither should occur in
// practice, and guessing here risks either clearing a bench that is still
// real or extending one on a signal that was never actually about the whole
// account. Left untouched; the next probe tick tries again.
func reprobeOutcome(p *pool.Pool, a *pool.Account, spec provider.Spec, status int, h http.Header, body []byte, baseInterval time.Duration) {
	switch spec.Classify(status, h, body) {
	case provider.ErrQuota:
		var rejected []string
		if spec.RejectedWindows != nil {
			rejected = spec.RejectedWindows(h)
		}
		wide, wideNames := provider.ScopeRejection(a.Type, rejected)
		if !wide {
			return
		}
		until := resetFor(a, spec, h, wideNames)
		if until.IsZero() {
			until = time.Now().Add(baseInterval)
		}
		p.MarkReprobeRejected(a, until, baseInterval)
	case provider.ErrNone:
		if status < 400 {
			p.ClearExhausted(a)
		}
	}
}
