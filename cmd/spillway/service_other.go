//go:build !darwin && !windows

package main

import (
	"fmt"
	"runtime"
)

// Linux and the BSDs are left to their own init systems on purpose. Writing a
// systemd unit would cover most of Linux and quietly fail for anyone on
// runit, OpenRC or a container — and unlike launchd and Task Scheduler there
// is no per-user mechanism that is present everywhere.
func serviceUnsupported() error {
	return fmt.Errorf("`service` supports macOS (launchd) and Windows (Task Scheduler); "+
		"this is %s — run `spillway server` under your init system, e.g. a systemd user unit",
		runtime.GOOS)
}

func serviceInstall() error   { return serviceUnsupported() }
func serviceUninstall() error { return serviceUnsupported() }
func serviceStatus() error    { return serviceUnsupported() }
