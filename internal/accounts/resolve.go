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
		// Ask the registry rather than naming a provider. Claude's default
		// upstream restates the global setting, so applying it here would
		// pin the account to api.anthropic.com and quietly ignore a
		// configured `upstream`.
		if spec := provider.For(a.Type); spec.OwnsUpstream {
			upstream = spec.DefaultUpstream
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
