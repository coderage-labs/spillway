package config

import (
	"fmt"
	"os"

	"github.com/coderage-labs/spillway/internal/secrets"
)

// MigrateInlineSecrets moves any inline token material in the config file to
// the secret store and scrubs it from the file (atomic 0600 rewrite).
// Returns the migrated account names; nil when nothing needed migrating.
func MigrateInlineSecrets(path string, store secrets.Store) ([]string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	var migrated []string
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		if a.AccessToken == "" && a.RefreshToken == "" {
			continue
		}
		if err := store.Set(a.Name, secrets.Secrets{
			AccessToken:  a.AccessToken,
			RefreshToken: a.RefreshToken,
		}); err != nil {
			return nil, fmt.Errorf("store secrets for account %q: %w", a.Name, err)
		}
		a.AccessToken = ""
		a.RefreshToken = ""
		migrated = append(migrated, a.Name)
	}
	if len(migrated) == 0 {
		return nil, nil
	}
	if err := writeFile(path, cfg); err != nil {
		return nil, err
	}
	return migrated, nil
}
