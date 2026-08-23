package config

import (
	"os"
	"path/filepath"
	"reflect"
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

// loginOwnedFields are the only AccountConfig fields a login flow is
// entitled to overwrite on re-upsert (#45): the OAuth callers build their
// payload from the exchange result and nothing else. Every field NOT named
// here must survive a re-upsert exactly as the user left it. This list is
// deliberately used only by the test, in the "what login is allowed to
// change" direction: the production merge in accounts.go does not use an
// equivalent list — see its doc comment for why that asymmetry is the
// point.
var loginOwnedFields = map[string]bool{
	"Name":        true, // the match key; always equal on both sides here
	"Type":        true,
	"ExpiresAt":   true,
	"AccountUUID": true,
}

// fillWithSentinels sets every AccountConfig field to a distinctive non-zero
// value, keyed by the field's kind rather than its name, so a field added to
// the struct in the future is picked up automatically. An unhandled kind
// fails loudly rather than silently skipping the new field.
func fillWithSentinels(t *testing.T, acct *AccountConfig) {
	t.Helper()
	v := reflect.ValueOf(acct).Elem()
	rt := v.Type()
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		f := v.Field(i)
		switch name {
		case "Name":
			f.SetString("work")
			continue
		case "Type":
			// Must satisfy Validate's provider.Known check; the value itself
			// is irrelevant to this test since Type is login-owned and gets
			// overwritten by loginPayload regardless.
			f.SetString("claude-oauth")
			continue
		case "Upstream":
			// Must satisfy Validate's absolute-URL check.
			f.SetString("https://sentinel-upstream.example.com")
			continue
		}
		switch f.Kind() {
		case reflect.String:
			f.SetString("sentinel-" + name)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			f.SetInt(4242)
		case reflect.Ptr:
			if f.Type().Elem().Kind() == reflect.Bool {
				b := true
				f.Set(reflect.ValueOf(&b))
				continue
			}
			t.Fatalf("field %s: fillWithSentinels does not know this pointer type (%s); extend it", name, f.Type())
		case reflect.Map:
			m := reflect.MakeMap(f.Type())
			m.SetMapIndex(reflect.ValueOf("k").Convert(f.Type().Key()), reflect.ValueOf("v").Convert(f.Type().Elem()))
			f.Set(m)
		default:
			t.Fatalf("field %s: fillWithSentinels does not know this kind (%s); extend it for the new AccountConfig field", name, f.Kind())
		}
	}
}

// TestUpsertAccountPreservesUserSettingsOnRelogin is issue #45: re-upserting
// an existing account with a login-shaped payload (only what OAuth actually
// learned — type, expiry, provider uuid) must leave every OTHER field
// exactly as the user set it: label, priority, disabled, allowOverage,
// upstream, modelMap, and whatever gets added to AccountConfig next.
//
// Every field is asserted in its own subtest specifically so that breaking
// one field (e.g. priority) fails on its own, not merged into one big
// diff that could hide a partial regression.
func TestUpsertAccountPreservesUserSettingsOnRelogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")

	var original AccountConfig
	fillWithSentinels(t, &original)
	if err := UpsertAccount(path, original); err != nil {
		t.Fatalf("seed UpsertAccount: %v", err)
	}

	// Shaped exactly like runLoginClaude's payload in cmd/spillway/login.go:
	// name (to match), type, expiresAt, accountUuid. Nothing else.
	loginPayload := AccountConfig{
		Name:        "work",
		Type:        "claude-oauth",
		ExpiresAt:   999999999,
		AccountUUID: "uuid-from-relogin",
	}
	if err := UpsertAccount(path, loginPayload); err != nil {
		t.Fatalf("relogin UpsertAccount: %v", err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("accounts after relogin = %+v", cfg.Accounts)
	}
	got := cfg.Accounts[0]

	rt := reflect.TypeOf(AccountConfig{})
	ov := reflect.ValueOf(original)
	lv := reflect.ValueOf(loginPayload)
	gv := reflect.ValueOf(got)
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		t.Run(name, func(t *testing.T) {
			gotVal := gv.Field(i).Interface()
			// Token material is neither login-owned nor preserved: §5 keeps
			// the config to metadata only, so a re-login clears it. See
			// TestUpsertAccountClearsInlineTokenMaterial.
			if name == "AccessToken" || name == "RefreshToken" {
				if gotVal != interface{}("") {
					t.Errorf("%s = %#v, want cleared — token material must not survive in the yaml", name, gotVal)
				}
				return
			}
			if loginOwnedFields[name] {
				want := lv.Field(i).Interface()
				if !reflect.DeepEqual(gotVal, want) {
					t.Errorf("login-owned field %s = %#v, want %#v (what login just learned)", name, gotVal, want)
				}
				return
			}
			want := ov.Field(i).Interface()
			if !reflect.DeepEqual(gotVal, want) {
				t.Errorf("user-set field %s = %#v, want %#v (preserved from before relogin)", name, gotVal, want)
			}
		})
	}
}

// A re-login must not leave token material in the yaml. §5 keeps the config
// to metadata only, and the whole-struct overwrite that mergeLoginUpdate
// replaced enforced that by accident — the login payload carries no tokens,
// so a legacy inline one was zeroed on every login. Preserving every unset
// field would have quietly undone it, and MigrateInlineSecrets does not help
// here: it runs in `spillway server`, not in `spillway login`.
func TestUpsertAccountClearsInlineTokenMaterial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spillway.yaml")

	if err := UpsertAccount(path, AccountConfig{
		Name: "legacy", Type: "claude-oauth",
		AccessToken: "inline-access", RefreshToken: "inline-refresh",
		Label: "keepme", Priority: 3,
	}); err != nil {
		t.Fatal(err)
	}

	// A login-shaped payload: no tokens, as cmd/spillway/login.go builds it.
	if err := UpsertAccount(path, AccountConfig{
		Name: "legacy", Type: "claude-oauth", ExpiresAt: 123, AccountUUID: "u",
	}); err != nil {
		t.Fatal(err)
	}

	accts, err := ListAccountConfigs(path)
	if err != nil {
		t.Fatal(err)
	}
	var got AccountConfig
	for _, a := range accts {
		if a.Name == "legacy" {
			got = a
		}
	}
	if got.AccessToken != "" || got.RefreshToken != "" {
		t.Errorf("token material survived a re-login: access=%q refresh=%q",
			got.AccessToken, got.RefreshToken)
	}
	// And the user's settings still survive — the point of the fix.
	if got.Label != "keepme" || got.Priority != 3 {
		t.Errorf("user settings lost: label=%q priority=%d", got.Label, got.Priority)
	}
	// Raw file check: the yaml itself must not contain the secret.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "inline-access") || strings.Contains(string(raw), "inline-refresh") {
		t.Error("the yaml on disk still contains token material")
	}
}
