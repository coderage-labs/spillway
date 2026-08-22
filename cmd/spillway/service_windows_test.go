package main

import (
	"errors"
	"strings"
	"testing"
)

func fakeSchtasks(t *testing.T, fn func(args ...string) (string, error)) *[]string {
	t.Helper()
	var calls []string
	old := schtasks
	schtasks = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if fn == nil {
			return "", nil
		}
		return fn(args...)
	}
	t.Cleanup(func() { schtasks = old })
	return &calls
}

// Reinstalling over a running daemon has to actually swap the binary.
//
// It did not. /Create /F rewrites the registration without touching the
// process, and MultipleInstancesPolicy is IgnoreNew, so the /Run that follows
// is discarded while the old instance is alive. The command reported success
// and the previous binary kept serving every request — the exact case an
// upgrade produces.
func TestInstallStopsTheRunningTaskBeforeReplacingIt(t *testing.T) {
	calls := fakeSchtasks(t, nil)
	if err := serviceInstall(); err != nil {
		t.Fatal(err)
	}

	end, create, run := idxOfArg(*calls, "/End"), idxOfArg(*calls, "/Create"), idxOfArg(*calls, "/Run")
	if end == len(*calls) {
		t.Fatalf("never stopped the running task: %v", *calls)
	}
	if end > create {
		t.Errorf("/End must precede /Create: %v", *calls)
	}
	if run < create {
		t.Errorf("/Run must follow /Create: %v", *calls)
	}
}

// After /End the scheduler still reports the task running for a moment, and
// the process still holds the port. Starting the replacement into that window
// is what an upgrade does, and the new daemon exits unable to bind — leaving
// the machine with no proxy, because the task reads "Ready" rather than
// failed and RestartOnFailure never fires.
func TestInstallWaitsForTheOldTaskToStop(t *testing.T) {
	running := 3
	calls := fakeSchtasks(t, func(args ...string) (string, error) {
		if args[0] == "/Query" {
			if running > 0 {
				running--
				return "TaskName: \\dev.coderage.spillway\nStatus: Running", nil
			}
			return "TaskName: \\dev.coderage.spillway\nStatus: Ready", nil
		}
		return "", nil
	})
	if err := serviceInstall(); err != nil {
		t.Fatal(err)
	}
	// It must have kept asking until the task stopped, and only then created.
	queries, create := 0, -1
	for i, c := range *calls {
		if strings.HasPrefix(c, "/Query") {
			queries++
		}
		if strings.HasPrefix(c, "/Create") && create < 0 {
			create = i
		}
	}
	if queries == 0 {
		t.Fatalf("never waited for the task to stop: %v", *calls)
	}
	for i, c := range *calls {
		if strings.HasPrefix(c, "/Query") && i > create {
			continue // status checks after creating are fine
		}
	}
	if create >= 0 && idxOfArg(*calls, "/Query") > create {
		t.Errorf("created the task before waiting for the old one: %v", *calls)
	}
}

// /End fails when nothing is running and when nothing is registered — both
// ordinary on a first install — and neither may abort it.
func TestInstallProceedsWhenNothingIsRunning(t *testing.T) {
	calls := fakeSchtasks(t, func(args ...string) (string, error) {
		if args[0] == "/End" {
			return "ERROR: The system cannot find the file specified.", errors.New("exit status 1")
		}
		return "", nil
	})
	if err := serviceInstall(); err != nil {
		t.Fatalf("a failed /End aborted a first install: %v", err)
	}
	if idxOfArg(*calls, "/Create") == len(*calls) {
		t.Errorf("task never registered: %v", *calls)
	}
}

// Uninstall must stop the process too, or it outlives the task that owns it
// and there is nothing left to stop it by.
func TestUninstallStopsBeforeDeleting(t *testing.T) {
	calls := fakeSchtasks(t, nil)
	if err := serviceUninstall(); err != nil {
		t.Fatal(err)
	}
	if idxOfArg(*calls, "/End") > idxOfArg(*calls, "/Delete") {
		t.Errorf("/End must precede /Delete: %v", *calls)
	}
}

func idxOfArg(all []string, want string) int {
	for i, s := range all {
		if strings.HasPrefix(s, want+" ") || s == want {
			return i
		}
	}
	return len(all)
}

// A /Run that succeeds and dies a second later is the failure to retry: the
// old daemon still had the port, the replacement could not bind, and the
// scheduler recorded a completed run rather than a failure — so nothing
// restarted it and the machine had no proxy until the next logon.
func TestInstallRetriesUntilTheDaemonStays(t *testing.T) {
	runs, ready := 0, 0
	fakeSchtasks(t, func(args ...string) (string, error) {
		switch args[0] {
		case "/Run":
			runs++
			return "", nil
		case "/Query":
			// The first two starts die immediately; the third stays up.
			if runs >= 3 {
				return "Status: Running", nil
			}
			ready++
			return "Status: Ready", nil
		}
		return "", nil
	})
	if err := serviceInstall(); err != nil {
		t.Fatal(err)
	}
	if runs < 3 {
		t.Errorf("gave up after %d /Run attempts; a start that dies must be retried", runs)
	}
	if ready == 0 {
		t.Error("never checked whether the task was still running after starting it")
	}
}

// It must not retry for ever, and it must report rather than claim success.
func TestInstallGivesUpAndSaysSoWhenTheDaemonNeverStays(t *testing.T) {
	runs := 0
	fakeSchtasks(t, func(args ...string) (string, error) {
		if args[0] == "/Run" {
			runs++
		}
		if args[0] == "/Query" {
			return "Status: Ready", nil
		}
		return "", nil
	})
	// serviceInstall reports this on stderr rather than failing: the task is
	// registered, and a logon will start it even if this attempt could not.
	if err := serviceInstall(); err != nil {
		t.Fatalf("registration itself should still succeed: %v", err)
	}
	if runs < 2 {
		t.Errorf("only tried %d time(s)", runs)
	}
	if runs > 20 {
		t.Errorf("retried %d times — that is a hang, not a retry", runs)
	}
}
