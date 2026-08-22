package accounts

// Startup quota probe. An idle account reports no quota until something is
// routed to it, so a standby account's tank stays blank — possibly for days,
// which reads as a broken dashboard rather than an unused account.
//
// This is spillway acting as a client rather than proxying one, which the
// design doc lists as a deliberate non-goal (§9, "no fake traffic"): it is the
// most ToS-provocative thing here. It is therefore opt-out, one
// request per account, only for accounts with no quota data at all, and only
// at startup — never on a timer.

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
// fresh headers for free and are skipped, as are disabled ones.
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
	// Past its reset with no fresh reading: probe again, that is the only way
	// to learn it has refilled.
	return reset.IsZero() || now.Before(reset)
}

// needsProbe is true when we have no quota reading, or the newest one has
// aged past staleAfter — and never when probing would be billed.
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
