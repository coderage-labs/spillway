//go:build integration && linux

package main

import (
	"strconv"
	"strings"
	"testing"
)

// daemonPID is MainPID from the user unit, or 0.
func daemonPID(t *testing.T) int {
	t.Helper()
	out, err := systemctl("show", systemdUnitName(), "--property=MainPID", "--value")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}
