package config

import (
	"fmt"
	"os"
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
			acct.Upstream = firstNonEmpty(acct.Upstream, cfg.Accounts[i].Upstream)
			cfg.Accounts[i] = acct
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
