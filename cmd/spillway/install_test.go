package main

import (
	"errors"
	"strings"
	"testing"
)

// fakeSteps installs a step list for one test and records what ran.
func fakeSteps(t *testing.T, list []step) {
	t.Helper()
	old := installSteps
	installSteps = func() []step { return list }
	t.Cleanup(func() { installSteps = old })
}

func okStep(name string, ran *[]string) step {
	return step{
		name:      name,
		install:   func(installOpts) error { *ran = append(*ran, name+":install"); return nil },
		uninstall: func() error { *ran = append(*ran, name+":uninstall"); return nil },
		status:    func() error { *ran = append(*ran, name+":status"); return nil },
	}
}

// One step failing must not cost the others. A box can want the daemon and
// not the plugin, and stopping at the first problem would leave the machine
// in a state no one asked for.
func TestInstallContinuesPastAFailingStep(t *testing.T) {
	var ran []string
	bad := okStep("bad", &ran)
	bad.install = func(installOpts) error { ran = append(ran, "bad:install"); return errors.New("nope") }
	fakeSteps(t, []step{okStep("first", &ran), bad, okStep("last", &ran)})

	out := captureStdout(t, func() {
		err := runInstall([]string{"install"})
		if err == nil {
			t.Fatal("a failing step must make the whole install fail")
		}
		if !strings.Contains(err.Error(), "bad") {
			t.Errorf("error does not name the failing step: %v", err)
		}
	})
	want := []string{"first:install", "bad:install", "last:install"}
	if strings.Join(ran, ",") != strings.Join(want, ",") {
		t.Errorf("ran %v, want %v", ran, want)
	}
	if !strings.Contains(out, "failed: nope") {
		t.Errorf("the failure was not reported inline:\n%s", out)
	}
}

// An unavailable step is skipped with its reason, and is not a failure — no
// `claude` on the box is a legitimate way to run only the daemon.
func TestUnavailableStepIsSkippedNotFailed(t *testing.T) {
	var ran []string
	gone := okStep("plugin", &ran)
	gone.unavailable = func() string { return "`claude` is not on PATH" }
	fakeSteps(t, []step{gone, okStep("service", &ran)})

	out := captureStdout(t, func() {
		if err := runInstall([]string{"install"}); err != nil {
			t.Fatalf("a skipped step must not fail the install: %v", err)
		}
	})
	if strings.Contains(strings.Join(ran, ","), "plugin") {
		t.Errorf("the unavailable step ran anyway: %v", ran)
	}
	if !strings.Contains(out, "skipped: `claude` is not on PATH") {
		t.Errorf("no reason given for the skip:\n%s", out)
	}
}

func TestInstallActionsAndFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"install"}, "a:install"},
		{[]string{"uninstall"}, "a:uninstall"},
		{[]string{"install", "status"}, "a:status"},
	} {
		var ran []string
		fakeSteps(t, []step{okStep("a", &ran)})
		captureStdout(t, func() {
			if err := runInstall(tc.args); err != nil {
				t.Fatalf("%v: %v", tc.args, err)
			}
		})
		if strings.Join(ran, ",") != tc.want {
			t.Errorf("%v ran %v, want %s", tc.args, ran, tc.want)
		}
	}

	// --plugin-source has to reach the step, in both spellings.
	for _, args := range [][]string{
		{"install", "--plugin-source", "me/mine"},
		{"install", "--plugin-source=me/mine"},
	} {
		var got string
		fakeSteps(t, []step{{name: "a", install: func(o installOpts) error { got = o.pluginSource; return nil }}})
		captureStdout(t, func() {
			if err := runInstall(args); err != nil {
				t.Fatal(err)
			}
		})
		if got != "me/mine" {
			t.Errorf("%v: pluginSource = %q, want me/mine", args, got)
		}
	}

	// The default is the public marketplace, not a local path.
	var def string
	fakeSteps(t, []step{{name: "a", install: func(o installOpts) error { def = o.pluginSource; return nil }}})
	captureStdout(t, func() { runInstall([]string{"install"}) })
	if def != DefaultPluginSource {
		t.Errorf("default source = %q, want %q", def, DefaultPluginSource)
	}
}

func TestUnknownActionAndFlagAreRefused(t *testing.T) {
	var ran []string
	fakeSteps(t, []step{okStep("a", &ran)})
	captureStdout(t, func() {
		if err := runInstall([]string{"wat"}); err == nil {
			t.Error("unknown action accepted")
		}
		if err := runInstall([]string{"install", "--nope"}); err == nil {
			t.Error("unknown flag accepted")
		}
		if err := runInstall([]string{"install", "--plugin-source"}); err == nil {
			t.Error("--plugin-source accepted with no value")
		}
	})
}

func TestMarketplaceNameIsMatchedWhole(t *testing.T) {
	const listing = `Configured marketplaces:

  ❯ claude-plugins-official
    Source: GitHub (anthropics/claude-plugins-official)

  ❯ spillway
    Source: Directory (/Users/x/Repos/spillway)
`
	if !hasSpillwayMarketplace(listing) {
		t.Error("did not find the spillway marketplace")
	}
	// A near-miss must not count: skipping the add on a false positive would
	// leave the real marketplace unregistered and the install silently short.
	near := strings.Replace(listing, "❯ spillway\n", "❯ spillway-extras\n", 1)
	if hasSpillwayMarketplace(near) {
		t.Error("spillway-extras matched as spillway")
	}
	if hasSpillwayMarketplace("Configured marketplaces:\n") {
		t.Error("matched an empty listing")
	}
}

func TestPluginBlockIsJustOurs(t *testing.T) {
	const listing = `Installed plugins:

  ❯ other@elsewhere
    Version: 9.9.9
    Status: ✔ enabled

  ❯ spillway@spillway
    Version: 0.1.0
    Status: ✔ enabled

  ❯ later@elsewhere
    Version: 1.0.0
`
	got := strings.Join(spillwayPluginBlock(listing), "\n")
	if !strings.Contains(got, "spillway@spillway") || !strings.Contains(got, "0.1.0") {
		t.Errorf("spillway's own stanza missing:\n%s", got)
	}
	for _, foreign := range []string{"other@elsewhere", "later@elsewhere", "9.9.9"} {
		if strings.Contains(got, foreign) {
			t.Errorf("leaked another plugin's detail (%s):\n%s", foreign, got)
		}
	}
	if len(spillwayPluginBlock("Installed plugins:\n\n  ❯ other@elsewhere\n")) != 0 {
		t.Error("reported a block when spillway is not installed")
	}
}
