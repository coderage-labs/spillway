package accounts

import (
	"context"
	"errors"
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

func (m *Manager) reloadKeychain(a *pool.Account) error {
	o, err := LoadClaude(m.Keychain, m.now())
	if err != nil {
		a.Disable()
		m.log().Error("keychain reload failed, account disabled — re-login via `claude`",
			"account", a.Name, "err", err)
		return err
	}
	a.SetCredentials(o.AccessToken, o.RefreshToken, o.ExpiresAt)
	m.log().Info("keychain credentials reloaded", "account", a.Name,
		"expires", time.UnixMilli(o.ExpiresAt).UTC().Format(time.RFC3339))
	return nil
}
