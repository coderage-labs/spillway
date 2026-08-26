package config

import (
	"fmt"
	"os"
	"reflect"
)

// UpsertAccount adds or replaces (by name) an account's metadata in the
// config file, atomically at 0600. Token material is never written here.
func UpsertAccount(path string, acct AccountConfig) error {
	cfg, err := readOrDefaults(path)
	if err != nil {
		return err
	}
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Name == acct.Name {
			cfg.Accounts[i] = mergeLoginUpdate(cfg.Accounts[i], acct)
			return validateAndWrite(path, cfg)
		}
	}
	// A second login to the same provider account under a different name is
	// almost always a mistake, and a quiet one: the pool believes it has an
	// extra account, rotates onto a tank backed by the same quota it just
	// left, and — worst — runs two refreshers against one credential, which
	// is how a live token got killed early in this project.
	if dup := findByUUID(cfg.Accounts, acct.AccountUUID); dup != "" {
		return fmt.Errorf("that is the same provider account as %q (account uuid %s)\n"+
			"  to re-authenticate it:  spillway login claude %s\n"+
			"  to replace it:          spillway accounts remove %s",
			dup, acct.AccountUUID, dup, dup)
	}
	cfg.Accounts = append(cfg.Accounts, acct)
	return validateAndWrite(path, cfg)
}

// RemoveAccount deletes an account by name. Missing is an error — a typo'd
// remove should be visible.
func RemoveAccount(path, name string) error {
	cfg, err := readOrDefaults(path)
	if err != nil {
		return err
	}
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Name == name {
			cfg.Accounts = append(cfg.Accounts[:i], cfg.Accounts[i+1:]...)
			return validateAndWrite(path, cfg)
		}
	}
	return fmt.Errorf("account %q not found in %s", name, path)
}

// ListAccountConfigs returns the accounts recorded in the config file at
// path. Exported for #44's shared name resolver: `login`, `accounts
// remove/priority/overage` all need to resolve a name against the config
// with no daemon running, and a read for that purpose has no business
// creating the file if it's missing — unlike LoadFrom, this reuses
// readOrDefaults and never writes.
func ListAccountConfigs(path string) ([]AccountConfig, error) {
	cfg, err := readOrDefaults(path)
	if err != nil {
		return nil, err
	}
	return cfg.Accounts, nil
}

// FindAccountByUUID reports the existing account with this provider account
// uuid, or "". Exported so login can refuse a duplicate BEFORE writing token
// material: UpsertAccount rejects it too, but by then the secret is already
// in the keychain under a name no config will ever reference again.
func FindAccountByUUID(path, uuid string) (string, error) {
	if uuid == "" {
		return "", nil
	}
	cfg, err := readOrDefaults(path)
	if err != nil {
		return "", err
	}
	return findByUUID(cfg.Accounts, uuid), nil
}

// findByUUID returns the name of an existing account with this provider
// account uuid, or "". An empty uuid matches nothing: providers that do not
// report one (kimi) must not all collide with each other.
func findByUUID(accts []AccountConfig, uuid string) string {
	if uuid == "" {
		return ""
	}
	for _, a := range accts {
		if a.AccountUUID == uuid {
			return a.Name
		}
	}
	return ""
}

func readOrDefaults(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		d := Defaults()
		return &d, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

func validateAndWrite(path string, cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("merged config invalid: %w", err)
	}
	return writeFile(path, cfg)
}

// mergeLoginUpdate returns existing with every field acct actually set (left
// at a non-zero value) applied on top of it. This exists because of #45:
// UpsertAccount used to replace the whole struct on re-login, so the login
// callers — which build their payload from nothing but the OAuth result
// (type, expiresAt, accountUuid) — silently wiped every field the user had
// configured by hand: label, priority, disabled, allowOverage, upstream,
// modelMap. Re-authenticating is the documented recovery for a dead
// credential, so that fired exactly when someone was already having a bad
// day, and one of the wiped fields (allowOverage) is the one that costs
// money.
//
// Reflection over zero-vs-non-zero, rather than a hand-maintained list of
// "fields login is allowed to touch", is deliberate: a field added to
// AccountConfig tomorrow defaults to preserved without this function
// changing. Login has to opt a field IN by actually setting it to something
// non-zero — which is exactly what it does today for type/expiresAt/uuid,
// and for upstream on the kimi path. The one place this needs a second look
// is if a future field's zero value is itself a meaningful "set to nothing"
// (e.g. explicitly un-labelling an account by setting label back to "");
// UpsertAccount cannot express that today, and did not before this change
// either — SetAccountPriority/SetAccountOverage exist precisely because
// those two already needed to say "set to exactly this, including zero".
func mergeLoginUpdate(existing, acct AccountConfig) AccountConfig {
	merged := existing
	mv := reflect.ValueOf(&merged).Elem()
	av := reflect.ValueOf(acct)
	for i := 0; i < av.NumField(); i++ {
		if f := av.Field(i); !f.IsZero() {
			mv.Field(i).Set(f)
		}
	}
	// Token material is the exception, and it is cleared rather than
	// preserved. §5 is that the config holds metadata only, and the whole-
	// struct overwrite this replaced happened to enforce that by accident:
	// the login payload carries no tokens, so a legacy inline token in the
	// yaml was zeroed every time. Preserving everything unset would quietly
	// undo that. MigrateInlineSecrets scrubs these at daemon startup, but it
	// runs in `spillway server` only — someone re-authenticating with the
	// daemon down would otherwise keep a plaintext token on disk.
	merged.AccessToken = ""
	merged.RefreshToken = ""
	// Source is the second exception (#81), same shape as the one above: a
	// successful login means spillway now holds its own OAuth grant for this
	// account, which is the literal definition of "not borrowed" —
	// source: keychain must not survive it. The login payload never sets
	// Source (it is not something OAuth exchange learns), so relying on
	// non-zero-copy would leave a stale "keychain" entry silently routing
	// around the credential login just wrote — the exact bug reported in
	// #81's comment: a fresh spillway-owned grant sitting unused while the
	// account keeps borrowing (and eventually loses) the CLI's own token.
	//
	// Audited the rest of AccountConfig for the same shape while here, per
	// #81's ask (this is the third bug of this class, after #50/#45): Label,
	// Disabled, Priority, AllowOverage, Upstream and ModelMap are all
	// genuinely fine to preserve — none of them are things a login flow
	// learns or invalidates, so the zero-value-means-untouched default is
	// correct for all of them. Only credential-shaped fields (token
	// material, and now Source) need an exception.
	merged.Source = ""
	return merged
}

// SetAccountOverage sets, clears or unsets an account's allowOverage.
//
// Deliberately not routed through UpdateSettings: that path is reachable from
// the dashboard, and this is the one setting that makes spillway spend money.
// Editing it takes a command, so the friction is proportional to the
// consequence. v == nil removes the key, restoring "follow the pool".
func SetAccountOverage(path, name string, v *bool) error {
	cfg, err := readOrDefaults(path)
	if err != nil {
		return err
	}
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Name != name {
			continue
		}
		cfg.Accounts[i].AllowOverage = v
		return validateAndWrite(path, cfg)
	}
	return fmt.Errorf("account %q not found in %s", name, path)
}

// SetAccountPriority sets one account's selection priority; lower is
// preferred. Unlike overage this is not a money decision, so it takes any
// integer and says nothing dire about it.
func SetAccountPriority(path, name string, prio int) error {
	cfg, err := readOrDefaults(path)
	if err != nil {
		return err
	}
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Name != name {
			continue
		}
		cfg.Accounts[i].Priority = prio
		return validateAndWrite(path, cfg)
	}
	return fmt.Errorf("account %q not found in %s", name, path)
}
