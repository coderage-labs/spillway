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

import (
	"fmt"
	"os"
)

// serviceLabel names the launchd agent, the systemd unit and the Scheduled
// Task. A variable rather than a constant so the integration tests can
// register under a name of their own: they install and uninstall a real
// service, and running them must not touch the one on the machine.
var serviceLabel = "dev.coderage.spillway"

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

// serverArgs is what the scheduler should run the daemon with.
//
// --config is passed only when this process has a non-default one: pinning it
// unconditionally would mean a service that ignores a config the user moves
// later, and omitting it always means a service that ignores the config it
// was installed with.
func serverArgs(logPath string) []string {
	args := []string{"server"}
	if logPath != "" {
		args = append(args, "--log-file", logPath)
	}
	if cfg := os.Getenv("SPILLWAY_CONFIG"); cfg != "" {
		args = append(args, "--config", cfg)
	}
	return args
}
