package main

import (
	"runtime"
	"strings"
	"testing"
)

// A binary that cannot say what it is makes every later bug report guesswork.
// The linker variables are set by a workflow, which the compiler cannot check
// — so at minimum, assert the fallback produces something identifying.
func TestBuildInfoAlwaysIdentifiesSomething(t *testing.T) {
	got := buildInfo()
	if got == "" {
		t.Fatal("buildInfo is empty")
	}
	if !strings.Contains(got, runtime.Version()) {
		t.Errorf("%q omits the Go version — the toolchain is half the answer to "+
			"'why does this binary behave differently'", got)
	}
	// Unstamped builds must say so rather than claiming a version.
	if version == "" && !strings.HasPrefix(got, "dev") {
		t.Errorf("%q does not announce itself as an unreleased build", got)
	}
}

// The stamped path is what ships, and it is the one nothing else exercises.
func TestBuildInfoUsesTheLinkerValues(t *testing.T) {
	ov, oc, od := version, commit, date
	t.Cleanup(func() { version, commit, date = ov, oc, od })

	version, commit, date = "v1.2.3", "0123456789abcdef", "2026-08-22T06:00:00Z"
	got := buildInfo()
	for _, want := range []string{"v1.2.3", "0123456", "2026-08-22"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing %q", got, want)
		}
	}
	// Abbreviated, not truncated mid-render: the full sha is noise in a log
	// line that is already long.
	if strings.Contains(got, "0123456789abcdef") {
		t.Errorf("%q carries the full commit sha", got)
	}
	// The date is a day, not a timestamp.
	if strings.Contains(got, "T06:00:00Z") {
		t.Errorf("%q carries a full timestamp", got)
	}
}
