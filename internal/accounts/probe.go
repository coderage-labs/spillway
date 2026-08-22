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

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
)

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
		err := probeOne(ctx, p, a, client, defaultUpstream)
		if errors.Is(err, errProbeUnauthorized) {
			// The stored token was superseded (another holder refreshed it).
			// Recover once and retry before giving up.
			if rerr := p.Recover(ctx, a); rerr == nil {
				err = probeOne(ctx, p, a, client, defaultUpstream)
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

func probeOne(ctx context.Context, p *pool.Pool, a *pool.Account, client *http.Client, defaultUpstream string) error {
	upstream := a.Upstream
	if upstream == "" {
		upstream = defaultUpstream
	}
	model := probeModel(a)
	body := fmt.Sprintf(probeBody, model)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(upstream, "/")+"/v1/messages", bytes.NewReader([]byte(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Authorization", "Bearer "+a.Token())

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

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
	return nil
}
