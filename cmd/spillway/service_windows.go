package main

// Windows: a per-user Scheduled Task, not a Windows Service.
//
// That is a correctness choice, not an ease-of-implementation one. Account
// tokens live in Credential Manager, which is per-user and unreadable from
// SYSTEM — a service would install cleanly, start cleanly, and then fail to
// find a single credential. A logon-triggered task also matches what launchd
// does on macOS (a GUI agent, not a daemon) and needs no administrator.
//
// The task runs the binary through PowerShell rather than directly, for two
// reasons the scheduler cannot supply itself: -WindowStyle Hidden keeps a
// console window from appearing at every logon, and redirection gives the
// daemon a log file, which schtasks has no way to configure.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func serviceLogPaths() (out string, err error) {
	base, err := os.UserCacheDir() // %LocalAppData%
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "spillway", "spillway.log"), nil
}

func schtasks(args ...string) (string, error) {
	out, err := exec.Command("schtasks", args...).CombinedOutput()
	// schtasks writes UTF-16 on some locales; the ASCII we match on survives
	// either way, so only the framing is trimmed.
	return strings.TrimSpace(string(out)), err
}

func serviceInstall() error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}
	logPath, err := serviceLogPaths()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}

	doc, err := taskXML(bin, logPath)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "spillway-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(doc); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// /F replaces an existing registration rather than failing.
	if out, err := schtasks("/Create", "/TN", taskName, "/XML", f.Name(), "/F"); err != nil {
		return fmt.Errorf("schtasks /Create: %v: %s", err, out)
	}
	// A logon trigger will not fire until the next logon, and nobody expects
	// `service install` to leave the daemon stopped.
	if out, err := schtasks("/Run", "/TN", taskName); err != nil {
		fmt.Fprintf(os.Stderr, "spillway: task registered but did not start: %v: %s\n", err, out)
	}
	fmt.Printf("service installed: Task Scheduler task %s\n", taskName)
	fmt.Printf("logs: %s\n", logPath)
	return nil
}

func serviceUninstall() error {
	if out, err := schtasks("/Delete", "/TN", taskName, "/F"); err != nil {
		if strings.Contains(out, "cannot find") || strings.Contains(out, "does not exist") {
			fmt.Println("service NOT installed")
			return nil
		}
		return fmt.Errorf("schtasks /Delete: %v: %s", err, out)
	}
	fmt.Println("service uninstalled")
	return nil
}

func serviceStatus() error {
	out, err := schtasks("/Query", "/TN", taskName, "/FO", "LIST")
	if err != nil {
		fmt.Println("service NOT installed")
		return nil
	}
	status := "unknown"
	for _, line := range strings.Split(out, "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "Status:") {
			status = strings.TrimSpace(strings.TrimPrefix(l, "Status:"))
		}
	}
	logPath, _ := serviceLogPaths()
	fmt.Printf("service installed (%s)\nstate: %s\nlogs: %s\n", taskName, status, logPath)
	return nil
}
