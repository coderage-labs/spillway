package config

// Where spillway keeps its files.
//
// os.UserConfigDir() on every platform: %AppData% on Windows,
// ~/Library/Application Support on macOS, $XDG_CONFIG_HOME (or ~/.config)
// elsewhere. Inside it, one directory of our own.

import (
	"fmt"
	"os"
	"path/filepath"
)

// dirName is our subdirectory inside the platform config dir.
const dirName = "spillway"

func configDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config dir: %w", err)
	}
	return filepath.Join(base, dirName), nil
}
