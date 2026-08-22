//go:build darwin

package accounts

import (
	"fmt"
	"os/user"

	"github.com/zalando/go-keyring"
)

const claudeKeychainService = "Claude Code-credentials"

// DefaultSource reads Claude Code's OAuth credentials from the macOS
// Keychain (item written by the official CLI on login).
func DefaultSource() Source {
	return SourceFunc(func() ([]byte, error) {
		u, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("resolve current user: %w", err)
		}
		v, err := keyring.Get(claudeKeychainService, u.Username)
		if err != nil {
			return nil, err
		}
		return []byte(v), nil
	})
}
