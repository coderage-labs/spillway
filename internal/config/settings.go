package config

// Editable settings — the subset the dashboard may change.
//
// Two rules shape this file. First, an allowlist rather than a general
// patch: the config also holds credentials-adjacent fields and listener
// addresses, and "the browser can write any key" is a much larger promise
// than "the browser can change the rotation threshold". Second, validate
// before persisting: a bad holdMax written to disk is only discovered at the
// next start, by which point the daemon will not come up.

import (
	"fmt"
	"os"
	"time"
)

// Settings is the editable surface. Pointer fields distinguish "not supplied"
// from "set to the zero value", so a partial update cannot silently reset a
// field the caller never mentioned.
type Settings struct {
	ExhaustedMode      *string `json:"exhaustedMode,omitempty"`
	HoldMax            *string `json:"holdMax,omitempty"`
	SwitchThreshold    *string `json:"switchThreshold,omitempty"`
	ProbeOnStart       *bool   `json:"probeOnStart,omitempty"`
	ProbeInterval      *string `json:"probeInterval,omitempty"`
	CrossProvider      *bool   `json:"crossProvider,omitempty"`
	StickyAcrossFamily *bool   `json:"stickyAcrossFamily,omitempty"`
	// HideOverageFromClient is editable here, unlike allowOverage, because
	// it cannot cause spend by itself: with allowOverage off (the default,
	// and deliberately NOT reachable from this surface) spillway refuses
	// rather than bills, stripped markers or not. What it does change is
	// who asks for consent — see the config field's comment (issue #103).
	HideOverageFromClient *bool `json:"hideOverageFromClient,omitempty"`
	// Accounts maps account name -> editable per-account fields.
	Accounts map[string]AccountSettings `json:"accounts,omitempty"`
}

// AccountSettings is the editable surface of one account. Credentials are
// deliberately absent: token material must not be reachable from a browser,
// loopback or not.
type AccountSettings struct {
	Label    *string `json:"label,omitempty"`
	Disabled *bool   `json:"disabled,omitempty"`
	Priority *int    `json:"priority,omitempty"`
}

// CurrentSettings reports the editable values, so a UI can render the form
// from the same definition the writer accepts.
func CurrentSettings(c *Config) Settings {
	mode, hold, probeIv := c.Pool.ExhaustedMode, c.Pool.HoldMax, c.Pool.ProbeInterval
	thr := fmt.Sprintf("%g", c.Pool.SwitchThreshold)
	cross := c.Pool.CrossProvider
	sticky := c.Pool.StickyAcrossFamily
	hideOverage := c.Pool.HideOverageFromClient
	probe := c.Pool.ProbeOnStart == nil || *c.Pool.ProbeOnStart

	s := Settings{
		ExhaustedMode: &mode, HoldMax: &hold, SwitchThreshold: &thr,
		ProbeOnStart: &probe, ProbeInterval: &probeIv, CrossProvider: &cross,
		StickyAcrossFamily:    &sticky,
		HideOverageFromClient: &hideOverage,
		Accounts:              map[string]AccountSettings{},
	}
	for _, a := range c.Accounts {
		label, disabled, prio := a.Label, a.Disabled, a.Priority
		s.Accounts[a.Name] = AccountSettings{Label: &label, Disabled: &disabled, Priority: &prio}
	}
	return s
}

// apply writes the supplied fields onto cfg, leaving everything else alone.
func (s Settings) apply(cfg *Config) error {
	if s.ExhaustedMode != nil {
		cfg.Pool.ExhaustedMode = *s.ExhaustedMode
	}
	if s.HoldMax != nil {
		cfg.Pool.HoldMax = *s.HoldMax
	}
	if s.ProbeInterval != nil {
		cfg.Pool.ProbeInterval = *s.ProbeInterval
	}
	if s.ProbeOnStart != nil {
		v := *s.ProbeOnStart
		cfg.Pool.ProbeOnStart = &v
	}
	if s.CrossProvider != nil {
		cfg.Pool.CrossProvider = *s.CrossProvider
	}
	if s.StickyAcrossFamily != nil {
		cfg.Pool.StickyAcrossFamily = *s.StickyAcrossFamily
	}
	if s.HideOverageFromClient != nil {
		cfg.Pool.HideOverageFromClient = *s.HideOverageFromClient
	}
	if s.SwitchThreshold != nil {
		var f float64
		if _, err := fmt.Sscanf(*s.SwitchThreshold, "%g", &f); err != nil {
			return fmt.Errorf("switchThreshold: %q is not a number", *s.SwitchThreshold)
		}
		cfg.Pool.SwitchThreshold = f
	}
	for name, upd := range s.Accounts {
		found := false
		for i := range cfg.Accounts {
			if cfg.Accounts[i].Name != name {
				continue
			}
			found = true
			if upd.Label != nil {
				cfg.Accounts[i].Label = *upd.Label
			}
			if upd.Disabled != nil {
				cfg.Accounts[i].Disabled = *upd.Disabled
			}
			if upd.Priority != nil {
				cfg.Accounts[i].Priority = *upd.Priority
			}
		}
		if !found {
			return fmt.Errorf("account %q not found", name)
		}
	}
	return nil
}

// validateEditable checks the fields this API accepts. Config.Validate covers
// the whole file; these are the messages a UI should show, phrased for the
// person typing rather than the process starting.
func validateEditable(cfg *Config) error {
	switch cfg.Pool.ExhaustedMode {
	case "fail", "hold", "notify":
	default:
		return fmt.Errorf("exhaustedMode: %q must be fail, hold or notify", cfg.Pool.ExhaustedMode)
	}
	if cfg.Pool.HoldMax != "" && cfg.Pool.HoldMax != "0" {
		if _, err := time.ParseDuration(cfg.Pool.HoldMax); err != nil {
			return fmt.Errorf("holdMax: %q is not a duration such as 4h or 30m", cfg.Pool.HoldMax)
		}
	}
	if cfg.Pool.ProbeInterval != "" && cfg.Pool.ProbeInterval != "0" {
		if _, err := time.ParseDuration(cfg.Pool.ProbeInterval); err != nil {
			return fmt.Errorf("probeInterval: %q is not a duration such as 30m", cfg.Pool.ProbeInterval)
		}
	}
	if t := cfg.Pool.SwitchThreshold; t <= 0 || t > 1 {
		return fmt.Errorf("switchThreshold: %g must be greater than 0 and at most 1", t)
	}
	return cfg.Validate()
}

// UpdateSettings applies s to the config at path, atomically, and only if the
// result validates. The file is re-read inside the call rather than taking a
// caller's copy: it may be hand-edited at any time, and clobbering that is
// exactly the failure this guards against.
func UpdateSettings(path string, s Settings) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := s.apply(cfg); err != nil {
		return nil, err
	}
	// Before persisting, never after: an invalid file that reaches disk stops
	// the daemon coming up, and the person who caused it has closed the tab.
	if err := validateEditable(cfg); err != nil {
		return nil, err
	}
	if err := writeFile(path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
