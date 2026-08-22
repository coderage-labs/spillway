package main

// Command demo serves the real dashboard over a crafted pool, so states that
// are hard or expensive to reproduce against live accounts can actually be
// looked at: a parked request, an account on paid extra usage, a pool on its
// last reserve.
//
// It exists because the hold banner shipped rendering as an empty red bar on
// every page load. The bug was one CSS line and would have been obvious in a
// screenshot; it survived because there was no cheap way to produce the state
// that shows it.
//
//	go run ./internal/admin/demo    # then open http://127.0.0.1:7699

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
)

func main() {
	now := time.Now()
	paid := pool.NewAccount("you@side.example", pool.SourceYAML, "t", "", 0, "")
	paid.Type = "claude-oauth"
	paid.SetLabel("side")
	paid.SetOverageForTest(provider.Overage{Known: true, Available: true})
	paid.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.1, ResetAt: now.Add(3 * time.Hour), FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 1, ResetAt: now.Add(13 * time.Hour), FetchedAt: now},
	})
	free := pool.NewAccount("you@acme.example", pool.SourceYAML, "t", "", 0, "")
	free.Type = "claude-oauth"
	free.SetLabel("work")
	free.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.97, ResetAt: now.Add(40 * time.Minute), FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 0.6, ResetAt: now.Add(30 * time.Hour), FetchedAt: now},
	})
	p := pool.New([]*pool.Account{free, paid}, now)
	p.Apply(pool.Settings{AllowOverage: true})
	p.MarkExhausted(paid, now.Add(13*time.Hour))
	release := p.BeginHold(now.Add(2*time.Hour + 14*time.Minute))
	defer release()

	// Settings enabled against a scratch config so the panel renders — it is
	// the only way to see the per-account rows, including the read-only
	// extra-usage state.
	srv := admin.New(p, nil, events.New(), "")
	cfgPath := filepath.Join(os.TempDir(), "spillway-demo.yaml")
	_ = os.WriteFile(cfgPath, []byte(`upstream: https://api.anthropic.com
accounts:
  - name: you@acme.example
    type: claude-oauth
    label: work
  - name: you@side.example
    type: claude-oauth
    label: side
    allowOverage: true
`), 0o600)
	srv.EnableSettings(cfgPath, func(*config.Config) {})

	log.Fatal(http.ListenAndServe("127.0.0.1:7699", srv))
}
