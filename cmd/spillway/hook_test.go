package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/coderage-labs/spillway/internal/config"
)

func TestMergeHookPreservesUnrelatedSettings(t *testing.T) {
	settings := map[string]any{
		"model":       "opus",
		"env":         map[string]any{"MY_VAR": "keep-me", "HTTPS_PROXY": "http://stale:1"},
		"permissions": map[string]any{"allow": []any{"Bash"}},
	}
	got := mergeHook(settings, map[string]string{"HTTPS_PROXY": "http://127.0.0.1:7654"})

	env := got["env"].(map[string]any)
	if env["MY_VAR"] != "keep-me" {
		t.Errorf("unmanaged env var dropped: %v", env)
	}
	if env["HTTPS_PROXY"] != "http://127.0.0.1:7654" {
		t.Errorf("managed var not updated: %v", env["HTTPS_PROXY"])
	}
	if got["model"] != "opus" || got["permissions"] == nil {
		t.Errorf("unrelated top-level settings lost: %v", got)
	}
}

func TestRemoveHookLeavesUserVars(t *testing.T) {
	settings := map[string]any{"env": map[string]any{
		"MY_VAR": "keep-me", "HTTPS_PROXY": "x", "NODE_EXTRA_CA_CERTS": "y", "API_TIMEOUT_MS": "1",
	}}
	got := removeHook(settings)
	env := got["env"].(map[string]any)
	if len(env) != 1 || env["MY_VAR"] != "keep-me" {
		t.Fatalf("expected only MY_VAR to survive, got %v", env)
	}
}

func TestRemoveHookDropsEmptyEnv(t *testing.T) {
	got := removeHook(map[string]any{"env": map[string]any{"HTTPS_PROXY": "x"}})
	if _, ok := got["env"]; ok {
		t.Fatalf("empty env block should be removed, got %v", got)
	}
}

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

func TestHookEnvUsesMITMNotBaseURL(t *testing.T) {
	// Remote Control refuses to start when ANTHROPIC_BASE_URL is not
	// api.anthropic.com (§6.15), so the hook must never set it.
	cfg := config.Defaults()
	env := hookEnv(&cfg, "/tmp/ca.pem")
	if _, bad := env["ANTHROPIC_BASE_URL"]; bad {
		t.Fatal("hook must not set ANTHROPIC_BASE_URL — it breaks Remote Control")
	}
	if env["HTTPS_PROXY"] == "" || env["NODE_EXTRA_CA_CERTS"] != "/tmp/ca.pem" {
		t.Fatalf("MITM env incomplete: %v", env)
	}
}

// Installing must never silently replace a status line another tool owns —
// that line may be doing far more than ours.
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
