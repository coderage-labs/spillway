package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spillway.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const baseCfg = `proxy:
  port: 7654
  host: 127.0.0.1
upstream: https://api.anthropic.com
pool:
  exhaustedMode: notify
  holdMax: 4h
  switchThreshold: 0.98
log:
  level: info
accounts:
  - name: work
    type: claude-oauth
`

func str(s string) *string { return &s }
func boolp(b bool) *bool   { return &b }

func TestUpdateSettingsAppliesOnlySuppliedFields(t *testing.T) {
	p := writeCfg(t, baseCfg)
	cfg, err := UpdateSettings(p, Settings{ExhaustedMode: str("fail")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pool.ExhaustedMode != "fail" {
		t.Errorf("exhaustedMode = %q", cfg.Pool.ExhaustedMode)
	}
	// Everything not mentioned must survive: a partial update is not a reset.
	if cfg.Pool.HoldMax != "4h" || cfg.Pool.SwitchThreshold != 0.98 {
		t.Errorf("unmentioned fields changed: holdMax=%q threshold=%v",
			cfg.Pool.HoldMax, cfg.Pool.SwitchThreshold)
	}
	if cfg.Proxy.Port != 7654 || cfg.Upstream != "https://api.anthropic.com" {
		t.Errorf("unrelated config changed: %+v", cfg.Proxy)
	}
}

// Validation happens BEFORE the write: an invalid file on disk stops the
// daemon coming up, long after the person who caused it closed the tab.
func TestUpdateSettingsRejectsBadValuesWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Settings
		want string
	}{
		{"bad mode", Settings{ExhaustedMode: str("sideways")}, "exhaustedMode"},
		{"bad duration", Settings{HoldMax: str("4 hours")}, "holdMax"},
		{"bad interval", Settings{ProbeInterval: str("soon")}, "probeInterval"},
		{"threshold too high", Settings{SwitchThreshold: str("1.5")}, "switchThreshold"},
		{"threshold zero", Settings{SwitchThreshold: str("0")}, "switchThreshold"},
		{"unknown account", Settings{Accounts: map[string]AccountSettings{
			"ghost": {Label: str("x")}}}, "not found"},
	} {
		p := writeCfg(t, baseCfg)
		before, _ := os.ReadFile(p)

		_, err := UpdateSettings(p, tc.in)
		if err == nil {
			t.Errorf("%s: expected rejection", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q should mention %q", tc.name, err, tc.want)
		}
		after, _ := os.ReadFile(p)
		if string(before) != string(after) {
			t.Errorf("%s: config was modified despite the error", tc.name)
		}
	}
}

func TestUpdateSettingsPerAccountFields(t *testing.T) {
	p := writeCfg(t, baseCfg)
	cfg, err := UpdateSettings(p, Settings{Accounts: map[string]AccountSettings{
		"work": {Label: str("day job"), Disabled: boolp(true)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Accounts[0].Label != "day job" || !cfg.Accounts[0].Disabled {
		t.Errorf("account not updated: %+v", cfg.Accounts[0])
	}
}

// The file may be hand-edited at any moment, so the writer re-reads rather
// than trusting a caller's copy.
func TestUpdateSettingsPreservesConcurrentHandEdits(t *testing.T) {
	p := writeCfg(t, baseCfg)
	edited := strings.Replace(baseCfg, "level: info", "level: debug", 1)
	if err := os.WriteFile(p, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := UpdateSettings(p, Settings{ExhaustedMode: str("hold")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("hand edit clobbered: log.level = %q, want debug", cfg.Log.Level)
	}
}

func TestCurrentSettingsRoundTrips(t *testing.T) {
	p := writeCfg(t, baseCfg)
	cfg, err := LoadFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	s := CurrentSettings(cfg)
	if s.ExhaustedMode == nil || *s.ExhaustedMode != "notify" {
		t.Errorf("exhaustedMode not reported")
	}
	if _, ok := s.Accounts["work"]; !ok {
		t.Errorf("accounts not reported: %v", s.Accounts)
	}
	// Feeding it straight back must be a no-op, or the UI's own form would
	// change settings just by being saved.
	if _, err := UpdateSettings(p, s); err != nil {
		t.Fatalf("round-trip rejected: %v", err)
	}
	after, err := LoadFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	if after.Pool.ExhaustedMode != cfg.Pool.ExhaustedMode ||
		after.Pool.SwitchThreshold != cfg.Pool.SwitchThreshold ||
		after.Pool.HoldMax != cfg.Pool.HoldMax {
		t.Errorf("round-trip changed values: %+v -> %+v", cfg.Pool, after.Pool)
	}
}

// allowOverage must never be reachable through the settings API.
//
// That endpoint is driven by a browser page. Every other setting it exposes
// changes how long a request waits; this one decides whether the user is
// charged, so it is deliberately absent from the allowlist and set by
// `spillway accounts overage` instead. A field added to Settings by habit
// would quietly undo that.
func TestSettingsCannotEnableOverage(t *testing.T) {
	body := `{"accounts":{"a":{"allowOverage":true,"label":"x"}},"allowOverage":true}`
	var in Settings
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatal(err)
	}
	// Unknown fields are dropped by encoding/json, which is the mechanism —
	// assert the effect, so this fails if someone adds the field.
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "allowOverage") {
		t.Errorf("Settings round-trips allowOverage, making it writable from the "+
			"dashboard:\n%s", raw)
	}
}
