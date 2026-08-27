package main

import (
	"errors"
	"fmt"
)

// Issue #83: `accounts remove`, `accounts priority` and `accounts overage`
// all used to edit the config and stop there — nothing told a running
// daemon, so it kept serving from stale state (in remove's case, a deleted
// credential) until it was restarted. These two functions tell it, reusing
// the admin endpoints the dashboard already applies its own writes through
// (pool.Apply, via the settings handler's onSettings callback) rather than
// inventing a second path into the pool.
//
// Both follow login_notice.go's restartNotice shape on purpose: a bare
// string to print, never an error. The CLI has already made the change
// durable on disk by the time either of these runs — that succeeded, full
// stop — so nothing here may turn a successful mutation into a failed
// command. A daemon that cannot be reached is not a problem to report as
// one: the config is the source of truth and a daemon starting later reads
// it correctly on its own, so that case says so plainly (not applied yet,
// nothing broken) rather than staying silent the way restartNotice does for
// login. Login's silence fits because nothing bad has happened yet for a
// brand new account; here the entire point of the change is "no restart
// needed", so pretending nothing needs saying when that promise didn't hold
// would be the "silently imply it applied live" issue #83 explicitly rules
// out.

// liveApplyAccountEdit tells a reachable daemon to re-read the config file
// and re-apply it to the running pool — what POST /api/settings with no
// body means. Used after `accounts priority`/`accounts overage`, which
// write straight to the yaml (overage deliberately bypasses the dashboard's
// PUT-shaped settings path — see config.SetAccountOverage's doc — so there
// is no new payload to PUT, only an instruction to pick up what the file
// now says).
func liveApplyAccountEdit() string {
	api, err := dialAdmin()
	if err != nil {
		return "not applied live: no daemon is currently running — this takes effect the next time `spillway server` starts"
	}
	if err := api.postJSON("/api/settings", nil, nil); err != nil {
		if errors.Is(err, ErrAdminUnreachable) {
			// The ordinary case: dialAdmin only resolves the config, it
			// never dials, so "nothing is listening" surfaces here, not
			// above. Same message as the dialAdmin-error branch — from the
			// caller's point of view it is the same situation.
			return "not applied live: no daemon is currently running — this takes effect the next time `spillway server` starts"
		}
		return fmt.Sprintf("saved, but the running daemon refused to apply it live (%v) — restart to be sure it takes effect", err)
	}
	return "applied to the running daemon immediately — no restart needed"
}

// liveRemoveAccount tells a reachable daemon to drop name from its pool
// immediately, so a credential the CLI has just deleted from the secret
// store can never be selected for a request (issue #83's original report).
// Not found in the running pool is not treated as a failure by the server
// (see admin.handleAccountRemove) — the CLI's goal already holds either way
// — so the only failure path here is the daemon being unreachable at all,
// or rejecting the request outright.
func liveRemoveAccount(name string) string {
	api, err := dialAdmin()
	if err != nil {
		return "not removed from a running daemon: none is currently running — a future start will not include it"
	}
	if err := api.postJSON("/api/accounts/remove", map[string]string{"name": name}, nil); err != nil {
		if errors.Is(err, ErrAdminUnreachable) {
			return "not removed from a running daemon: none is currently running — a future start will not include it"
		}
		return fmt.Sprintf("removed from the config, but the running daemon refused to remove it (%v) — "+
			"it may keep serving from the now-deleted credential until you restart", err)
	}
	return "removed from the running daemon immediately — it can no longer be selected for a request"
}

// accountAddPayload mirrors internal/admin's accountAddRequest JSON shape —
// not shared directly since admin's is package-private, same as
// accountRemoveResponse below is re-declared rather than imported.
// login.go fills this from the tokens it already has in memory right after
// a successful OAuth/device-flow exchange, so liveAddAccount never needs to
// re-read the keychain or yaml itself.
type accountAddPayload struct {
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

// accountAddResult mirrors admin's accountAddResponse.
type accountAddResult struct {
	Added           bool   `json:"added"`
	RestartRequired bool   `json:"restartRequired"`
	Reason          string `json:"reason"`
}

// liveAddAccount tells a reachable daemon to add req to its pool
// immediately (issue #87), or — for a name it already has — to hot-swap
// that account's credentials in place (the re-auth fold-in of issue #46's
// stale-credential gap: a running daemon used to keep serving the OLD
// credential in memory until restarted even after a successful
// re-authentication). A down daemon is a clean success, not an error, same
// as liveApplyAccountEdit/liveRemoveAccount: the config and secret store
// are already durably written by the time login.go calls this, and a
// future `spillway server` start reads them correctly on its own.
func liveAddAccount(req accountAddPayload) string {
	api, err := dialAdmin()
	if err != nil {
		return "not applied live: no daemon is currently running — this takes effect the next time `spillway server` starts"
	}
	var resp accountAddResult
	if err := api.postJSON("/api/accounts/add", req, &resp); err != nil {
		if errors.Is(err, ErrAdminUnreachable) {
			return "not applied live: no daemon is currently running — this takes effect the next time `spillway server` starts"
		}
		return fmt.Sprintf("saved, but the running daemon refused to apply it live (%v) — restart to be sure it takes effect", err)
	}
	switch {
	case resp.RestartRequired:
		return fmt.Sprintf("applied to the running daemon for ordinary requests, but %s", resp.Reason)
	case resp.Added:
		return "added to the running daemon immediately — selectable for the very next request"
	default:
		return "credentials updated on the running daemon immediately — no restart needed"
	}
}
