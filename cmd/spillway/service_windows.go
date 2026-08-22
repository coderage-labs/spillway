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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func serviceLogPaths() (out string, err error) {
	base, err := os.UserCacheDir() // %LocalAppData%
	if err != nil {
		return "", err
	}
	name := "spillway"
	if serviceLabel != "dev.coderage.spillway" {
		name = serviceLabel
	}
	return filepath.Join(base, "spillway", name+".log"), nil
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

	doc, err := taskXMLFile(bin, logPath)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "spillway-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(doc); err != nil {
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
	_, _ = schtasks("/End", "/TN", taskName())
	waitUntilTaskStopped()

	// /F replaces an existing registration rather than failing.
	if out, err := schtasks("/Create", "/TN", taskName(), "/XML", f.Name(), "/F"); err != nil {
		return fmt.Errorf("schtasks /Create: %v: %s", err, out)
	}
	// A logon trigger will not fire until the next logon, and nobody expects
	// `service install` to leave the daemon stopped.
	if err := runUntilItStays(); err != nil {
		fmt.Fprintf(os.Stderr, "spillway: task registered but did not start: %v\n", err)
	}
	fmt.Printf("service installed: Task Scheduler task %s\n", taskName())
	fmt.Printf("logs: %s\n", logPath)
	return nil
}

func serviceUninstall() error {
	// Stop it before deregistering, or the process outlives the task that
	// owns it and nothing is left to stop it by.
	_, _ = schtasks("/End", "/TN", taskName())
	if out, err := schtasks("/Delete", "/TN", taskName(), "/F"); err != nil {
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
	out, err := schtasks("/Query", "/TN", taskName(), "/FO", "LIST")
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
	fmt.Printf("service installed (%s)\nstate: %s\nlogs: %s\n", taskName(), status, logPath)
	return nil
}

// runUntilItStays starts the task and checks it is still running a moment
// later, retrying if not.
//
// Waiting for the previous task to stop is not enough on its own. The
// scheduler reports Ready as soon as it has finished with the task, and the
// old process can still hold the port for a moment after that — so the
// replacement starts, fails to bind, and exits. The scheduler then records a
// completed run rather than a failure, RestartOnFailure never fires, and the
// machine is left with no proxy until the next logon. Intermittent, which is
// worse: it passed one probe run and failed the next on identical code.
//
// Checking that it is still running after a second is the part that matters.
// A /Run that "succeeds" and dies immediately is the failure being retried.
func runUntilItStays() error {
	var last string
	for attempt := 0; attempt < 8; attempt++ {
		if out, err := schtasks("/Run", "/TN", taskName()); err != nil {
			last = fmt.Sprintf("%v: %s", err, out)
			time.Sleep(time.Second)
			continue
		}
		time.Sleep(1500 * time.Millisecond)
		out, err := schtasks("/Query", "/TN", taskName(), "/FO", "LIST")
		if err == nil && strings.Contains(out, "Running") {
			return nil
		}
		last = "started and exited immediately"
		time.Sleep(time.Second)
	}
	return errors.New(last)
}

// waitUntilTaskStopped blocks until the scheduler stops reporting the task as
// running, or gives up.
//
// /End returns as soon as the scheduler has asked; the process is still there
// for a moment afterwards, still holding port 7654. Starting the replacement
// into that window is a reinstall — which is every upgrade — and the new
// daemon failed to bind and exited. RestartOnFailure did not bring it back:
// the task simply read "Ready" and the machine had no proxy at all. Measured
// on a Windows runner at over ninety seconds before the probe gave up.
func waitUntilTaskStopped() {
	for attempt := 0; attempt < 40; attempt++ {
		out, err := schtasks("/Query", "/TN", taskName(), "/FO", "LIST")
		if err != nil {
			return // not registered; nothing to wait for
		}
		if !strings.Contains(out, "Running") {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}
