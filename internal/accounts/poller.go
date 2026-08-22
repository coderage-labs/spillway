package accounts

// /usages quota polling for kimi accounts (design doc §6.5: Kimi sends no
// rate-limit headers, so quota state comes from polling, not responses).

import (
	"context"
	"log/slog"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
)

// UsagesPollInterval is how often each kimi account's /usages is read.
const UsagesPollInterval = 60 * time.Second

// PollKimiUsages fetches /usages once for every kimi account and stores the
// windows as its quota state. Failures are logged, never fatal — quota state
// degrades to "unknown" and rotation falls back to reactive errors (§6.5).
func PollKimiUsages(ctx context.Context, p *pool.Pool, logger *slog.Logger) {
	for _, a := range p.Accounts() {
		// Providers that report usage out-of-band rather than in response
		// headers (§6.20).
		if !provider.For(a.Type).PollsUsage || a.State() == pool.StateDisabled {
			continue
		}
		usages, err := provider.FetchKimiUsages(ctx, nil, a.Upstream, a.Token())
		if err != nil {
			logger.Warn("kimi /usages poll failed", "account", a.Name, "err", err)
			continue
		}
		windows := make([]pool.QuotaWindow, len(usages))
		for i, u := range usages {
			windows[i] = pool.QuotaWindow{
				Name:      u.Name,
				Limit:     u.Limit,
				Used:      u.Used,
				ResetAt:   u.ResetAt,
				FetchedAt: time.Now(),
				// Without this the replace-by-source filter has nothing to
				// match and every poll appends instead.
				Source: "poll",
			}
		}
		a.SetQuotaWindows(windows)
		logger.Debug("kimi quota state", "account", a.Name, "windows", windows)
	}
}

// StartUsagesPoller runs PollKimiUsages on an interval until ctx ends.
func StartUsagesPoller(ctx context.Context, p *pool.Pool, logger *slog.Logger) {
	go func() {
		PollKimiUsages(ctx, p, logger) // prime immediately
		tick := time.NewTicker(UsagesPollInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				PollKimiUsages(ctx, p, logger)
			}
		}
	}()
}
