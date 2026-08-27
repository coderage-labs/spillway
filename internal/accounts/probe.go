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
// So is an account whose quota is spent and whose stored reset has not passed
// (wouldBill), and that one is a money question: with extra usage enabled such
// an account answers a probe with a 200 and a charge, while the reading and
// reset time already on file say everything the probe would (design doc §6.21,
// "a probe must never be a purchase"). needsProbe returns true for an account
// with no windows at all before ever reaching this guard, so on its own that
// would let the first probe after a daemon restart bill (issue #34) — fixed
// not here but one level up, by never actually starting a restart with zero
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
// fresh headers for free and are skipped, as are disabled ones and any whose
// quota is spent and unreset — probing those costs money, not information
// (see wouldBill).
func ProbeIdle(ctx context.Context, p *pool.Pool, client *http.Client, defaultUpstream string,
	staleAfter time.Duration, logger *slog.Logger) {
	for _, a := range p.Accounts() {
		if a.State() == pool.StateDisabled || !needsProbe(a, staleAfter) {
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

// wouldBill reports that probing this account would be charged rather than
// covered by the subscription.
//
// This is not hypothetical. An account whose weekly window is spent but which
// has extra usage available answers a probe with a 200 and a bill — so
// `probeOnStart` plus `probeInterval` quietly bought a request every thirty
// minutes, forever, to re-learn a fact already on file.
//
// There is nothing to learn either way: if quota is gone the probe returns
// the reading we already have, and if it is gone with overage on, we pay for
// the privilege. The stored reset time is what says when to look again, so
// waiting for it costs nothing.
func wouldBill(a *pool.Account, now time.Time) bool {
	spent, reset := false, time.Time{}
	for _, w := range a.QuotaWindows() {
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
	// now.Before(reset): still within the window that made it spent, so the
	// stored reading already says what a probe would — wait for the reset
	// instead of paying to re-learn it.
	//
	// reset.IsZero(): no reset was ever recorded for the spent window (a
	// provider that didn't report one), so there is no time to wait for and
	// no way to tell "still spent" from "reset already happened but nobody
	// looked". This deliberately still returns true — never probe rather
	// than risk a bill — which means such an account's tank can go stuck
	// until something other than a probe updates it (ordinary proxied
	// traffic, which bills only because the request itself was real, still
	// refreshes it via RecordQuota). The alternative, probing whenever the
	// reset is unknown, would turn "unknown" into "bill every staleAfter
	// tick forever" for any provider window that omits a reset — worse than
	// a stuck tank against the one hard rule here (never spend uninvited),
	// so this is the deliberate choice, not a latent bug.
	return reset.IsZero() || now.Before(reset)
}

// needsProbe is true when we have no quota reading, or the newest one has
// aged past staleAfter.
//
// The wouldBill guard sits after the no-reading case, not before it, so on
// its own it would only stop repeat probes: an account with no windows at
// all would be probed even when that bills, which used to be one charge per
// daemon restart (issue #34). Deliberately not fixed by moving the check
// above the empty-windows return — that would leave a never-yet-probed
// account's tank blank forever, since nothing else refreshes an idle
// account. The actual fix is that "no windows" no longer happens on a normal
// restart: SeedQuota (seed.go) installs last-known state from quota_samples
// before this ever runs, so len(wins) == 0 here means the account genuinely
// has never had a reading, not "we just restarted".
func needsProbe(a *pool.Account, staleAfter time.Duration) bool {
	wins := a.QuotaWindows()
	if len(wins) == 0 {
		return true
	}
	if wouldBill(a, time.Now()) {
		return false
	}
	// A rejected re-probe (issue #90) sets NextProbeAt to enforce its own
	// growing backoff, separately from — and possibly longer than —
	// staleAfter; check it before the ordinary staleness comparison below so
	// a repeatedly-rejected exhausted account is spaced out rather than
	// re-probed every tick at the base interval.
	if next := a.NextProbeAt(); !next.IsZero() && time.Now().Before(next) {
		return false
	}
	if staleAfter <= 0 {
		return false // periodic probing disabled: startup gap only
	}
	newest := time.Time{}
	for _, w := range wins {
		if w.FetchedAt.After(newest) {
			newest = w.FetchedAt
		}
	}
	return time.Since(newest) > staleAfter
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
