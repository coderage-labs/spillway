package main

// Reading and writing ~/.claude/settings.json.
//
// Used by `spillway statusline install`, which adds a statusLine entry. These
// lived alongside the removed `hook` command; the file editing was never
// specific to it.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func claudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func readSettings(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		// Never clobber a file we cannot parse — the user's settings are not
		// ours to rewrite on a guess.
		return nil, fmt.Errorf("parse %s: %w (fix or move the file, then retry)", path, err)
	}
	return m, nil
}

// writeSettings writes atomically, keeping a .bak of any previous content.
func writeSettings(path string, m map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if prev, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".spillway.bak", prev, 0o600); err != nil {
			return fmt.Errorf("write backup: %w", err)
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
