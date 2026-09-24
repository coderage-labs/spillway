package main

// Issue #158: a pin overrides every routing decision spillway makes —
// threshold, priority, stickiness, rotation — and `spillway status` said
// nothing about it. A pinned pool and an automatic one printed an identical
// table while behaving nothing alike, and confirming a pin had been cleared
// meant reading /api/state and inferring from an omitempty field being
// ABSENT.
//
// These drive statusTable against a stub admin server. Never dialAdmin: that
// resolves the real config and the running daemon's listener.

import (
	"strings"
	"testing"
)

// twoPoolAccounts: the pinned one is also the one serving, which is the common
// case and the one where "which of these is the pin" is hardest to see.
const twoPoolAccounts = `[
	{"name":"arena","type":"claude-oauth","state":"ok","inFlight":2,
	 "quotaWindows":[{"name":"5h","limit":100,"used":40}]},
	{"name":"beta","type":"claude-oauth","state":"ok","inFlight":0,
	 "quotaWindows":[{"name":"5h","limit":100,"used":10}]}
]`

func renderStatusWith(t *testing.T, accountsJSON, stateJSON string) string {
	t.Helper()
	var buf strings.Builder
	if err := statusTable(stubAdminWithState(t, accountsJSON, stateJSON), &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestStatusAnnouncesThePinnedAccount is the issue: say a pin is set, and to
// which account.
func TestStatusAnnouncesThePinnedAccount(t *testing.T) {
	out := renderStatusWith(t, twoPoolAccounts, `{"usable":2,"total":2,"pinned":"arena"}`)

	if !strings.Contains(out, "pinned to arena") {
		t.Errorf("status does not name the pinned account:\n%s", out)
	}
	// What the pin SUPPRESSES, not merely that it exists. Without this the
	// reader still has to know what a pin does to act on the line.
	if !strings.Contains(out, "rotation, priority and thresholds are bypassed") {
		t.Errorf("status does not say what the pin suppresses:\n%s", out)
	}
	// And how to undo it. Note the flag: bare `spillway switch` REPORTS the
	// pin, it does not clear it, so printing that would leave a pin set by
	// someone who believed they had followed the instruction.
	if !strings.Contains(out, "spillway switch --auto") {
		t.Errorf("status does not say how to restore automatic selection:\n%s", out)
	}
	// The banner goes ABOVE the table: it is the mode every row below is
	// being read in, not a footnote about one of them.
	pin := strings.Index(out, "pinned to arena")
	head := strings.Index(out, "ACCOUNT")
	if head < 0 {
		t.Fatalf("status lost its table header:\n%s", out)
	}
	if pin > head {
		t.Errorf("the pin banner is below the table, where it reads as a footnote:\n%s", out)
	}
}

// TestStatusPinNamesOnlyThePinnedAccount: with the pinned account ALSO the
// one serving, the banner must still be unambiguous about which of the two
// is pinned.
func TestStatusPinNamesOnlyThePinnedAccount(t *testing.T) {
	out := renderStatusWith(t, twoPoolAccounts, `{"usable":2,"total":2,"pinned":"arena"}`)

	banner := strings.SplitN(out, "\n", 2)[0]
	if !strings.Contains(banner, "arena") {
		t.Fatalf("the first line does not name the pinned account: %q", banner)
	}
	if strings.Contains(banner, "beta") {
		t.Errorf("the pin banner %q names an account that is not pinned", banner)
	}
	// Both accounts are still in the table, and neither row grew a marker:
	// the rows must be byte-identical to the unpinned render, so anything
	// reading the table sees exactly what it saw before.
	unpinned := renderStatusWith(t, twoPoolAccounts, `{"usable":2,"total":2}`)
	if got := out[strings.Index(out, "ACCOUNT"):]; got != unpinned {
		t.Errorf("pinning changed the table itself:\n--- pinned ---\n%s\n--- unpinned ---\n%s", got, unpinned)
	}
}

// TestStatusUnpinnedRendersExactlyAsBefore: no stray marker, no blank header
// line, no leading whitespace. Asserted against the whole output rather than
// by searching for the word "pinned", because the regression this guards
// against is a banner that prints empty, not one that prints wrong.
func TestStatusUnpinnedRendersExactlyAsBefore(t *testing.T) {
	out := renderStatusWith(t, twoPoolAccounts, `{"usable":2,"total":2}`)

	const want = "ACCOUNT  TYPE          STATE  IN FLIGHT  5H LEFT\n" +
		"arena    claude-oauth  ok             2      60%\n" +
		"beta     claude-oauth  ok             0      90%\n"
	if out != want {
		t.Errorf("unpinned status changed:\n--- got ---\n%q\n--- want ---\n%q", out, want)
	}
}

// TestStatusSaysNothingAboutAPinWhenTheDaemonHasNoState: an older daemon
// serves no /api/state at all. That must print no banner rather than fail
// the command or invent an empty one.
func TestStatusSaysNothingAboutAPinWhenTheDaemonHasNoState(t *testing.T) {
	var buf strings.Builder
	// stubAdmin (status_overage_test.go) 404s everything but /api/accounts.
	if err := statusTable(stubAdmin(t, twoPoolAccounts), &buf); err != nil {
		t.Fatalf("status failed because /api/state was missing: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "pinned") {
		t.Errorf("status invented a pin banner from a missing endpoint:\n%s", out)
	}
	if !strings.HasPrefix(out, "ACCOUNT") {
		t.Errorf("status grew a line above the table with no pin set:\n%q", out)
	}
}
