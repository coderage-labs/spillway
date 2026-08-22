package main

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestTaskXMLIsValidAndComplete(t *testing.T) {
	x, err := taskXML(`C:\Users\me\bin\spillway.exe`, `C:\Users\me\AppData\Local\spillway\spillway.log`)
	if err != nil {
		t.Fatal(err)
	}
	var parsed any
	if err := xml.Unmarshal([]byte(x), &parsed); err != nil {
		t.Fatalf("task definition is not valid XML: %v", err)
	}
	for _, want := range []string{
		"LogonTrigger",
		// The session matters: Credential Manager secrets are unreadable from
		// anywhere else, so a task that runs elsewhere installs fine and then
		// cannot find a single account.
		"<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>LeastPrivilege</RunLevel>",
		// The scheduler's default is a 72-hour limit, after which it kills a
		// perfectly healthy daemon.
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		// A laptop on battery still wants its proxy.
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
		"RestartOnFailure",
		"powershell.exe",
		"-WindowStyle Hidden",
		"spillway.exe",
		"server",
	} {
		if !strings.Contains(x, want) {
			t.Errorf("task definition missing %q", want)
		}
	}
	// Without a redirect the daemon's log goes nowhere, and *>> rather than
	// >> because slog writes to stderr.
	if !strings.Contains(x, "*&gt;&gt;") && !strings.Contains(x, "*>>") {
		t.Error("no output redirection: the daemon's log would be discarded")
	}
}

// decodeArguments pulls the PowerShell command line back out of the task
// definition. Asserting against the raw XML would be testing the wrong layer:
// encoding/xml renders a quote as &#39;, so a check for doubled quotes in the
// raw text fails on correct output. What matters is what the scheduler
// actually hands PowerShell.
func decodeArguments(t *testing.T, doc string) string {
	t.Helper()
	var parsed struct {
		Actions struct {
			Exec struct {
				Command   string `xml:"Command"`
				Arguments string `xml:"Arguments"`
			} `xml:"Exec"`
		} `xml:"Actions"`
	}
	if err := xml.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("task definition is not valid XML: %v", err)
	}
	return parsed.Actions.Exec.Arguments
}

// A Windows username can contain a quote, and the binary path contains the
// username. Unescaped, that ends the PowerShell string literal and the rest
// of the path becomes commands the scheduler runs at every logon.
func TestTaskXMLEscapesHostilePaths(t *testing.T) {
	evil := `C:\Users\o'brien'; Remove-Item C:\ -Recurse; '\spillway.exe`
	x, err := taskXML(evil, `C:\logs\a&b.log`)
	if err != nil {
		t.Fatal(err)
	}
	args := decodeArguments(t, x)

	// Every single quote doubled, so the injected text stays inside the
	// string literal instead of escaping into a new statement.
	if strings.Contains(args, `o'brien';`) {
		t.Errorf("single quote not doubled — this is a command injection:\n%s", args)
	}
	if !strings.Contains(args, `o''brien''`) {
		t.Errorf("expected doubled quotes in the escaped path:\n%s", args)
	}
	// The ampersand round-trips through XML rather than breaking the document.
	if !strings.Contains(args, `a&b.log`) {
		t.Errorf("log path did not survive XML round-trip:\n%s", args)
	}
	// The command must still be one PowerShell statement: an odd number of
	// quotes means one of them terminated the literal.
	if n := strings.Count(args, "'"); n%2 != 0 {
		t.Errorf("odd number of single quotes (%d) — the literal is unbalanced:\n%s", n, args)
	}
}

// The task must land in the root folder, not under \Microsoft\Windows\.
func TestTaskNameIsRooted(t *testing.T) {
	if !strings.HasPrefix(taskName, `\`) {
		t.Errorf("taskName = %q, want a leading backslash", taskName)
	}
	if !strings.Contains(taskName, serviceLabel) {
		t.Errorf("taskName = %q, want it to carry the service label", taskName)
	}
}
