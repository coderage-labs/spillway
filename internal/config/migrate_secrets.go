package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/coderage-labs/spillway/internal/notify"
	"github.com/coderage-labs/spillway/internal/secrets"
)

// MigrateInlineSecrets moves any inline token material in the config file to
// the secret store and scrubs it from the file (atomic 0600 rewrite): both
// account credentials (§5) and, since issue #101, notify channel
// credentials someone hand-edited into the yaml. Returns the migrated
// account and channel names; both nil when nothing needed migrating.
func MigrateInlineSecrets(path string, store secrets.Store) (migratedAccounts, migratedChannels []string, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		if a.AccessToken == "" && a.RefreshToken == "" {
			continue
		}
		if err := store.Set(a.Name, secrets.Secrets{
			AccessToken:  a.AccessToken,
			RefreshToken: a.RefreshToken,
		}); err != nil {
			return nil, nil, fmt.Errorf("store secrets for account %q: %w", a.Name, err)
		}
		a.AccessToken = ""
		a.RefreshToken = ""
		migratedAccounts = append(migratedAccounts, a.Name)
	}
	for i := range cfg.Notify.Channels {
		c := &cfg.Notify.Channels[i]
		if c.URL == "" && c.Topic == "" && c.Token == "" && c.UserKey == "" {
			continue
		}
		blob, err := json.Marshal(notify.Destination{
			URL: c.URL, Topic: c.Topic, Token: c.Token, UserKey: c.UserKey,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("marshal channel secret for %q: %w", c.Name, err)
		}
		if err := store.SetRaw(notify.ChannelKey(c.Name), blob); err != nil {
			return nil, nil, fmt.Errorf("store channel secret for %q: %w", c.Name, err)
		}
		c.URL, c.Topic, c.Token, c.UserKey = "", "", "", ""
		migratedChannels = append(migratedChannels, c.Name)
	}
	if len(migratedAccounts) == 0 && len(migratedChannels) == 0 {
		return nil, nil, nil
	}
	if err := writeFile(path, cfg); err != nil {
		return nil, nil, err
	}
	return migratedAccounts, migratedChannels, nil
}
