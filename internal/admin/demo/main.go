package main

// Command demo serves the real dashboard over a crafted pool, so states that
// are hard or expensive to reproduce against live accounts can actually be
// looked at.
//
// It exists because the hold banner shipped rendering as an empty red bar on
// every page load. The bug was one CSS line and would have been obvious in a
// screenshot; it survived because there was no cheap way to produce the state
// that shows it.
//
// Two states, one flag:
//
//	go run ./internal/admin/demo          # healthy pool: four accounts, headroom to spare
//	go run ./internal/admin/demo -hold     # the same pool later: dry, paid extra usage
//	                                       # also spent, a request parked on the hold banner
//
// Then open http://127.0.0.1:7699. Every address is `*.example`; nothing here
// is a real account.

import (
	"flag"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

func main() {
	hold := flag.Bool("hold", false, "serve the dry-pool / hold-banner state instead of the healthy one")
	flag.Parse()

	now := time.Now()
	work, labs, side, kimi := buildAccounts(now, *hold)
	accounts := []*pool.Account{work, labs, side, kimi}

	p := pool.New(accounts, now)
	p.Apply(pool.Settings{AllowOverage: true, CrossProvider: !*hold})

	release := func() {}
	if *hold {
		// The pool truly has nothing to give: every Claude account is spent
		// (including side's paid extra usage), and cross-provider rotation
		// is off, so Kimi's headroom does not rescue a Claude request. That
		// combination is what makes the hold honest rather than staged.
		p.MarkExhausted(work, now.Add(38*time.Minute))
		p.MarkExhausted(labs, now.Add(72*time.Minute))
		p.MarkExhausted(side, now.Add(22*time.Minute))
		release = p.BeginHold(now.Add(22 * time.Minute))
	} else if err := p.Pin("you@acme.example", false); err != nil {
		log.Printf("demo: pin: %v", err)
	}
	defer release() // never reached past ListenAndServe, harmless if it were

	// Request log: a real SQLite file (never an in-memory stand-in — the
	// dashboard's history and requests endpoints are backed by reqlog, not
	// the pool), removed and rebuilt on every run so a capture never shows
	// a previous run's timestamps.
	logPath := filepath.Join(os.TempDir(), "spillway-demo-requests.db")
	_ = os.Remove(logPath)
	rl, err := reqlog.Open(logPath)
	if err != nil {
		log.Fatal(err)
	}
	defer rl.Close()
	seedRequests(rl, now, *hold)
	seedQuotaHistory(rl, now, accounts)

	broker := events.New()
	go seedSpillEvents(broker, *hold)

	srv := admin.New(p, rl, broker, "")

	// Settings enabled against a scratch config so the panel renders — it is
	// the only way to see the per-account rows, including the read-only
	// extra-usage state, and the crossProvider checkbox that makes the hold
	// state honest above.
	cfgPath := filepath.Join(os.TempDir(), "spillway-demo.yaml")
	_ = os.WriteFile(cfgPath, []byte(demoConfigYAML(*hold)), 0o600)
	srv.EnableSettings(cfgPath, func(*config.Config) {})

	log.Printf("demo: serving %s pool on http://127.0.0.1:7699", map[bool]string{true: "hold", false: "healthy"}[*hold])
	log.Fatal(http.ListenAndServe("127.0.0.1:7699", srv))
}

func demoConfigYAML(hold bool) string {
	cross := "true"
	if hold {
		cross = "false"
	}
	return `upstream: https://api.anthropic.com
pool:
  exhaustedMode: hold
  holdMax: 4h
  switchThreshold: 0.98
  allowOverage: true
  crossProvider: ` + cross + `
  probeOnStart: true
  probeInterval: 30m
accounts:
  - name: you@acme.example
    type: claude-oauth
    label: work
  - name: you@labs.example
    type: claude-oauth
    label: labs
  - name: you@side.example
    type: claude-oauth
    label: side
    priority: 5
    allowOverage: true
  - name: you@kimi.example
    type: kimi-oauth
    label: kimi
`
}

// buildAccounts returns the same four identities in both states — two
// Claude accounts (one pinned, one carrying a near-spent 7d-fable bucket
// while its general windows stay healthy), one Claude account on paid extra
// usage, and one Kimi account for cross-provider contrast. Only the numbers
// differ between the healthy and hold runs; nothing here misrepresents what
// spillway tracks.
func buildAccounts(now time.Time, hold bool) (work, labs, side, kimi *pool.Account) {
	work = pool.NewAccount("you@acme.example", pool.SourceYAML, "t", "", 0, "")
	work.Type = "claude-oauth"
	work.SetLabel("work")

	labs = pool.NewAccount("you@labs.example", pool.SourceYAML, "t", "", 0, "")
	labs.Type = "claude-oauth"
	labs.SetLabel("labs")

	side = pool.NewAccount("you@side.example", pool.SourceYAML, "t", "", 0, "")
	side.Type = "claude-oauth"
	side.SetLabel("side")
	side.SetPriority(5) // a paid backstop is spent last, not first

	kimi = pool.NewAccount("you@kimi.example", pool.SourceYAML, "t", "", 0, "")
	kimi.Type = "kimi-oauth"
	kimi.SetLabel("kimi")

	if hold {
		work.SetQuotaWindows([]pool.QuotaWindow{
			{Name: "5h", Limit: 1, Used: 1, Source: "headers", ResetAt: now.Add(38 * time.Minute), FetchedAt: now},
			{Name: "7d", Limit: 1, Used: 0.91, Source: "headers", ResetAt: now.Add(3*24*time.Hour + 2*time.Hour), FetchedAt: now},
		})
		labs.SetQuotaWindows([]pool.QuotaWindow{
			{Name: "5h", Limit: 1, Used: 1, Source: "headers", ResetAt: now.Add(72 * time.Minute), FetchedAt: now},
			{Name: "7d", Limit: 1, Used: 0.95, Source: "headers", ResetAt: now.Add(4*24*time.Hour + 19*time.Hour), FetchedAt: now},
			{Name: "7d-fable", Limit: 1, Used: 1, Source: "headers", ResetAt: now.Add(16 * time.Hour), FetchedAt: now},
		})
		side.SetQuotaWindows([]pool.QuotaWindow{
			{Name: "5h", Limit: 1, Used: 1, Source: "headers", ResetAt: now.Add(22 * time.Minute), FetchedAt: now},
			{Name: "7d", Limit: 1, Used: 1, Source: "headers", ResetAt: now.Add(1*24*time.Hour + 14*time.Hour), FetchedAt: now},
		})
		// The paid backstop is spent too: extra usage exhausted for the
		// billing period, not merely unconfigured. That is what makes the
		// hold that follows honest rather than a state overage would have
		// covered.
		side.SetOverageForTest(provider.Overage{
			Known: true, Available: false, InUse: false,
			Utilization: 1,
			Reason:      "extra usage exhausted for this billing period",
			ResetAt:     now.Add(3 * 24 * time.Hour),
		})
		// Deliberately far from the Claude cluster: seven windows above are
		// all bunched at 0-9% headroom, and Kimi sits alone up at 52-67%.
		// That combination — a tight low cluster plus a distant healthy
		// outlier — is the exact trigger for spillway#75 (the headroom
		// chart's anti-overlap pass could push the outlier's label off the
		// plot's top edge). An earlier version of this demo narrowed these
		// figures to 0.45/0.35 specifically to dodge that bug; now that
		// drawHeadroom clamps symmetrically, they're restored to the
		// original values so `-hold` doubles as a standing regression
		// check — this state is still comfortably "ok" (nowhere near the
		// 0.98 threshold) and still the healthiest account in the pool.
		kimi.SetQuotaWindows([]pool.QuotaWindow{
			{Name: "5h", Limit: 1, Used: 0.33, Source: "poll", ResetAt: now.Add(3*time.Hour + 20*time.Minute), FetchedAt: now},
			{Name: "7d", Limit: 1, Used: 0.48, Source: "poll", ResetAt: now.Add(4*24*time.Hour + 9*time.Hour), FetchedAt: now},
		})
		return
	}

	work.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.34, Source: "headers", ResetAt: now.Add(2*time.Hour + 51*time.Minute), FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 0.41, Source: "headers", ResetAt: now.Add(3*24*time.Hour + 11*time.Hour), FetchedAt: now},
	})
	labs.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.22, Source: "headers", ResetAt: now.Add(4*time.Hour + 6*time.Minute), FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 0.53, Source: "headers", ResetAt: now.Add(5*24*time.Hour + 4*time.Hour), FetchedAt: now},
		// Present so the per-family bucket is visible in a healthy pool,
		// but well under the 0.98 switch threshold — a hero image should
		// not lead with a spent bucket. The window going over threshold
		// (fableSpent) is demonstrated by the -hold state instead.
		{Name: "7d-fable", Limit: 1, Used: 0.35, Source: "headers", ResetAt: now.Add(2*24*time.Hour + 9*time.Hour), FetchedAt: now},
	})
	side.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.58, Source: "headers", ResetAt: now.Add(55 * time.Minute), FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 0.34, Source: "headers", ResetAt: now.Add(2*24*time.Hour + 19*time.Hour), FetchedAt: now},
	})
	// Extra usage is available and permitted (CanOverage doesn't require
	// the subscription quota to actually be spent — it's a standing
	// permission, confirmed by the provider), so the "on extra usage"
	// badge is honest even though this account's quota is currently
	// healthy. OverageUsed > 0 while InUse is false: it was drawn on
	// earlier in this billing period and that counter refills on its own
	// slower cycle (README §Extra usage), not with the quota window that
	// has since reset — not currently billing, just not starting from 0.
	side.SetOverageForTest(provider.Overage{
		Known: true, Available: true, InUse: false,
		Utilization: 0.22,
		ResetAt:     now.Add(11 * 24 * time.Hour),
	})
	kimi.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.31, Source: "poll", ResetAt: now.Add(3*time.Hour + 22*time.Minute), FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 0.47, Source: "poll", ResetAt: now.Add(4*24*time.Hour + 9*time.Hour), FetchedAt: now},
	})
	return
}

// historyStartShort is the used-fraction the seeded history starts at for
// a fast-cycling window (5h today), low enough to draw a visible rising
// trend into the account's live figure but never above it: a history that
// starts above cur would draw usage falling on its own within one quota
// window, which never happens — a window only ever climbs until it
// resets, at which point spillway records a fresh window rather than a
// dip in the old one. Without the second clamp, an account whose live Used
// is very small (cur*0.35 below the 0.02 floor) would get exactly that
// impossible dip.
func historyStartShort(cur float64) float64 {
	start := cur * 0.35
	if start < 0.02 {
		start = 0.02
	}
	if start > cur {
		start = cur
	}
	return start
}

// historyStartLong is the seeded starting point for a window whose reset
// is days away (7d, 7d-fable, or anything else the demo doesn't
// specifically know is fast-cycling). Two reasons it stays almost flat
// rather than climbing the same 65% that historyStartShort draws:
//
//   - realism: a bucket that only resets every several days genuinely
//     barely moves within the 5.5h of history seeded here.
//   - the dashboard's burn-rate alarm (index.html's beatsReset) compares a
//     projected dry time against the window's OWN reset, which for a 7d
//     window is measured in days. Reading a 65%-over-5.5h climb into that
//     projection makes almost any account look like it is about to run
//     dry before refilling — which is what actually happened in the
//     first cut of this demo (issue #72 review): a healthy pool's hero
//     screenshot came out with a red "runs dry" banner. A small, mostly
//     flat climb keeps the projected dry time safely past any multi-day
//     reset instead.
func historyStartLong(cur float64) float64 {
	start := cur - 0.01
	if start < 0 {
		start = 0
	}
	return start
}

// seedQuotaHistory backfills reqlog's quota_samples table so the headroom
// chart and the burn-rate/dry-in columns have a past to measure against
// (RecordQuota needs one; a live account has no history on its own). Every
// series climbs from a lower starting point up to the account's actual
// current Used, so the chart, the tanks, and the exact-figures table never
// disagree about where an account stands right now.
func seedQuotaHistory(rl *reqlog.Log, now time.Time, accounts []*pool.Account) {
	const points = 12
	const span = 5*time.Hour + 30*time.Minute
	step := span / (points - 1)
	rnd := rand.New(rand.NewSource(1)) // deterministic: same chart every run

	for _, a := range accounts {
		for _, w := range a.QuotaWindows() {
			if w.Limit <= 0 {
				continue
			}
			cur := w.Used / w.Limit
			short := w.Name == "5h"
			var start float64
			if short {
				start = historyStartShort(cur)
			} else {
				start = historyStartLong(cur)
			}
			for i := 0; i < points; i++ {
				frac := float64(i) / float64(points-1)
				used := start + (cur-start)*frac
				if short {
					// Noise only where the slope is steep enough to carry
					// it — on a long window it would swamp the deliberately
					// tiny climb and reintroduce the false-alarm risk
					// historyStartLong exists to avoid.
					used += (rnd.Float64() - 0.5) * 0.02
				}
				if used < 0 {
					used = 0
				}
				if used > cur || i == points-1 {
					used = cur // never overshoot, and pin the last point exactly
				}
				ts := now.Add(-span + step*time.Duration(i))
				_ = rl.RecordQuota(reqlog.Sample{
					Ts:      ts,
					Account: a.Name,
					Window:  w.Name,
					Limit:   w.Limit,
					Used:    used,
					ResetAt: w.ResetAt,
				})
			}
		}
	}
}

// seedRequests backfills the request log so the requests table and the
// activity histogram have rows to show, instead of "nothing through the
// pool yet".
func seedRequests(rl *reqlog.Log, now time.Time, hold bool) {
	type row struct {
		ago         time.Duration
		account     string
		path        string
		status      int
		durationMs  int64
		bytes       int64
		event       string
		modelAsked  string
		modelServed string
	}

	var rows []row
	if hold {
		rows = []row{
			{42 * time.Minute, "you@acme.example", "/v1/messages", 200, 1240, 8100, reqlog.EventServed, "claude-sonnet-4-5-20250929", "claude-sonnet-4-5-20250929"},
			{35 * time.Minute, "you@labs.example", "/v1/messages", 200, 1890, 14200, reqlog.EventServed, "claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001"},
			{28 * time.Minute, "you@side.example", "/v1/messages", 200, 2210, 19800, reqlog.EventOverage, "claude-sonnet-4-5-20250929", "claude-sonnet-4-5-20250929"},
			{24 * time.Minute, "you@acme.example", "/v1/messages", 429, 190, 0, reqlog.EventRateRetry, "claude-sonnet-4-5-20250929", ""},
			{19 * time.Minute, "you@acme.example", "/v1/messages", 200, 1510, 9700, reqlog.EventRotatedQuota, "claude-sonnet-4-5-20250929", "claude-sonnet-4-5-20250929"},
			{14 * time.Minute, "you@labs.example", "/v1/messages", 200, 1780, 12500, reqlog.EventRotatedQuota, "claude-opus-4-1-20250805", "claude-opus-4-1-20250805"},
			{10 * time.Minute, "you@side.example", "/v1/messages", 200, 2640, 22100, reqlog.EventOverage, "claude-sonnet-4-5-20250929", "claude-sonnet-4-5-20250929"},
			{5 * time.Minute, "you@side.example", "/v1/messages", 429, 340, 0, reqlog.EventExhausted, "claude-sonnet-4-5-20250929", ""},
		}
	} else {
		rows = []row{
			{65 * time.Minute, "you@acme.example", "/v1/messages", 200, 1450, 8412, reqlog.EventServed, "claude-sonnet-4-5-20250929", "claude-sonnet-4-5-20250929"},
			{58 * time.Minute, "you@kimi.example", "/v1/messages", 200, 2100, 15230, reqlog.EventServed, "claude-sonnet-4-5-20250929", "k3"},
			{51 * time.Minute, "you@labs.example", "/v1/messages", 200, 640, 1875, reqlog.EventServed, "claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001"},
			{44 * time.Minute, "you@side.example", "/v1/messages", 200, 1330, 9040, reqlog.EventOverage, "claude-sonnet-4-5-20250929", "claude-sonnet-4-5-20250929"},
			{37 * time.Minute, "you@acme.example", "/v1/messages/count_tokens", 200, 85, 312, reqlog.EventServed, "claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001"},
			{30 * time.Minute, "you@kimi.example", "/v1/messages", 200, 1900, 12004, reqlog.EventServed, "claude-haiku-4-5-20251001", "kimi-for-coding-highspeed"},
			{24 * time.Minute, "you@labs.example", "/v1/messages", 200, 2200, 18820, reqlog.EventServed, "claude-haiku-4-5-fable", "claude-haiku-4-5-fable"},
			{17 * time.Minute, "you@acme.example", "/v1/messages", 429, 210, 0, reqlog.EventRateRetry, "claude-sonnet-4-5-20250929", ""},
			{12 * time.Minute, "you@side.example", "/v1/messages", 200, 3005, 21400, reqlog.EventOverage, "claude-opus-4-1-20250805", "claude-opus-4-1-20250805"},
			{6 * time.Minute, "you@acme.example", "/v1/messages", 200, 1120, 7300, reqlog.EventRotatedQuota, "claude-sonnet-4-5-20250929", "claude-sonnet-4-5-20250929"},
			{2 * time.Minute, "you@kimi.example", "/v1/messages", 200, 980, 5100, reqlog.EventServed, "claude-sonnet-4-5-20250929", "k3"},
		}
	}

	for _, r := range rows {
		_ = rl.Record(reqlog.Entry{
			Ts:          now.Add(-r.ago),
			Account:     r.account,
			Path:        r.path,
			Status:      r.status,
			DurationMs:  r.durationMs,
			Bytes:       r.bytes,
			Event:       r.event,
			ModelAsked:  r.modelAsked,
			ModelServed: r.modelServed,
		})
	}
}

// seedSpillEvents publishes a handful of rotation/hold events on the SSE
// broker each time the number of connected subscribers goes up — Subscribe
// has no replay buffer, so anything published before a browser connects is
// simply lost and the panel would read "No spills yet" no matter what this
// seeds. Firing on a subscriber-count increase, rather than on a timer,
// means every fresh page load gets its own burst within a couple hundred
// milliseconds regardless of when the previous one landed — a fixed-period
// timer left a real gap where a capture taken between bursts saw nothing.
func seedSpillEvents(b *events.Broker, hold bool) {
	var evs []events.Event
	if hold {
		evs = []events.Event{
			{Type: "rotated-quota", Account: "you@acme.example", Detail: "5h window spent, rotated to you@labs.example"},
			{Type: "rotated-quota", Account: "you@labs.example", Detail: "5h window spent, rotated to you@side.example"},
			{Type: "exhausted", Account: "you@side.example", Detail: "extra usage exhausted for this billing period"},
			{Type: "held", Detail: "every account is spent, holding until reset"},
		}
	} else {
		evs = []events.Event{
			{Type: "rotated-quota", Account: "you@labs.example", Detail: "7d-fable window over threshold, preferring you@acme.example"},
			{Type: "rotated-conn", Account: "you@acme.example", Detail: "upstream connection reset before first byte, retried on you@kimi.example"},
			{Type: "recovered", Account: "you@kimi.example", Detail: "credential recovered after a transient 401"},
		}
	}

	last := 0
	for {
		if n := b.Subscribers(); n > last {
			for _, e := range evs {
				e.Ts = time.Now()
				b.Publish(e)
				time.Sleep(150 * time.Millisecond)
			}
			last = n
		} else {
			last = n
		}
		time.Sleep(150 * time.Millisecond)
	}
}
