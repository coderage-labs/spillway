package accounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
	"github.com/coderage-labs/spillway/internal/secrets"
)

// refreshWindow is how close to expiry EnsureFresh starts refreshing.
const refreshWindow = 5 * time.Minute

// Manager recovers account credentials (pool.TokenManager). Refresh calls
// are singleflight per account (§6.4): concurrent refreshes collapse to one
// HTTP call, and the result is persisted before waiters are released —
// providers can rotate refresh tokens on use, so parallel refreshes would
// invalidate each other.
type Manager struct {
	Refresher  *Refresher
	Keychain   Source        // reload source for keychain accounts (never written)
	Secrets    secrets.Store // refresh write-back for yaml accounts
	ConfigPath string        // spillway.yaml path for expiry updates
	Logger     *slog.Logger
	Now        func() time.Time // testability; defaults to time.Now
	// KimiAuthBase overrides the kimi token endpoint (tests; empty → default).
	KimiAuthBase string

	mu       sync.Mutex
	inflight map[*pool.Account]*refreshCall
}

type refreshCall struct {
	done chan struct{}
	err  error
}

// NewManager wires a Manager for the running daemon.
func NewManager(cfgPath string, keychain Source, store secrets.Store, logger *slog.Logger) *Manager {
	m := &Manager{
		Refresher:  NewRefresher(),
		Keychain:   keychain,
		Secrets:    store,
		ConfigPath: cfgPath,
		Logger:     logger,
		inflight:   map[*pool.Account]*refreshCall{},
	}
	return m
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) log() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// EnsureFresh refreshes the account when its token expires within
// refreshWindow; a no-op otherwise. Static-key accounts (no refresh token,
// no expiry) are never refresh-attempted.
func (m *Manager) EnsureFresh(ctx context.Context, a *pool.Account) error {
	_, _, exp := a.Credentials()
	if exp <= 0 || exp > m.now().Add(refreshWindow).UnixMilli() {
		return nil
	}
	return m.singleflight(ctx, a)
}

// Recover forces one refresh/reload after an upstream 401, regardless of
// expiry. For a static-key account no refresh exists — the 401 stands, and
// the account is disabled with a re-add message.
func (m *Manager) Recover(ctx context.Context, a *pool.Account) error {
	return m.singleflight(ctx, a)
}

func (m *Manager) singleflight(ctx context.Context, a *pool.Account) error {
	m.mu.Lock()
	if c, ok := m.inflight[a]; ok {
		m.mu.Unlock()
		select {
		case <-c.done:
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := &refreshCall{done: make(chan struct{})}
	m.inflight[a] = c
	m.mu.Unlock()

	c.err = m.refreshOne(ctx, a)

	m.mu.Lock()
	delete(m.inflight, a)
	m.mu.Unlock()
	close(c.done)
	return c.err
}

// refreshOne performs the actual recovery: keychain reload for keychain
// accounts (never a keychain write — the claude CLI owns that item), token
// refresh + persistence for inline accounts, at the provider's endpoint.
func (m *Manager) refreshOne(ctx context.Context, a *pool.Account) error {
	if a.Source == pool.SourceKeychain {
		return m.reloadKeychain(a)
	}

	_, refresh, _ := a.Credentials()
	if refresh == "" {
		if a.IsStaticKey() {
			// Static credential (console API key): reached only via Recover
			// after an upstream 401 — the key itself was rejected. Disable
			// with a re-add message, never "no refresh token".
			a.Disable()
			m.log().Error("static key rejected by upstream, account disabled — re-add the key (login/import)",
				"account", a.Name)
			return errors.New("account " + a.Name + ": static key rejected; re-add the key")
		}
		a.Disable()
		m.log().Error("account has no refresh token, disabling", "account", a.Name)
		return errors.New("account " + a.Name + ": no refresh token")
	}

	// The provider names the refresh shape; the Manager owns the endpoints
	// and client, which a package-level registry could not hold per-instance
	// (§6.20).
	var (
		access, newRefresh string
		expiresAt          int64
		err                error
	)
	switch provider.For(a.Type).RefreshFlavour {
	case provider.KimiDevice:
		var res *provider.KimiTokens
		if res, err = provider.KimiRefresh(ctx, nil, m.KimiAuthBase, refresh); err == nil {
			access, newRefresh, expiresAt = res.AccessToken, res.RefreshToken, res.ExpiresAtMs(m.now())
		}
	default: // provider.AnthropicOAuth
		var res *TokenResult
		if res, err = m.Refresher.Refresh(ctx, refresh); err == nil {
			access, expiresAt = res.AccessToken, res.ExpiresAt
			if newRefresh = res.RefreshToken; newRefresh == "" {
				newRefresh = refresh // provider omitted it: keep the old one
			}
		}
	}
	if errors.Is(err, ErrRefreshDead) {
		a.Disable()
		m.log().Error("refresh token rejected, account disabled — re-login required",
			"account", a.Name)
		return err
	}
	if err != nil {
		// Transient: keep the account; next Select tries again.
		m.log().Warn("token refresh failed (transient)", "account", a.Name, "err", err)
		return err
	}

	a.SetCredentials(access, newRefresh, expiresAt)
	// Persist before releasing waiters (§6.4): tokens to the secret store,
	// expiry metadata to the yaml.
	if m.Secrets != nil {
		if err := m.Secrets.Set(a.Name, secrets.Secrets{AccessToken: access, RefreshToken: newRefresh}); err != nil {
			m.log().Error("refreshed token not persisted to secret store", "account", a.Name, "err", err)
		}
	}
	if err := config.UpdateAccountExpiry(m.ConfigPath, a.Name, expiresAt); err != nil {
		// Token is live in memory; persistence failure is loud but not fatal.
		m.log().Error("refreshed token expiry not persisted", "account", a.Name, "err", err)
	}
	m.log().Info("token refreshed", "account", a.Name,
		"expires", time.UnixMilli(expiresAt).UTC().Format(time.RFC3339))
	return nil
}

// KeychainRemedy is the one line every keychain-reload failure and every
// startup warning should point at. Once #81's login fix has shipped,
// re-authenticating is the whole fix: UpsertAccount now clears Source on a
// successful login, so this single command both gives the account its own
// credential and drops it out of the broken borrowed-token path.
func KeychainRemedy(name string) string {
	return fmt.Sprintf("run `spillway login claude %s` to give it its own credential "+
		"(this also drops source: keychain from the config), then restart spillway", name)
}

// errKeychainAlreadyDisabled is returned by reloadKeychain without touching
// the keychain at all when the account is already disabled from a previous
// reload of this exact kind (#81). A borrowed credential that has already
// failed cannot pass just because we ask again a minute later — Anthropic
// rotates refresh tokens, so spillway does not, and never will, write back
// to this keychain item, meaning nothing changes it out from under us except
// a fresh `claude` login. Detecting "already gave up on this" and stopping
// avoids the exact failure #81 reported: the same dead entry reloaded once a
// minute for an hour, 60 attempts that could not possibly succeed, before
// the one useful line finally printed.
//
// This deliberately does NOT re-read the keychain to check whether the
// material changed and, if so, revive the account — that would be building
// "disabled becomes recoverable automatically", which #81 explicitly defers
// to a separate decision (see the design note in the PR description). A
// genuine re-login is still picked up normally by the ordinary path below,
// for any account that has not yet been disabled — e.g. right after a daemon
// restart, when pool.New reconstructs every keychain account as eligible
// regardless of the credential's last known expiry (keychain accounts are
// always "CanRefresh") and this function runs for real again.
var errKeychainAlreadyDisabled = errors.New("keychain credential already disabled")

func (m *Manager) reloadKeychain(a *pool.Account) error {
	if a.State() == pool.StateDisabled {
		return fmt.Errorf("account %q: %w — %s", a.Name, errKeychainAlreadyDisabled, KeychainRemedy(a.Name))
	}
	o, err := LoadClaude(m.Keychain, m.now())
	if err != nil {
		a.Disable()
		m.log().Error("keychain reload failed, account disabled — "+KeychainRemedy(a.Name),
			"account", a.Name, "err", err)
		return err
	}
	a.SetCredentials(o.AccessToken, o.RefreshToken, o.ExpiresAt)
	m.log().Info("keychain credentials reloaded", "account", a.Name,
		"expires", time.UnixMilli(o.ExpiresAt).UTC().Format(time.RFC3339))
	return nil
}
