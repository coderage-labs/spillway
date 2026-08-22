package main

// `spillway service` runs the daemon without a terminal (design doc §9).
// The proxy core needs no TTY — only login and `run` are interactive — so the
// daemon is the natural default and `run` becomes a thin convenience.
//
// The per-platform halves live in service_darwin.go / service_windows.go /
// service_other.go. All three register the daemon as a PER-USER agent rather
// than a system service, and that is not a shortcut: account tokens live in
// the user's keychain (Keychain Services, Credential Manager, Secret
// Service), so a daemon running as root or SYSTEM could not read them.

import "fmt"

const serviceLabel = "dev.coderage.spillway"

func runService(args []string) error {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "install":
		return serviceInstall()
	case "uninstall":
		return serviceUninstall()
	case "status":
		return serviceStatus()
	default:
		return fmt.Errorf("unknown service action %q (install|uninstall|status)", action)
	}
}
