package accounts

import (
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
	"github.com/coderage-labs/spillway/internal/secrets"
)

// ResolveYAML builds a pool account from config metadata, reading token
// material from the secret store (yaml files hold metadata only, §5). A
// missing store entry is a clear error naming the account.
func ResolveYAML(a config.AccountConfig, store secrets.Store) (*pool.Account, error) {
	s, err := store.Get(a.Name)
	if err != nil {
		return nil, err
	}
	upstream := a.Upstream
	if upstream == "" {
		// The provider's own default; an empty one means "use the global
		// upstream", which is Claude's case.
		if d := provider.For(a.Type).DefaultUpstream; d != "" && a.Type != "" && a.Type != "claude-oauth" {
			upstream = d
		}
	}
	acct := pool.NewAccount(a.Name, pool.SourceYAML,
		s.AccessToken, s.RefreshToken, a.ExpiresAt, upstream)
	acct.Type = a.Type
	acct.Label = a.Label
	acct.Priority = a.Priority
	acct.AllowOverage = a.AllowOverage
	acct.AccountUUID = a.AccountUUID
	acct.ModelMap = a.ModelMap
	return acct, nil
}
