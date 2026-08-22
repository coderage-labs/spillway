//go:build integration && darwin

package main

import (
	"strconv"
	"strings"
	"testing"
)

// daemonPID is the pid launchd reports for the agent, or 0.
func daemonPID(t *testing.T) int {
	t.Helper()
	out, err := launchctl("print", launchdTarget())
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(l, "pid = "); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
}
