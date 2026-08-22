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

// A variable so the install sequence can be tested without a real Task
// Scheduler, which no CI runner offers and no test can drive.
var schtasks = func(args ...string) (string, error) {
	out, err := exec.Command("schtasks", args...).CombinedOutput()
	// schtasks writes UTF-16 on some locales; the ASCII we match on survives
	// either way, so only the framing is trimmed.
	return strings.TrimSpace(string(out)), err
}

func serviceInstall() error {
	// selfPath, not os.Executable + EvalSymlinks: the scheduled task stores
	// this path and runs it at every logon, so resolving a package manager's
	// stable symlink records something the next upgrade removes.
	bin, err := selfPath()
	if err != nil {
		return err
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

	// Stop whatever is running before replacing the registration.
	//
	// Without this, reinstalling over a running daemon did nothing at all to
	// the daemon: /Create /F rewrites the task but leaves the process alone,
	// and MultipleInstancesPolicy is IgnoreNew, so the /Run below is
	// discarded because an instance is already running. The command printed
	// "service installed" while the old binary kept serving — which is
	// exactly what an upgrade does, and exactly when it matters.
	//
	// Ignoring the error is correct: it fails when nothing is running and
	// when nothing is registered, which are the ordinary cases on a first
	// install.
	_, _ = schtasks("/End", "/TN", taskName)

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
	// Stop it before deregistering, or the process outlives the task that
	// owns it and nothing is left to stop it by.
	_, _ = schtasks("/End", "/TN", taskName)
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
