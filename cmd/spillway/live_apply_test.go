package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
)

// liveTestConfig writes a scratch spillway.yaml naming addr as the admin
// listener and the given accounts, points SPILLWAY_CONFIG at it, and
// returns its path. Accounts are seeded via config.UpsertAccount so the
// file round-trips through the same code the real CLI does.
func liveTestConfig(t *testing.T, addr string, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "spillway.yaml")
	if err := os.WriteFile(path, []byte("admin:\n  addr: "+addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := config.UpsertAccount(path, config.AccountConfig{Name: n, Type: "claude-oauth"}); err != nil {
			t.Fatalf("seed account %q: %v", n, err)
		}
	}
	t.Setenv("SPILLWAY_CONFIG", path)
	return path
}

// TestLiveApplyAccountEditAppliesPriorityToRunningPool is the regression
// test for issue #83's priority half: `accounts priority` used to write the
// config and tell the operator to restart. This drives a REAL pool.Pool
// behind a REAL admin.Server (the same wiring cmd/spillway/main.go builds),
// calls setPriority exactly as the CLI does, and then proves the change
// reached the pool by asserting a selection outcome — not by reading the
// config back.
func TestLiveApplyAccountEditAppliesPriorityToRunningPool(t *testing.T) {
	primary := pool.NewAccount("primary", pool.SourceYAML, "t", "", 0, "")
	primary.Type = "claude-oauth"
	fallback := pool.NewAccount("fallback", pool.SourceYAML, "t", "", 0, "")
	fallback.Type = "claude-oauth"
	p := pool.New([]*pool.Account{primary, fallback}, time.Now())

	srv := admin.New(p, nil, events.New(), "")
	front := httptest.NewServer(srv)
	defer front.Close()

	cfgPath := liveTestConfig(t, strings.TrimPrefix(front.URL, "http://"), "primary", "fallback")
	srv.EnableSettings(cfgPath, func(nc *config.Config) { p.Apply(poolSettings(nc)) })

	// Make the primary account the busy one: without priority actually
	// reaching the pool, load alone would hand selection to fallback. Built
	// up via Pin (a public API) rather than reaching into the account, since
	// this test lives outside the pool package.
	if err := p.Pin("primary", false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got := p.SelectFor(fmt.Sprintf("warmup-%d", i), nil); got == nil || got.Name != "primary" {
			t.Fatalf("warmup selection = %v, want primary", got)
		}
	}
	p.Unpin()

	if err := setPriority(cfgPath, []string{"fallback", "5"}); err != nil {
		t.Fatalf("setPriority: %v", err)
	}

	got := p.Select("session-a")
	if got == nil {
		t.Fatal("nothing selected")
	}
	if got.Name != "primary" {
		t.Fatalf("selected %q; primary's better priority must still win despite load — priority did not reach the running pool", got.Name)
	}
}

// TestLiveApplyAccountEditAppliesOverageToRunningPool is #83's money-shaped
// case named explicitly in the issue: overage must apply live, in both
// directions, or a user who just turned billing off keeps being billed
// until they restart.
func TestLiveApplyAccountEditAppliesOverageToRunningPool(t *testing.T) {
	now := time.Now()
	billable := pool.NewAccount("billable", pool.SourceYAML, "t", "", 0, "")
	billable.Type = "claude-oauth"
	frugal := pool.NewAccount("frugal", pool.SourceYAML, "t", "", 0, "")
	frugal.Type = "claude-oauth"
	p := pool.New([]*pool.Account{billable, frugal}, now)
	// Both exhausted with overage available upstream — tiers one and two of
	// SelectExcept require StateOK, so both fail for both accounts and only
	// the third (paid) tier can return anything, and only for an account
	// CanOverage says yes for.
	p.MarkExhausted(billable, now.Add(time.Hour))
	p.MarkExhausted(frugal, now.Add(time.Hour))
	billable.SetOverageForTest(provider.Overage{Known: true, Available: true})
	frugal.SetOverageForTest(provider.Overage{Known: true, Available: true})

	srv := admin.New(p, nil, events.New(), "")
	front := httptest.NewServer(srv)
	defer front.Close()

	cfgPath := liveTestConfig(t, strings.TrimPrefix(front.URL, "http://"), "billable", "frugal")
	srv.EnableSettings(cfgPath, func(nc *config.Config) { p.Apply(poolSettings(nc)) })

	// Pool-wide overage stays off; only "billable" opts in explicitly.
	if err := setOverage(cfgPath, []string{"billable", "on"}); err != nil {
		t.Fatalf("setOverage on: %v", err)
	}
	if got := p.SelectFor("s1", nil); got == nil || got.Name != "billable" {
		t.Fatalf("SelectFor after enabling overage = %v, want billable", got)
	}
	p.Done(billable)

	// Now flip it back off and confirm the running pool actually stops
	// billing — the exact scenario #83 calls out: turning it off must not
	// keep spending money until a restart.
	if err := setOverage(cfgPath, []string{"billable", "off"}); err != nil {
		t.Fatalf("setOverage off: %v", err)
	}
	if got := p.SelectFor("s2", nil); got != nil {
		t.Fatalf("SelectFor after disabling overage = %v, want nil (nothing should be willing to bill)", got)
	}
}

// TestLiveRemoveAccountTakesEffectOnRunningPool is the end-to-end version of
// issue #83's original report: `accounts remove` reaching a real admin
// server backed by a real pool, asserted against selection.
func TestLiveRemoveAccountTakesEffectOnRunningPool(t *testing.T) {
	gone := pool.NewAccount("gone", pool.SourceYAML, "t", "", 0, "")
	gone.Type = "claude-oauth"
	stays := pool.NewAccount("stays", pool.SourceYAML, "t", "", 0, "")
	stays.Type = "claude-oauth"
	p := pool.New([]*pool.Account{gone, stays}, time.Now())

	srv := admin.New(p, nil, events.New(), "")
	front := httptest.NewServer(srv)
	defer front.Close()
	liveTestConfig(t, strings.TrimPrefix(front.URL, "http://"), "gone", "stays")

	msg := liveRemoveAccount("gone")
	if msg == "" || strings.Contains(strings.ToLower(msg), "not removed") {
		t.Fatalf("unexpected message from a reachable daemon: %q", msg)
	}

	for i := 0; i < 20; i++ {
		got := p.SelectFor(fmt.Sprintf("s-%d", i), nil)
		if got != nil && got.Name == "gone" {
			t.Fatalf("removed account is still selectable after liveRemoveAccount")
		}
	}
}

// TestLiveRemoveAccountDaemonDownIsCleanSuccess is #83's other explicit
// requirement: a down daemon must stay a clean success, not an error, and
// must not claim to have applied live when it did not.
func TestLiveRemoveAccountDaemonDownIsCleanSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close() // nothing answers here now

	liveTestConfig(t, addr, "solo")

	msg := liveRemoveAccount("solo")
	if msg == "" {
		t.Fatal("want a message explaining nothing is running, got silence")
	}
	if strings.Contains(strings.ToLower(msg), "immediately") {
		t.Errorf("message must not claim live application when the daemon is unreachable: %q", msg)
	}
}

// TestLiveAddAccountTakesEffectOnRunningPool is issue #87's mirror of
// TestLiveRemoveAccountTakesEffectOnRunningPool: a brand-new account posted
// via liveAddAccount (the call login.go makes right after writing the
// config and secret store) must be selectable on the running pool
// immediately — asserted against selection, not a response flag.
func TestLiveAddAccountTakesEffectOnRunningPool(t *testing.T) {
	existing := pool.NewAccount("existing", pool.SourceYAML, "t", "", 0, "")
	existing.Type = "claude-oauth"
	p := pool.New([]*pool.Account{existing}, time.Now())

	srv := admin.New(p, nil, events.New(), "")
	front := httptest.NewServer(srv)
	defer front.Close()
	liveTestConfig(t, strings.TrimPrefix(front.URL, "http://"), "existing")

	msg := liveAddAccount(accountAddPayload{
		Name: "fresh", Type: "claude-oauth", AccessToken: "new-tok", ExpiresAt: 12345,
	})
	if msg == "" || strings.Contains(strings.ToLower(msg), "not applied") {
		t.Fatalf("unexpected message from a reachable daemon: %q", msg)
	}

	if err := p.Pin("fresh", false); err != nil {
		t.Fatalf("the added account is not resolvable by the running pool: %v", err)
	}
	got := p.SelectFor("s", nil)
	if got == nil || got.Name != "fresh" {
		t.Fatalf("SelectFor while pinned to the added account = %v, want fresh", got)
	}
}

// TestLiveAddAccountReauthHotSwapsOnRunningDaemon: re-authenticating an
// existing account (same name posted twice) must update its credentials on
// the running pool in place — issue #87's fold-in of #46's re-auth gap —
// rather than only printing a "restart to fix it" notice.
func TestLiveAddAccountReauthHotSwapsOnRunningDaemon(t *testing.T) {
	work := pool.NewAccount("work", pool.SourceYAML, "old-tok", "old-refresh", 1, "")
	work.Type = "claude-oauth"
	work.Disable()
	p := pool.New([]*pool.Account{work}, time.Now())

	srv := admin.New(p, nil, events.New(), "")
	front := httptest.NewServer(srv)
	defer front.Close()
	liveTestConfig(t, strings.TrimPrefix(front.URL, "http://"), "work")

	msg := liveAddAccount(accountAddPayload{
		Name: "work", Type: "claude-oauth", AccessToken: "new-tok", RefreshToken: "new-refresh", ExpiresAt: 999,
	})
	if msg == "" || strings.Contains(strings.ToLower(msg), "not applied") {
		t.Fatalf("unexpected message from a reachable daemon: %q", msg)
	}
	if len(p.Accounts()) != 1 {
		t.Fatalf("re-auth created a duplicate: %d accounts", len(p.Accounts()))
	}
	tok, refresh, expiresAt := work.Credentials()
	if tok != "new-tok" || refresh != "new-refresh" || expiresAt != 999 {
		t.Errorf("credentials not hot-swapped on the running pool: got (%q, %q, %d)", tok, refresh, expiresAt)
	}
	if work.State() != pool.StateOK {
		t.Errorf("re-auth must revive the account on the running pool, got state %v", work.State())
	}
}

// TestLiveAddAccountDaemonDownIsCleanSuccess is #87's explicit requirement,
// mirroring TestLiveRemoveAccountDaemonDownIsCleanSuccess: a down daemon
// must stay a clean success, not an error, and must not claim a live
// update happened when it did not.
func TestLiveAddAccountDaemonDownIsCleanSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close() // nothing answers here now

	liveTestConfig(t, addr, "solo")

	msg := liveAddAccount(accountAddPayload{Name: "fresh", Type: "claude-oauth", AccessToken: "t"})
	if msg == "" {
		t.Fatal("want a message explaining nothing is running, got silence")
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "immediately") || strings.Contains(lower, "added to the running daemon") {
		t.Errorf("message must not claim live application when the daemon is unreachable: %q", msg)
	}
	if !strings.Contains(lower, "no daemon") && !strings.Contains(lower, "not applied") {
		t.Errorf("message does not say live application did not happen: %q", msg)
	}
}

// TestLiveAddAccountCustomUpstreamReportsRestartRequired is the CLI-visible
// half of #87's "still restart-only" case: a custom, non-provider-default
// upstream must surface a restart requirement in the printed message rather
// than silently claiming full success.
func TestLiveAddAccountCustomUpstreamReportsRestartRequired(t *testing.T) {
	p := pool.New(nil, time.Now())
	srv := admin.New(p, nil, events.New(), "")
	front := httptest.NewServer(srv)
	defer front.Close()
	liveTestConfig(t, strings.TrimPrefix(front.URL, "http://"))

	msg := liveAddAccount(accountAddPayload{
		Name: "custom-1", Type: "claude-oauth", Upstream: "https://custom.example.com/v1", AccessToken: "t",
	})
	lower := strings.ToLower(msg)
	if !strings.Contains(lower, "restart") {
		t.Errorf("message must say a restart is needed for the custom upstream host: %q", msg)
	}
	if strings.Contains(msg, "custom.example.com") == false {
		t.Errorf("message does not name the host needing a restart: %q", msg)
	}
	// Still selectable right now — the restart caveat is CONNECT-mode only.
	if err := p.Pin("custom-1", false); err != nil {
		t.Fatalf("custom-upstream account must still be selectable immediately: %v", err)
	}
}

// TestSetPriorityCleanSuccessWithDaemonDown: the config write must still
// succeed and the command must still return nil even though nothing is
// listening — restart is a fallback, never a requirement.
func TestSetPriorityCleanSuccessWithDaemonDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()

	cfgPath := liveTestConfig(t, addr, "work")

	out := captureStdout(t, func() {
		if err := setPriority(cfgPath, []string{"work", "3"}); err != nil {
			t.Fatalf("setPriority returned an error with the daemon down: %v", err)
		}
	})
	if !strings.Contains(strings.ToLower(out), "no daemon") && !strings.Contains(strings.ToLower(out), "not applied") {
		t.Errorf("output does not say live application did not happen: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "immediately") {
		t.Errorf("output must not claim live application when the daemon is unreachable: %q", out)
	}

	cfg, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].Priority != 3 {
		t.Fatalf("priority was not durably written to disk: %+v", cfg.Accounts)
	}
}
