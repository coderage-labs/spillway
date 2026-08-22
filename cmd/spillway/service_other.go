//go:build !darwin && !windows && !linux

package main

import (
	"fmt"
	"runtime"
)

// The BSDs and anything else are left to their own init systems. Linux moved
// to service_linux.go: systemd has a per-user mechanism, and detecting it is
// better than supporting nothing because it is not universal.
func serviceUnsupported() error {
	return fmt.Errorf("`service` supports macOS (launchd), Windows (Task Scheduler) and Linux (systemd); "+
		"this is %s — run `spillway server` under your init system, e.g. a systemd user unit",
		runtime.GOOS)
}

func serviceInstall() error   { return serviceUnsupported() }
func serviceUninstall() error { return serviceUnsupported() }
func serviceStatus() error    { return serviceUnsupported() }
