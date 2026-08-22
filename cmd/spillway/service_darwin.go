//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	// Named after the service, not the product: the integration tests
	// install under a label of their own, and without this their daemon
	// writes into the log of the one actually running on the machine.
	base := "spillway"
	if serviceLabel != "dev.coderage.spillway" {
		base = serviceLabel
	}
	return filepath.Join(dir, base+".log"), filepath.Join(dir, base+".err.log"), nil
}

// plistXML renders the launchd agent. KeepAlive restarts the daemon if it
// dies; RunAtLoad starts it at login.
func plistXML(binPath, outLog, errLog string) string {
	esc := func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
		return r.Replace(s)
	}
	// launchd redirects the streams itself, so the daemon needs no log flag.
	var progArgs string
	for _, a := range serverArgs("") {
		progArgs += "\n    <string>" + esc(a) + "</string>"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + serviceLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + esc(binPath) + `</string>` + progArgs + `
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

// A variable so the retry logic above it can be tested without a real
// launchd, which no CI runner and no test has.
var launchctl = func(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// bootstrapRetryDelay is short enough to be invisible when the first attempt
// works and long enough to outlast launchd settling; tests set it to zero.
var bootstrapRetryDelay = 200 * time.Millisecond

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
	if out, err := bootstrapService(plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	fmt.Printf("service installed: %s\n", plist)
	fmt.Printf("logs: %s\n", outLog)
	return nil
}

// bootstrapService loads the plist, retrying briefly. launchd needs a moment
// after the previous job goes away, and bootstrapping inside that window
// fails with EIO — surfaced as the unhelpful "try re-running as root".
//
// The first attempt at this checked whether the old job was still registered
// and gave up when it was not, on the theory that a leftover job was the only
// thing a retry could fix. That is exactly backwards: a `brew upgrade` had
// already killed the daemon by deleting the binary out from under it, so the
// job was long gone, `launchctl print` said so, and the guard turned a
// transient EIO into a hard failure that left the machine with no daemon.
// Retry whatever the reason; a bounded wait costs nothing when it works
// first time, which is the usual case.
func bootstrapService(plist string) (string, error) {
	domain := "gui/" + strconv.Itoa(os.Getuid())
	var out string
	var err error
	for attempt := 0; attempt < 15; attempt++ {
		if out, err = launchctl("bootstrap", domain, plist); err == nil {
			return out, nil
		}
		time.Sleep(bootstrapRetryDelay)
	}
	return out, err
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
