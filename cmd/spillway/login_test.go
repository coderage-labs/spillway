package main

import (
	"errors"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/accounts"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/secrets"
)

func TestListAccounts(t *testing.T) {
	cfg := config.Defaults()
	cfg.Accounts = []config.AccountConfig{
		{Name: "work", Type: "claude-oauth", ExpiresAt: 4102444800000, AccountUUID: "uuid-1"},
		{Name: "old", Type: "claude-oauth", ExpiresAt: 1},
		{Name: "local", Type: "claude-oauth", Source: "keychain"},
		{Name: "ghost", Type: "claude-oauth"},
	}
	store := secrets.NewFake()
	_ = store.Set("work", secrets.Secrets{AccessToken: "a", RefreshToken: "r"})
	_ = store.Set("old", secrets.Secrets{AccessToken: "a", RefreshToken: "r"})

	rows := listAccounts(&cfg, store, nil, time.Now())
	if len(rows) != 4 {
		t.Fatalf("rows = %d", len(rows))
	}
	byName := map[string]accountRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if r := byName["work"]; r.Status != "ok" || r.Secrets != "present" || r.UUID != "uuid-1" {
		t.Errorf("work = %+v", r)
	}
	if r := byName["old"]; r.Status != "expired" {
		t.Errorf("old = %+v", r)
	}
	if r := byName["local"]; r.Secrets != "keychain" || r.Status != "ok" {
		t.Errorf("local = %+v", r)
	}
	if r := byName["ghost"]; r.Status != "no-secrets" {
		t.Errorf("ghost = %+v", r)
	}
}

// A keychain-sourced account is owned and refreshed by the claude CLI, so the
// expiry in our yaml is a snapshot that goes stale within hours. Judging
// status from it reported a healthy account as "expired".
func TestListAccountsUsesLiveKeychainExpiry(t *testing.T) {
	now := time.Date(2026, 8, 22, 4, 0, 0, 0, time.UTC)
	stale := now.Add(-6 * time.Hour).UnixMilli() // what our yaml still says
	fresh := now.Add(3 * time.Hour).UnixMilli()  // what the CLI actually holds

	var cfg config.Config
	cfg.Accounts = []config.AccountConfig{
		{Name: "cli-owned", Type: "claude-oauth", Source: "keychain", ExpiresAt: stale},
	}
	live := func(time.Time) (*accounts.ClaudeOAuth, error) {
		return &accounts.ClaudeOAuth{AccessToken: "t", ExpiresAt: fresh}, nil
	}

	rows := listAccounts(&cfg, secrets.NewFake(), live, now)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Status != "ok" {
		t.Errorf("status = %q, want ok (the live credential is valid)", rows[0].Status)
	}
	if rows[0].ExpiresAt != fresh {
		t.Errorf("expiry = %d, want the live value %d", rows[0].ExpiresAt, fresh)
	}
}

// If the CLI has no login at all, say so rather than showing a stale expiry.
func TestListAccountsReportsMissingKeychainLogin(t *testing.T) {
	now := time.Now()
	var cfg config.Config
	cfg.Accounts = []config.AccountConfig{
		{Name: "cli-owned", Type: "claude-oauth", Source: "keychain", ExpiresAt: now.Add(time.Hour).UnixMilli()},
	}
	live := func(time.Time) (*accounts.ClaudeOAuth, error) { return nil, errors.New("no credentials") }

	rows := listAccounts(&cfg, secrets.NewFake(), live, now)
	if rows[0].Status != "no-secrets" || rows[0].Secrets != "missing" {
		t.Errorf("got status=%q secrets=%q, want no-secrets/missing", rows[0].Status, rows[0].Secrets)
	}
}
