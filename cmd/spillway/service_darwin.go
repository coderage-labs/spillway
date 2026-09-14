//go:build darwin

package main

import (
	"fmt"
	"io"
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

// serviceLogPaths returns the two log files the agent involves.
//
// daemonLog is spillway's own, opened and rotated by the daemon itself
// (issue #171). launchdLog is where launchd redirects the process's streams:
// it catches what exists before the logger does and what the Go runtime
// prints on the way down — a panic, a bind failure — and nothing else.
//
// They MUST be different paths. The 236 MB of #171 was a file launchd
// redirected stderr to, which is precisely the file spillway cannot rotate:
// the descriptor belongs to launchd, so renaming the file leaves launchd
// appending to the renamed inode while the fresh one stays empty. Pointing
// both at one path would be worse than the bug — two writers, one of them
// moving the file under the other.
func serviceLogPaths() (daemonLog, launchdLog string, err error) {
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
	return filepath.Join(dir, base+".log"), filepath.Join(dir, base+".launchd.log"), nil
}

// legacyErrLog is the file older plists redirected stderr to, and the one
// that reached 236 MB. Nothing writes it after this version reinstalls the
// agent; install says so rather than deleting somebody's log for them.
func legacyErrLog() (string, error) {
	daemonLog, _, err := serviceLogPaths()
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(daemonLog, ".log") + ".err.log", nil
}

// plistXML renders the launchd agent. KeepAlive restarts the daemon if it
// dies; RunAtLoad starts it at login.
func plistXML(binPath, daemonLog, launchdLog string) string {
	esc := func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
		return r.Replace(s)
	}
	// The daemon is told to open its own log, and launchd's redirect goes
	// somewhere else: only one writer per file, and the busy one is the file
	// spillway can rotate. See serviceLogPaths.
	var progArgs string
	for _, a := range serverArgs(daemonLog) {
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
  <key>StandardOutPath</key><string>` + esc(launchdLog) + `</string>
  <key>StandardErrorPath</key><string>` + esc(launchdLog) + `</string>
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
	daemonLog, launchdLog, err := serviceLogPaths()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(daemonLog), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plist, []byte(plistXML(bin, daemonLog, launchdLog)), 0o644); err != nil {
		return err
	}
	// Replace any previous registration; bootout failure is fine when
	// nothing was loaded.
	//
	// This is also the whole of the upgrade from a pre-#171 agent, and the
	// order is what keeps it safe: bootout stops the old daemon and closes
	// the descriptors launchd held on the old paths, and only the daemon
	// bootstrap starts afterwards passes --log-file. There is never a
	// moment when two processes write one log file, and the old file is
	// left exactly as it is — not rotated, not truncated, not moved.
	_, _ = launchctl("bootout", launchdTarget())
	if out, err := bootstrapService(plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	fmt.Printf("service installed: %s\n", plist)
	fmt.Printf("logs: %s\n", daemonLog)
	fmt.Printf("startup and crash output: %s\n", launchdLog)
	reportLegacyErrLog(os.Stdout)
	return nil
}

// reportLegacyErrLog tells an upgrading user about the file that used to
// hold everything and now holds nothing.
//
// Said rather than done: that file is 236 MB of the user's history on the
// machine this issue came from, and an installer that silently deletes a
// log — even a stale one — is not a trade worth making for a tidy Logs
// folder. Nothing writes it once the new plist is loaded, so removing it is
// safe whenever they get round to it.
func reportLegacyErrLog(w io.Writer) {
	path, err := legacyErrLog()
	if err != nil {
		return
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		return
	}
	fmt.Fprintf(w, "note: %s (%d MB) is no longer written — spillway now rotates its own log.\n",
		path, fi.Size()>>20)
	fmt.Fprintf(w, "      delete it when you no longer want the history:  rm %s\n", path)
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
