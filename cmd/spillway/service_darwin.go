//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func servicePlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"), nil
}

func serviceLogPaths() (out, errPath string, err error) {
	home, herr := os.UserHomeDir()
	if herr != nil {
		return "", "", herr
	}
	dir := filepath.Join(home, "Library", "Logs")
	return filepath.Join(dir, "spillway.log"), filepath.Join(dir, "spillway.err.log"), nil
}

// plistXML renders the launchd agent. KeepAlive restarts the daemon if it
// dies; RunAtLoad starts it at login.
func plistXML(binPath, outLog, errLog string) string {
	esc := func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
		return r.Replace(s)
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + serviceLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + esc(binPath) + `</string>
    <string>server</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>` + esc(outLog) + `</string>
  <key>StandardErrorPath</key><string>` + esc(errLog) + `</string>
</dict>
</plist>
`
}

func launchctl(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// launchdTarget is the service's launchd address for this user's GUI session.
func launchdTarget() string {
	return "gui/" + strconv.Itoa(os.Getuid()) + "/" + serviceLabel
}

func serviceInstall() error {
	plist, err := servicePlistPath()
	if err != nil {
		return err
	}
	// selfPath, not os.Executable + EvalSymlinks: this path is written into
	// the plist once and read at every logon, and resolving a package
	// manager's stable symlink records a versioned path that the next
	// upgrade deletes.
	bin, err := selfPath()
	if err != nil {
		return err
	}
	outLog, errLog, err := serviceLogPaths()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outLog), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plist, []byte(plistXML(bin, outLog, errLog)), 0o644); err != nil {
		return err
	}
	// Replace any previous registration; bootout failure is fine when
	// nothing was loaded.
	_, _ = launchctl("bootout", launchdTarget())
	if out, err := launchctl("bootstrap", "gui/"+strconv.Itoa(os.Getuid()), plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	fmt.Printf("service installed: %s\n", plist)
	fmt.Printf("logs: %s\n", outLog)
	return nil
}

func serviceUninstall() error {
	plist, err := servicePlistPath()
	if err != nil {
		return err
	}
	if out, err := launchctl("bootout", launchdTarget()); err != nil && !strings.Contains(out, "No such process") {
		fmt.Fprintf(os.Stderr, "spillway: launchctl bootout: %v: %s\n", err, out)
	}
	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("service uninstalled")
	return nil
}

func serviceStatus() error {
	plist, err := servicePlistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(plist); os.IsNotExist(err) {
		fmt.Println("service NOT installed")
		return nil
	}
	out, err := launchctl("print", launchdTarget())
	if err != nil {
		fmt.Printf("service installed (%s) but not loaded\n", plist)
		return nil
	}
	state, pid := "unknown", ""
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "state = ") {
			state = strings.TrimPrefix(l, "state = ")
		}
		if strings.HasPrefix(l, "pid = ") {
			pid = strings.TrimPrefix(l, "pid = ")
		}
	}
	fmt.Printf("service installed (%s)\nstate: %s", plist, state)
	if pid != "" {
		fmt.Printf("  pid: %s", pid)
	}
	fmt.Println()
	return nil
}
