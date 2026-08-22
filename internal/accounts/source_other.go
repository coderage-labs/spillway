//go:build !darwin

package accounts

import (
	"os"
	"path/filepath"
)

// DefaultSource reads Claude Code's OAuth credentials from
// ~/.claude/.credentials.json (Linux layout).
func DefaultSource() Source {
	return SourceFunc(func() ([]byte, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	})
}
