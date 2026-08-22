package main

// One place that decides where token material lives, so every entry point
// agrees. Six call sites each constructed their own keychain, which was fine
// while there was only one answer.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/secrets"
)

var warnFallbackOnce sync.Once

// secretsFallbackPath is where secrets go when there is no keychain: beside
// the config, which is already 0700 and already the private directory for
// this machine's spillway state.
func secretsFallbackPath() string {
	cfgPath, err := config.Path()
	if err != nil {
		return "spillway-secrets.json"
	}
	return filepath.Join(filepath.Dir(cfgPath), "spillway-secrets.json")
}

// openSecrets returns the store, saying so once if it is the weaker one.
// Loud on purpose: a token moving from the OS keychain to a file on disk is
// not something to discover later from a file listing.
func openSecrets() secrets.Store {
	path := secretsFallbackPath()
	store, why := secrets.Open(path)
	if why != "" {
		warnFallbackOnce.Do(func() {
			fmt.Fprintf(os.Stderr,
				"spillway: no OS keychain available here (%s)\n"+
					"  storing tokens in %s (0600) instead — this is plaintext on disk,\n"+
					"  the same as the claude CLI does on Linux. Start a desktop keyring\n"+
					"  (gnome-keyring, KWallet) to use it instead.\n", why, path)
		})
	}
	return store
}
