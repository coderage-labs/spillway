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
