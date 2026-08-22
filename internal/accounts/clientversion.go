package accounts

// Supported client versions (design doc §6.8c).
//
// spillway depends on the shape of what the claude CLI sends and on which
// endpoints it calls — both of which have changed under us before (Remote
// Control's server-mode paths were discovered by watching live traffic, not
// from docs). The CLI also auto-updates, so a change can arrive without any
// action from the user.
//
// The decision in §6.8c was a documented matrix with an escape hatch rather
// than a hard failure: refusing to start because the CLI moved would be worse
// than the drift it guards against, since the proxy usually still works.

import (
	"fmt"
	"strconv"
	"strings"
)

// The tested range, compared at MAJOR.MINOR only. Patch releases ship weekly
// and do not move the request shape; warning on each would train people to
// ignore the warning, which is worse than not having it. Update deliberately,
// after checking the shape — not because a warning appeared.
//
// Exercised in practice: 2.1.239.
const (
	TestedMin = "2.0"
	TestedMax = "2.1"
)

// CheckClientVersion reports a human-readable warning when v falls outside
// the tested range, or "" when it is fine or unparseable. Never an error: an
// untested version is a caution, not a fault.
func CheckClientVersion(v string) string {
	got, ok := parseVersion(v)
	if !ok {
		return "" // unknown shape: say nothing rather than guess
	}
	got[2] = 0 // compare major.minor only
	lo, _ := parseVersion(TestedMin)
	hi, _ := parseVersion(TestedMax)

	switch {
	case less(got, lo):
		return fmt.Sprintf(
			"claude CLI %s is older than the tested range (%s-%s); "+
				"spillway may rely on endpoints it does not call",
			v, TestedMin, TestedMax)
	case less(hi, got):
		return fmt.Sprintf(
			"claude CLI %s is newer than the tested range (%s-%s); "+
				"if requests behave oddly, check whether the request shape moved",
			v, TestedMin, TestedMax)
	}
	return ""
}

type version [3]int

func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	// Trim any pre-release or build suffix.
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return version{}, false
	}
	var out version
	for i := 0; i < len(parts) && i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return version{}, false
		}
		out[i] = n
	}
	return out, true
}

func less(a, b version) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
