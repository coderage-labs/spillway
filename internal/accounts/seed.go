package accounts

// Startup quota seeding (design doc §6.21 item 4).
//
// Quota windows live in memory only (pool.Account.windows), so every daemon
// restart clears every account back to zero windows. needsProbe's
// empty-windows branch runs before the money guard (wouldBill), so on an
// account that is spent with extra usage permitted, that branch used to
// probe unconditionally: a real request, answered 200, and charged — once
// per restart, and the Homebrew cask restarts the service on every upgrade
// (issue #34).
//
// The fix is not to move the guard above the empty-windows check — an
// account with no reading at all would then never be probed, and nothing
// else refreshes an idle account, so its tank would stay blank forever
// (trading the money bug for a permanent blind spot). Instead, SeedQuota
// installs each account's last-known state from the persisted quota_samples
// table before the probe loop ever runs, so "zero windows" goes back to
// meaning what needsProbe assumes it means: genuinely unknown, safe to probe.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// SeedQuota loads the newest quota_samples row for each account/window and
// installs it as that account's starting quota state.
//
// Staleness: a sample is only seeded if it is still inside its own window —
// now before its ResetAt. A sample whose reset has already passed says
// nothing about current state: a 5h window sampled hours ago has certainly
// turned over, and even a 7d one may have (Anthropic can report a reset that
// lags reality). Seeding a stale "spent" reading past its reset would
// suppress a probe that should happen — the exact opposite failure to the
// one being fixed here — so it is discarded instead, and the account is
// probed fresh like any other with no data. A sample with no recorded reset
// at all (ResetAt.IsZero()) is the same unknown-safety case and is discarded
// on the same reasoning: both providers report a reset for anything actually
// worth billing over, so this should only bite readings that were never
// going to suppress a probe anyway.
//
// SeedQuota is an optimisation, not a correctness requirement (issue #104):
// ctx bounds how long it will wait on the query. A caller on the startup
// path should pass a short timeout so a slow or stuck read.Log does not
// delay binding the listeners — if ctx expires first, this logs a warning
// and returns having touched nothing, exactly like the "no samples at all"
// case. That matters here specifically because it uses modernc.org/sqlite,
// whose QueryContext genuinely interrupts the in-flight query on
// cancellation (sqlite3_interrupt) rather than abandoning it to finish on
// its own: without that, a query that finished late, after real probe/poll
// data had already arrived, could overwrite live state with a stale seed
// (setWindowsSourced's replace-by-source is wholesale, not merge-if-newer).
//
// Seeded windows are stamped with the Source the account's own provider
// uses live ("headers" for header-reporting providers, "poll" for polled
// ones) rather than a distinct "seeded" tag. QuotaWindow replacement is
// per-source and wholesale (setWindowsSourced): the first live reading —
// probe, poll, or ordinary proxied traffic — then atomically replaces the
// seed rather than leaving a second, stale copy of the same window name
// sitting alongside it forever, which is the exact bug class
// setWindowsSourced's own doc comment records for unlabelled "poll" writes.
// The seed's age is still visible downstream regardless of source: FetchedAt
// carries the original sample's timestamp rather than now, so both the
// dashboard's relative-time display and needsProbe's own staleness check
// see it for what it is.
func SeedQuota(ctx context.Context, p *pool.Pool, rl *reqlog.Log, now time.Time, logger *slog.Logger) {
	samples, err := rl.LatestQuotaSamples(ctx)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			logger.Warn("seed quota: timed out reading quota_samples, starting unseeded", "err", err)
		} else {
			logger.Warn("seed quota: reading quota_samples failed", "err", err)
		}
		return
	}
	byAccount := make(map[string][]pool.QuotaWindow)
	for _, s := range samples {
		if s.ResetAt.IsZero() || !now.Before(s.ResetAt) {
			continue // unknown or already past reset: not current state
		}
		byAccount[s.Account] = append(byAccount[s.Account], pool.QuotaWindow{
			Name: s.Window, Limit: s.Limit, Used: s.Used,
			ResetAt: s.ResetAt, FetchedAt: s.Ts,
		})
	}
	for _, a := range p.Accounts() {
		wins := byAccount[a.Name]
		if len(wins) == 0 {
			continue
		}
		source := "headers"
		if provider.For(a.Type).PollsUsage {
			source = "poll"
		}
		for i := range wins {
			wins[i].Source = source
		}
		a.SetQuotaWindows(wins)
		logger.Info("seeded quota from last known state", "account", a.Name, "windows", len(wins))
	}
}
