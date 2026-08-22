// Package accounts loads AI-vendor credentials from the local machine.
package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ClaudeOAuth is the parsed `claudeAiOauth` object from Claude Code's
// credential store.
type ClaudeOAuth struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"` // epoch milliseconds
	Scopes           []string `json:"scopes"`
	SubscriptionType string   `json:"subscriptionType"`
}

// Source yields the raw credential JSON (keychain item or credentials file).
type Source interface {
	Load() ([]byte, error)
}

// SourceFunc adapts a function to Source.
type SourceFunc func() ([]byte, error)

// Load implements Source.
func (f SourceFunc) Load() ([]byte, error) { return f() }

type claudeCredsFile struct {
	ClaudeAiOauth ClaudeOAuth `json:"claudeAiOauth"`
}

// LoadClaude reads Claude Code OAuth credentials from src, refusing empty
// tokens and tokens already expired at `now`. Token refresh is a later task —
// expiry fails visibly here.
func LoadClaude(src Source, now time.Time) (*ClaudeOAuth, error) {
	raw, err := src.Load()
	if err != nil {
		return nil, fmt.Errorf("claude credentials unavailable: %w (run `claude` once to log in)", err)
	}
	var f claudeCredsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("claude credentials malformed: %w", err)
	}
	o := &f.ClaudeAiOauth
	if o.AccessToken == "" {
		return nil, errors.New("claude credentials: accessToken is empty (run `claude` once to log in)")
	}
	if o.ExpiresAt <= now.UnixMilli() {
		return nil, fmt.Errorf("claude credentials: accessToken expired at %s (run `claude` to re-login; refresh not yet supported)",
			time.UnixMilli(o.ExpiresAt).UTC().Format(time.RFC3339))
	}
	return o, nil
}
