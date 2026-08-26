package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/coderage-labs/spillway/internal/config"
)

// TestWarnDeprecatedKeychainAccountsFiresForKeychainSourced is #81's startup
// warning: an account still configured source: keychain must be named at
// every startup, with the remedy, since re-login alone was not enough to
// clear it before this PR's mergeLoginUpdate fix (and even after, the user
// still has to actually run the login).
func TestWarnDeprecatedKeychainAccountsFiresForKeychainSourced(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := config.Defaults()
	cfg.Accounts = []config.AccountConfig{
		{Name: "ckitch", Type: "claude-oauth", Source: "keychain"},
	}
	warnDeprecatedKeychainAccounts(&cfg, logger)

	out := buf.String()
	if !strings.Contains(out, "ckitch") {
		t.Errorf("warning missing account name, got: %s", out)
	}
	if !strings.Contains(out, "spillway login claude ckitch") {
		t.Errorf("warning missing the remedy command, got: %s", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a WARN-level log line, got: %s", out)
	}
}

// TestWarnDeprecatedKeychainAccountsSilentForOwnedAccounts is the negative
// case: an account spillway already owns (no source, or source: yaml) must
// not be warned about — it is not the broken mode #81 is about.
func TestWarnDeprecatedKeychainAccountsSilentForOwnedAccounts(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := config.Defaults()
	cfg.Accounts = []config.AccountConfig{
		{Name: "owned", Type: "claude-oauth"},
		{Name: "kimi", Type: "kimi-oauth", Upstream: "https://api.kimi.com/coding"},
	}
	warnDeprecatedKeychainAccounts(&cfg, logger)

	if out := buf.String(); out != "" {
		t.Errorf("expected no warning for spillway-owned accounts, got: %s", out)
	}
}
