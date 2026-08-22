//go:build linux

package main

// systemd user units (§9). A *user* unit, not a system one, for the same
// reason launchd gets an agent and Windows a per-user task: the account
// tokens live in the user's Secret Service keyring, which a daemon running as
// root cannot read.
//
// Only systemd. An earlier version of this file supported nothing at all,
// on the grounds that a systemd unit would silently fail for anyone on runit,
// OpenRC or in a container — but that is an argument for detecting systemd
// and saying so when it is absent, not for leaving every Linux user to write
// their own unit. The detection is the point; the refusal below names what it
// found instead.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func systemdUnitName() string { return serviceLabel + ".service" }

func serviceUnitPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "systemd", "user", systemdUnitName()), nil
}

// serviceLogPaths is unused on Linux: a systemd service inherits journald, so
// there is no file to name. Kept because the cross-platform callers expect
// it, and returning the journal command would be a lie about a path.
func serviceLogPaths() (out, errPath string, err error) {
	return "", "", nil
}

// A variable so the tests can drive install/uninstall without a session bus.
var systemctl = func(args ...string) (string, error) {
	full := append([]string{"--user"}, args...)
	out, err := exec.Command("systemctl", full...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// requireSystemd reports why this machine cannot run a user unit, or "".
//
// /run/systemd/system is systemd's own documented marker for "systemd is the
// init system" — checking for the systemctl binary is not enough, since it is
// present on machines booted with something else.
func requireSystemd() string {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return "systemd is not the init system here (no /run/systemd/system)"
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return "systemctl is not on PATH"
	}
	return ""
}

func unsupported(why string) error {
	return fmt.Errorf("`service` needs systemd on Linux — %s.\n"+
		"  Run `spillway server` under your own init system instead", why)
}

// unitFile renders the user unit. Restart=on-failure rather than always: a
// clean exit is `spillway server` being asked to stop, and restarting it then
// would make the service impossible to stop without disabling it.
func unitFile(bin string) string {
	return `[Unit]
Description=spillway — pool your own AI subscription accounts behind one local proxy
Documentation=https://github.com/coderage-labs/spillway
After=default.target

[Service]
Type=simple
ExecStart=` + bin + ` ` + strings.Join(serverArgs(""), " ") + `
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`
}

func serviceInstall() error {
	if why := requireSystemd(); why != "" {
		return unsupported(why)
	}
	// selfPath, not os.Executable + EvalSymlinks: the unit stores this path
	// and runs it at every login, so resolving a package manager's stable
	// symlink records something the next upgrade removes.
	bin, err := selfPath()
	if err != nil {
		return err
	}
	unit, err := serviceUnitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unit, []byte(unitFile(bin)), 0o644); err != nil {
		return err
	}
	if out, err := systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v: %s", err, out)
	}
	// enable --now covers both cases: a fresh install starts it, and a
	// reinstall over a running service restarts it onto the new binary.
	if out, err := systemctl("enable", "--now", systemdUnitName()); err != nil {
		return fmt.Errorf("systemctl enable: %v: %s", err, out)
	}
	if out, err := systemctl("restart", systemdUnitName()); err != nil {
		return fmt.Errorf("systemctl restart: %v: %s", err, out)
	}
	fmt.Printf("service installed: %s\n", unit)
	fmt.Println("logs: journalctl --user -u spillway -f")
	// A user unit stops when the last session for the user ends, unless
	// lingering is on. For a daemon whose whole job is to be there when a
	// client starts, that is usually not what is wanted — but it needs
	// polkit, so it is a suggestion rather than something done silently.
	if !lingerEnabled() {
		fmt.Printf("to keep it running after you log out:\n  loginctl enable-linger %s\n", os.Getenv("USER"))
	}
	return nil
}

// lingerEnabled reports whether this user's services survive logout.
func lingerEnabled() bool {
	u := os.Getenv("USER")
	if u == "" {
		return false
	}
	out, err := exec.Command("loginctl", "show-user", u, "--property=Linger").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "Linger=yes"
}

func serviceUninstall() error {
	if why := requireSystemd(); why != "" {
		return unsupported(why)
	}
	unit, err := serviceUnitPath()
	if err != nil {
		return err
	}
	if out, err := systemctl("disable", "--now", systemdUnitName()); err != nil {
		// Not fatal: the unit may already be gone, and the file still needs
		// removing either way.
		fmt.Fprintf(os.Stderr, "spillway: systemctl disable: %v: %s\n", err, out)
	}
	if err := os.Remove(unit); err != nil && !os.IsNotExist(err) {
		return err
	}
	if out, err := systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v: %s", err, out)
	}
	fmt.Println("service uninstalled")
	return nil
}

func serviceStatus() error {
	if why := requireSystemd(); why != "" {
		return unsupported(why)
	}
	unit, err := serviceUnitPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(unit); os.IsNotExist(err) {
		fmt.Println("service NOT installed")
		return nil
	}
	fmt.Printf("service installed (%s)\n", unit)
	active, _ := systemctl("is-active", systemdUnitName())
	enabled, _ := systemctl("is-enabled", systemdUnitName())
	fmt.Printf("state: %s  at login: %s", active, enabled)
	if pid, err := systemctl("show", systemdUnitName(), "--property=MainPID", "--value"); err == nil &&
		pid != "" && pid != "0" {
		fmt.Printf("  pid: %s", pid)
	}
	fmt.Println()
	if !lingerEnabled() {
		fmt.Println("note: not lingering — this stops when you log out")
	}
	return nil
}
