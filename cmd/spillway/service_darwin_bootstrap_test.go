//go:build darwin

package main

import (
	"errors"
	"strings"
	"testing"
)

// fakeLaunchctl swaps in a scripted launchctl for one test.
func fakeLaunchctl(t *testing.T, fn func(args ...string) (string, error)) *[]string {
	t.Helper()
	var calls []string
	oldCmd, oldDelay := launchctl, bootstrapRetryDelay
	launchctl = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return fn(args...)
	}
	bootstrapRetryDelay = 0
	t.Cleanup(func() { launchctl, bootstrapRetryDelay = oldCmd, oldDelay })
	return &calls
}

// launchd rejects a bootstrap for a moment after the previous job goes away.
// An earlier version gave up on the first error whenever the old job was
// already gone — which is the common case, since `brew upgrade` kills the
// daemon by deleting its binary — and left the machine with no daemon at all.
func TestBootstrapRetriesPastATransientFailure(t *testing.T) {
	n := 0
	calls := fakeLaunchctl(t, func(...string) (string, error) {
		n++
		if n < 3 {
			return "Bootstrap failed: 5: Input/output error", errors.New("exit status 5")
		}
		return "", nil
	})
	if _, err := bootstrapService("/tmp/x.plist"); err != nil {
		t.Fatalf("gave up on a transient failure: %v", err)
	}
	if len(*calls) != 3 {
		t.Errorf("made %d attempts, want 3: %v", len(*calls), *calls)
	}
}

// A genuine failure still has to surface, with launchd's own output attached
// so the message says more than "it did not work".
func TestBootstrapGivesUpAndReportsWhy(t *testing.T) {
	calls := fakeLaunchctl(t, func(...string) (string, error) {
		return "Load failed: 5: Input/output error", errors.New("exit status 5")
	})
	out, err := bootstrapService("/tmp/x.plist")
	if err == nil {
		t.Fatal("a permanently failing bootstrap reported success")
	}
	if !strings.Contains(out, "Load failed") {
		t.Errorf("launchd's own output was dropped: %q", out)
	}
	if len(*calls) < 2 {
		t.Errorf("only %d attempt(s) before giving up", len(*calls))
	}
}

func TestBootstrapDoesNotRetryWhenItWorks(t *testing.T) {
	calls := fakeLaunchctl(t, func(...string) (string, error) { return "", nil })
	if _, err := bootstrapService("/tmp/x.plist"); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Errorf("made %d attempts for a clean bootstrap: %v", len(*calls), *calls)
	}
}
