package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadSettingsRefusesUnparseableFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Never clobber a file we cannot parse — the user's settings are not ours
	// to rewrite on a guess.
	if _, err := readSettings(p); err == nil {
		t.Fatal("expected an error for unparseable settings, got nil")
	}
}

func TestWriteSettingsBacksUpAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(p, []byte(`{"model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSettings(p, map[string]any{"model": "opus", "env": map[string]any{"HTTPS_PROXY": "http://x"}}); err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(p + ".spillway.bak")
	if err != nil || string(bak) != `{"model":"opus"}` {
		t.Fatalf("backup missing or wrong: %q %v", bak, err)
	}
	got, err := readSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != "opus" {
		t.Errorf("round-trip lost settings: %v", got)
	}
	var raw map[string]any
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("wrote invalid JSON: %v", err)
	}
}

func TestStatuslineInstallGuardsForeignEntry(t *testing.T) {
	settings := map[string]any{
		"statusLine": map[string]any{"type": "command", "command": "~/.claude/other.sh"},
	}
	cur, _ := settings["statusLine"].(map[string]any)
	existing, _ := cur["command"].(string)
	if isOurs(existing) {
		t.Fatalf("a foreign status line must not be treated as ours: %q", existing)
	}
}

// Uninstall must leave unrelated settings untouched.
func TestStatuslineUninstallPreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(p, []byte(`{"model":"opus","statusLine":{"type":"command","command":"/x/spillway statusline"},"env":{"A":"b"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := readSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	delete(m, "statusLine")
	if err := writeSettings(p, m); err != nil {
		t.Fatal(err)
	}
	got, err := readSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, still := got["statusLine"]; still {
		t.Error("statusLine survived removal")
	}
	if got["model"] != "opus" || got["env"] == nil {
		t.Errorf("unrelated settings lost: %v", got)
	}
}
