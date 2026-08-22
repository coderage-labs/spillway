package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestTaskXMLIsValidAndComplete(t *testing.T) {
	x, err := taskXML(`C:\Users\me\bin\spillway.exe`, `C:\Users\me\AppData\Local\spillway\spillway.log`)
	if err != nil {
		t.Fatal(err)
	}
	var parsed any
	if err := parseTaskXML([]byte(x), &parsed); err != nil {
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
		"spillway.exe",
		"server",
		"--log-file",
	} {
		if !strings.Contains(x, want) {
			t.Errorf("task definition missing %q", want)
		}
	}
	// The log reaches disk because the daemon opens the file itself, not
	// because a shell redirects into it — the shell is what orphaned the
	// daemon on /End.
	if strings.Contains(x, "*&gt;&gt;") || strings.Contains(x, "*>>") {
		t.Error("shell redirection is back, which means the action is a shell again")
	}
}

// decodeAction pulls the command and arguments back out of the task
// definition. Asserting against the raw XML would be testing the wrong layer:
// encoding/xml escapes quotes and ampersands, so a check against the raw text
// fails on correct output. What matters is what the scheduler runs.
func decodeAction(t *testing.T, doc string) (command, arguments string) {
	t.Helper()
	var parsed struct {
		Actions struct {
			Exec struct {
				Command   string `xml:"Command"`
				Arguments string `xml:"Arguments"`
			} `xml:"Exec"`
		} `xml:"Actions"`
	}
	if err := parseTaskXML([]byte(doc), &parsed); err != nil {
		t.Fatalf("task definition is not valid XML: %v", err)
	}
	return parsed.Actions.Exec.Command, parsed.Actions.Exec.Arguments
}

// The action must be the daemon, not a shell running the daemon.
//
// This is the regression guard for the bug a Windows runner found: with
// powershell as the action, powershell was the task's process and spillway
// was its child, so schtasks /End killed the shell, reported success, and
// left the daemon orphaned holding the port. Every upgrade then kept serving
// from the old binary. A shell here also reintroduces command injection,
// since the binary path contains the user's name.
func TestTaskActionIsTheBinaryNotAShell(t *testing.T) {
	evil := `C:\Users\o'brien'; Remove-Item C:\ -Recurse; '\spillway.exe`
	x, err := taskXML(evil, `C:\logs\a&b.log`)
	if err != nil {
		t.Fatal(err)
	}
	cmd, args := decodeAction(t, x)

	for _, shell := range []string{"powershell", "pwsh", "cmd.exe", "cmd /c", "/c "} {
		if strings.Contains(strings.ToLower(cmd), shell) {
			t.Fatalf("the action runs a shell (%q), so ending the task will not stop the daemon: %s", shell, cmd)
		}
	}
	if cmd != evil {
		t.Errorf("Command should be the binary path verbatim:\n got %q\nwant %q", cmd, evil)
	}
	// Nothing shell-ish smuggled into the arguments either.
	for _, bad := range []string{"*>>", "&", "|", ";"} {
		if strings.Contains(args, bad) && bad != "&" {
			t.Errorf("arguments contain shell syntax %q: %s", bad, args)
		}
	}
	if !strings.Contains(args, "server") || !strings.Contains(args, "--log-file") {
		t.Errorf("arguments do not start the server with a log file: %s", args)
	}
	// The log path survives XML round-trip and stays one argument.
	if !strings.Contains(args, `a&b.log`) {
		t.Errorf("log path did not survive XML round-trip:\n%s", args)
	}
}

// A log path with a space or a quote in it must stay a single argument.
// Windows splits a command line in the callee, so an unquoted path with a
// space becomes two arguments and --log-file silently gets the wrong value.
func TestLogPathIsOneArgument(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`C:\logs\spillway.log`, `C:\logs\spillway.log`},
		{`C:\Program Files\s.log`, `"C:\Program Files\s.log"`},
		{`C:\a b\c"d.log`, `"C:\a b\c\"d.log"`},
		{`C:\ends\with\slash\ `, `"C:\ends\with\slash\ "`},
	} {
		if got := quoteArg(tc.in); got != tc.want {
			t.Errorf("quoteArg(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
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

// parseTaskXML decodes the document these tests work with.
//
// taskXML returns a Go string — UTF-8 bytes — carrying a UTF-16 declaration,
// because the declaration describes the file taskXMLFile writes, not the
// string in memory. encoding/xml refuses that pairing outright, so the tests
// hand it a CharsetReader that passes the bytes through: they are checking
// structure and escaping, and the encoding is checked where it is real, by
// the integration test that hands the file to a scheduler.
func parseTaskXML(b []byte, v any) error {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	return d.Decode(v)
}

// The bytes and the declaration have to agree, or schtasks answers
// "unable to switch the encoding" and registers nothing. This is checkable
// without Windows, and it is the failure that shipped: the document said
// UTF-8, was written as UTF-8, and a real scheduler rejected it anyway.
func TestTaskXMLFileIsUTF16WithAMatchingDeclaration(t *testing.T) {
	b, err := taskXMLFile(`C:\Users\me\spillway.exe`, `C:\logs\spillway.log`)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
		t.Fatalf("no UTF-16LE BOM: % x", b[:min(4, len(b))])
	}
	// Decoded back, the declaration must say UTF-16 — the pairing schtasks
	// accepts. Either half alone is the rejection.
	var u16 []uint16
	for i := 2; i+1 < len(b); i += 2 {
		u16 = append(u16, uint16(b[i])|uint16(b[i+1])<<8)
	}
	text := string(utf16.Decode(u16))
	if !strings.Contains(text, `encoding="UTF-16"`) {
		t.Errorf("declaration does not say UTF-16:\n%s", firstLine(text))
	}
	if strings.Contains(text, "UTF-8") {
		t.Errorf("a UTF-8 declaration over UTF-16 bytes is the exact rejected combination:\n%s", firstLine(text))
	}
	// And the content survived the transcode.
	if !strings.Contains(text, "spillway.exe") {
		t.Errorf("binary path lost in encoding:\n%s", text[:min(300, len(text))])
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
