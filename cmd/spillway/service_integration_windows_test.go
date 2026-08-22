//go:build integration && windows

package main

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// daemonPID finds the running spillway.exe, or 0.
//
// By image name rather than through the scheduler: schtasks reports a status
// but not a pid, and the test binary is spillway.test.exe, so the only
// spillway.exe on the machine is the daemon under test.
func daemonPID(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq spillway.exe", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(line, "\",\"")
		if len(fields) < 2 {
			continue
		}
		if n, err := strconv.Atoi(strings.Trim(fields[1], `"`)); err == nil {
			return n
		}
	}
	return 0
}
