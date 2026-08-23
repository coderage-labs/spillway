package main

import "fmt"

// adminGetter is the one method restartNotice needs from *adminAPI — narrowed
// so a test can fake it without a real HTTP server, though the table tests in
// login_notice_test.go exercise the real thing via httptest, same as
// adminclient_test.go does.
type adminGetter interface {
	get(path string, v any) error
}

// restartNotice implements option 1 of issue #46: a running daemon loads its
// accounts at startup, so a fresh login does not change what it thinks about
// an account until it is restarted. If a daemon is reachable and still
// reports name as disabled right after a successful login, say so and name
// the fix; otherwise stay silent.
//
// Silence is the answer, not an error, in every other case:
//   - no daemon reachable — the ordinary case while setting up, or when
//     nothing is running yet. A warning there would be worse than nothing.
//   - the daemon answers but doesn't mention this account, or reports it as
//     something other than disabled (ok, exhausted, parked) — none of those
//     are the stale-credential problem this notice exists for.
//   - the daemon's response doesn't even decode — treated the same as
//     unreachable, not as grounds to guess.
//
// The string-only return is deliberate: there is nothing here for a caller
// to propagate as an error, so a call site cannot accidentally turn "daemon
// didn't answer" into a failed login.
func restartNotice(api adminGetter, name string) string {
	var accounts []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	if err := api.get("/api/accounts", &accounts); err != nil {
		return ""
	}
	for _, a := range accounts {
		if a.Name != name {
			continue
		}
		if a.State != "disabled" {
			return ""
		}
		return fmt.Sprintf(
			"note: the running daemon still reports %s as disabled — it loaded that credential at startup, so this login alone will not fix it.\n"+
				"  restart it to pick up the new one: `spillway service install` if it runs as a background service,\n"+
				"  or stop and re-run `spillway server` if you started it yourself in a terminal",
			name)
	}
	return ""
}

// warnIfDaemonStale is the call site login.go uses after a successful login.
// It is deliberately not the testable unit (restartNotice is, above) — this
// wrapper only dials the daemon and prints, and dialAdmin itself needs a
// config on disk to resolve, which is exactly the kind of thing login_test.go
// avoids setting up for every case. Any failure to even dial (bad config,
// nothing listening) is silence, same as restartNotice's own silence: the
// credential is already written by the time this runs, so nothing here may
// turn a successful login into a visible failure.
func warnIfDaemonStale(name string) {
	api, err := dialAdmin()
	if err != nil {
		return
	}
	if msg := restartNotice(api, name); msg != "" {
		fmt.Println(msg)
	}
}
