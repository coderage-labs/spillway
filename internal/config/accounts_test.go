package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coderage-labs/spillway/internal/testmode"
)

func TestUpsertAndRemoveAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")

	// Creates the file from defaults.
	if err := UpsertAccount(path, AccountConfig{
		Name: "work", Type: "claude-oauth", ExpiresAt: 4102444800000,
		AccountUUID: "11111111-2222-3333-4444-555555555555",
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].Name != "work" || cfg.Accounts[0].AccountUUID == "" {
		t.Errorf("accounts = %+v", cfg.Accounts)
	}
	if cfg.Proxy.Port != Defaults().Proxy.Port {
		t.Error("defaults not preserved on create")
	}

	// Upsert same name updates in place, preserving a custom upstream.
	if err := UpsertAccount(path, AccountConfig{Name: "work", Type: "claude-oauth", Upstream: "https://custom.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertAccount(path, AccountConfig{Name: "work", Type: "claude-oauth", ExpiresAt: 99}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = LoadFrom(path)
	if len(cfg.Accounts) != 1 {
		t.Fatalf("upsert duplicated: %+v", cfg.Accounts)
	}
	if cfg.Accounts[0].Upstream != "https://custom.example.com" || cfg.Accounts[0].ExpiresAt != 99 {
		t.Errorf("upsert lost fields: %+v", cfg.Accounts[0])
	}

	// No token material ever lands in the file.
	raw, _ := os.ReadFile(path)
	for _, tok := range []string{"accessToken", "refreshToken"} {
		if strings.Contains(string(raw), tok) {
			t.Errorf("yaml contains %q", tok)
		}
	}

	if err := RemoveAccount(path, "work"); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	cfg, _ = LoadFrom(path)
	if len(cfg.Accounts) != 0 {
		t.Errorf("accounts after remove = %+v", cfg.Accounts)
	}
	if err := RemoveAccount(path, "work"); err == nil {
		t.Error("removing a missing account should error")
	}

	testmode.AssertPrivateFile(t, path)
}

// A second login to the same provider account under a different name looks
// like a new account and is not one. The pool would rotate between tanks
// backed by one quota, and refresh one credential from two places.
func TestUpsertRefusesTheSameProviderAccountTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	first := AccountConfig{
		Name: "work", Type: "claude-oauth",
		AccountUUID: "11111111-2222-3333-4444-555555555555",
	}
	if err := UpsertAccount(path, first); err != nil {
		t.Fatal(err)
	}

	dup := first
	dup.Name = "test"
	err := UpsertAccount(path, dup)
	if err == nil {
		t.Fatal("the same provider account was added twice under different names")
	}
	// Validate would reject this too, so the message is the only thing the
	// Upsert-level check adds — and it is the one the user sees the instant
	// they finish an OAuth flow. It must name the existing account and both
	// ways forward; "merged config invalid: accounts[1]" does not.
	for _, want := range []string{"work", first.AccountUUID,
		"spillway login claude work", "accounts remove work"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got:\n%v", want, err)
		}
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Accounts) != 1 {
		t.Errorf("config has %d accounts, want the duplicate rejected", len(cfg.Accounts))
	}
}

// Re-authenticating under the SAME name must still work: that is the normal
// way to replace a dead refresh token.
func TestUpsertAllowsReAuthUnderTheSameName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	a := AccountConfig{Name: "work", Type: "claude-oauth", AccountUUID: "u-1", ExpiresAt: 1}
	if err := UpsertAccount(path, a); err != nil {
		t.Fatal(err)
	}
	a.ExpiresAt = 2
	if err := UpsertAccount(path, a); err != nil {
		t.Fatalf("re-login under the same name was refused: %v", err)
	}
	cfg, _ := LoadFrom(path)
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].ExpiresAt != 2 {
		t.Errorf("re-auth did not replace the entry: %+v", cfg.Accounts)
	}
}

// Providers that report no uuid must not all collide with each other.
func TestUpsertDoesNotDedupeAccountsWithoutAUUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	for _, n := range []string{"kimi-one", "kimi-two"} {
		if err := UpsertAccount(path, AccountConfig{Name: n, Type: "kimi-oauth"}); err != nil {
			t.Fatalf("account %q refused: %v", n, err)
		}
	}
	cfg, _ := LoadFrom(path)
	if len(cfg.Accounts) != 2 {
		t.Errorf("got %d accounts, want both — an empty uuid identifies nothing", len(cfg.Accounts))
	}
}

// A hand-edited config with the duplicate already in it must fail loudly at
// load, not start a second refresher for the same credential.
func TestValidateRejectsDuplicateProviderAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	yaml := `upstream: https://api.anthropic.com
accounts:
  - name: work
    type: claude-oauth
    accountUuid: same-uuid
  - name: test
    type: claude-oauth
    accountUuid: same-uuid
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("a config with one provider account listed twice loaded cleanly")
	}
	if !strings.Contains(err.Error(), "same provider account") {
		t.Errorf("error %q does not explain the problem", err)
	}
}

func TestSetAccountOverageRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	if err := UpsertAccount(path, AccountConfig{Name: "a", Type: "claude-oauth"}); err != nil {
		t.Fatal(err)
	}
	get := func() *bool {
		cfg, err := LoadFrom(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Accounts[0].AllowOverage
	}
	if get() != nil {
		t.Fatal("a new account should follow the pool, not carry a value")
	}

	yes := true
	if err := SetAccountOverage(path, "a", &yes); err != nil {
		t.Fatal(err)
	}
	if v := get(); v == nil || !*v {
		t.Errorf("allowOverage = %v, want true", v)
	}

	no := false
	if err := SetAccountOverage(path, "a", &no); err != nil {
		t.Fatal(err)
	}
	// false is not the same as unset: it overrides a pool-wide yes.
	if v := get(); v == nil || *v {
		t.Errorf("allowOverage = %v, want an explicit false", v)
	}

	if err := SetAccountOverage(path, "a", nil); err != nil {
		t.Fatal(err)
	}
	if v := get(); v != nil {
		t.Errorf("allowOverage = %v, want the key removed so the pool decides", v)
	}
}

func TestSetAccountOverageRejectsAnUnknownAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	if err := UpsertAccount(path, AccountConfig{Name: "a", Type: "claude-oauth"}); err != nil {
		t.Fatal(err)
	}
	yes := true
	err := SetAccountOverage(path, "typo", &yes)
	if err == nil {
		t.Fatal("a typo'd account name silently did nothing")
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("error %q should name the account", err)
	}
}
