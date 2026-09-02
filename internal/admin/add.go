package admin

// POST /api/accounts/add — issue #87: put a freshly authenticated account
// into the running pool immediately, the mirror of #83/#85's
// /api/accounts/remove. `spillway login` posts here right after writing the
// config and the secret store, carrying the plaintext tokens it already has
// in memory from the OAuth/device-flow exchange — that avoids the daemon
// needing to re-read the keychain or yaml from an HTTP handler just to learn
// what the CLI already knows.
//
// A name already present in the pool is treated as a re-auth (issue #46's
// other half, folded in here): its credentials are hot-swapped in place
// rather than the account being replaced, which also revives it from
// StateDisabled (Account.SetCredentials does that) — the daemon no longer
// keeps serving a stale, disabled credential in memory after a
// re-authentication until it's restarted.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
)

// accountAddRequest carries what the CLI already holds in memory right
// after a successful login. Upstream is only set by the caller for an
// account whose provider owns its own upstream (Kimi) or a custom override
// — a plain claude-oauth account leaves it empty, exactly like
// accounts.ResolveYAML resolves it from config.
type accountAddRequest struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	Upstream     string            `json:"upstream,omitempty"`
	AccessToken  string            `json:"accessToken"`
	RefreshToken string            `json:"refreshToken"`
	ExpiresAt    int64             `json:"expiresAt"`
	AccountUUID  string            `json:"accountUuid,omitempty"`
	ModelMap     map[string]string `json:"modelMap,omitempty"`
	Label        string            `json:"label,omitempty"`
	Priority     int               `json:"priority,omitempty"`
	AllowOverage *bool             `json:"allowOverage,omitempty"`
}

// accountAddResponse reports what actually happened, so the CLI can print
// something accurate rather than assuming success meant "brand new and
// fully live".
type accountAddResponse struct {
	// Added is true for a brand-new pool entry; false means name already
	// existed and this hot-swapped its credentials in place instead (the
	// re-auth fold-in). Both are success — Added only says which happened.
	Added bool `json:"added"`
	// RestartRequired is true when the account's upstream is a custom host
	// with no pre-minted MITM leaf: it IS selectable immediately for
	// base-URL-mode requests (the pool doesn't need MITM at all for that),
	// but CONNECT-mode (system-proxy) traffic to that host keeps
	// blind-tunnelling, unpooled, until a restart mints its leaf (#87's
	// "still restart-only" case).
	RestartRequired bool   `json:"restartRequired"`
	Reason          string `json:"reason,omitempty"`
}

// handleAccountAdd never reports "the account was already there" as an
// error — same idempotent shape as handleAccountRemove — because the re-auth
// fold-in makes that a normal, successful call, not a conflict.
func (s *Server) handleAccountAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req accountAddRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "malformed body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	// Re-auth fold-in: hot-swap credentials on the SAME *Account in place
	// rather than replacing it — that preserves its in-flight state
	// (sticky sessions already pointing at this pointer, its in-flight
	// counter) and reuses SetCredentials' own "revive from disabled"
	// behaviour (issue #46's stale-credential problem).
	for _, a := range s.pool.Accounts() {
		if a.Name == req.Name {
			wasDisabled := a.State() == pool.StateDisabled
			a.SetCredentials(req.AccessToken, req.RefreshToken, req.ExpiresAt)
			if wasDisabled && a.State() != pool.StateDisabled {
				// Issue #105: a live re-auth reviving a disabled credential
				// is a transition into potentially-usable capacity — wake
				// any request currently parked rather than leaving it to
				// find out only when its timer eventually fires.
				s.pool.SignalCapacity()
			}
			s.writeJSON(w, accountAddResponse{Added: false})
			return
		}
	}

	upstream := req.Upstream
	if upstream == "" {
		// Same rule as accounts.ResolveYAML: only a provider that owns its
		// own upstream (Kimi) gets defaulted here — Claude's DefaultUpstream
		// merely restates the global `upstream` setting, so defaulting it
		// would wrongly pin the account away from a configured override.
		if spec := provider.For(req.Type); spec.OwnsUpstream {
			upstream = spec.DefaultUpstream
		}
	}
	restart, reason := s.checkUpstreamLive(upstream)

	acct := pool.NewAccount(req.Name, pool.SourceYAML, req.AccessToken, req.RefreshToken, req.ExpiresAt, upstream)
	acct.Type = req.Type
	acct.SetLabel(req.Label)
	acct.SetPriority(req.Priority)
	acct.SetAllowOverage(req.AllowOverage)
	acct.AccountUUID = req.AccountUUID
	acct.ModelMap = req.ModelMap

	added := s.pool.Add(acct)
	if added && s.refreshHosts != nil {
		s.refreshHosts()
	}
	s.writeJSON(w, accountAddResponse{Added: added, RestartRequired: restart, Reason: reason})
}

// checkUpstreamLive reports whether upstream needs a restart before
// CONNECT-mode termination covers it, for this Server's MITM wiring.
func (s *Server) checkUpstreamLive(upstream string) (restartRequired bool, reason string) {
	return UpstreamRestartRequired(upstream, s.hostCovered)
}

// UpstreamRestartRequired is the one implementation of "can this account's
// upstream be served live, or does it need a restart first?" — exported so
// issue #84's config watcher asks the same question the same way rather
// than growing a second copy of the rule that would drift from this one.
//
// After #88 the MITM chain is minted once, up front, for every host in
// provider.DefaultUpstreamHosts() plus the configured set, and CA.Leaf is a
// pure lookup that errors on anything else. So a host that already has a
// pre-minted leaf is entirely live-safe; only a genuinely new host is
// restart-only, because covering it means regenerating the chain, which
// strands every already-running proxied CLI on the old anchor (#70).
//
// An empty upstream means "use the global default", which was covered
// before anything called this, so there is nothing to check. covered being
// nil (MITM not wired up — most tests, and a daemon whose CA failed to
// load) means "cannot promise CONNECT-mode coverage": conservatively
// flagged rather than silently assumed fine.
//
// The two callers act on the answer differently, deliberately.
// handleAccountAdd still adds the account — a person ran `spillway login`
// and is reading the reply, and the account genuinely does work in
// base-URL mode — and returns this reason with restartRequired set. The
// watcher refuses the add outright, because nobody is at a terminal to
// read anything: a half-live account whose CONNECT traffic blind-tunnels
// unpooled is exactly the silent wrong-behaviour an unattended reload must
// not create.
func UpstreamRestartRequired(upstream string, covered func(host string) bool) (restartRequired bool, reason string) {
	if upstream == "" {
		return false, ""
	}
	u, err := url.Parse(upstream)
	if err != nil || u.Hostname() == "" {
		return false, ""
	}
	host := u.Hostname()
	if covered != nil && covered(host) {
		return false, ""
	}
	return true, fmt.Sprintf(
		"%q is a custom upstream host, not any provider's default — it is selectable now for base-URL-mode "+
			"requests, but CONNECT/system-proxy mode will keep blind-tunnelling it, unpooled, until spillway is "+
			"restarted to mint its MITM leaf", host)
}
