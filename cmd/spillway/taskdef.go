package main

// The Windows Scheduled Task definition.
//
// Deliberately not named *_windows.go. The escaping here is the part that can
// be wrong in a way that matters — a username with a quote in it becomes an
// injection into a command the scheduler runs at every logon — and that
// suffix would put it out of reach of the test suite on the only platforms
// this is developed on.

import (
	"encoding/xml"
	"strings"
	"unicode/utf16"
)

// taskName() is what appears in Task Scheduler. Backslash-prefixed so it lands
// in the root folder rather than under \Microsoft\Windows\.
func taskName() string { return `\` + serviceLabel }

// taskXML renders the Scheduled Task definition.
//
// Built with encoding/xml rather than string concatenation: the binary path
// contains the user's name, which can contain anything, and a mis-escaped
// path here is an injection into a scheduled command.
func taskXML(binPath, logPath string) (string, error) {
	type exec struct {
		Command   string `xml:"Command"`
		Arguments string `xml:"Arguments"`
	}
	doc := struct {
		XMLName xml.Name `xml:"Task"`
		Version string   `xml:"version,attr"`
		NS      string   `xml:"xmlns,attr"`

		RegistrationInfo struct {
			Description string `xml:"Description"`
		} `xml:"RegistrationInfo"`
		Triggers struct {
			Logon struct {
				Enabled bool `xml:"Enabled"`
			} `xml:"LogonTrigger"`
		} `xml:"Triggers"`
		Principals struct {
			Principal struct {
				ID        string `xml:"id,attr"`
				LogonType string `xml:"LogonType"`
				RunLevel  string `xml:"RunLevel"`
			} `xml:"Principal"`
		} `xml:"Principals"`
		Settings struct {
			MultipleInstancesPolicy    string `xml:"MultipleInstancesPolicy"`
			DisallowStartIfOnBatteries bool   `xml:"DisallowStartIfOnBatteries"`
			StopIfGoingOnBatteries     bool   `xml:"StopIfGoingOnBatteries"`
			StartWhenAvailable         bool   `xml:"StartWhenAvailable"`
			Hidden                     bool   `xml:"Hidden"`
			ExecutionTimeLimit         string `xml:"ExecutionTimeLimit"`
			RestartOnFailure           struct {
				Interval string `xml:"Interval"`
				Count    int    `xml:"Count"`
			} `xml:"RestartOnFailure"`
		} `xml:"Settings"`
		Actions struct {
			Context string `xml:"Context,attr"`
			Exec    exec   `xml:"Exec"`
		} `xml:"Actions"`
	}{
		Version: "1.2",
		NS:      "http://schemas.microsoft.com/windows/2004/02/mit/task",
	}
	doc.RegistrationInfo.Description = "spillway — pooled LLM proxy"
	doc.Triggers.Logon.Enabled = true
	doc.Principals.Principal.ID = "Author"
	// InteractiveToken: run in the logged-on user's session, which is the
	// only place Credential Manager secrets are readable.
	doc.Principals.Principal.LogonType = "InteractiveToken"
	doc.Principals.Principal.RunLevel = "LeastPrivilege"
	doc.Settings.MultipleInstancesPolicy = "IgnoreNew"
	// A laptop on battery still wants its proxy; the scheduler's defaults
	// would otherwise refuse to start it and stop it mid-session.
	doc.Settings.DisallowStartIfOnBatteries = false
	doc.Settings.StopIfGoingOnBatteries = false
	doc.Settings.StartWhenAvailable = true
	doc.Settings.Hidden = false
	// PT0S = no limit. The default is 72 hours, after which the scheduler
	// would kill a perfectly healthy daemon.
	doc.Settings.ExecutionTimeLimit = "PT0S"
	// The nearest thing to launchd's KeepAlive.
	doc.Settings.RestartOnFailure.Interval = "PT1M"
	doc.Settings.RestartOnFailure.Count = 3
	doc.Actions.Context = "Author"
	// The binary itself, not powershell running it.
	//
	// The task used to run `powershell -Command "& 'spillway.exe' server *>>
	// 'log'"`, for the redirect and a hidden window. That made powershell the
	// task's process and spillway its child, and schtasks /End ends the
	// task's process: on a real scheduler it killed the shell, reported
	// "terminated successfully", and left the daemon orphaned and still
	// holding port 7657. The replacement could not bind, so every upgrade
	// kept serving from the old binary. Proven on a Windows runner —
	// powershell gone, spillway.exe alive with the port.
	//
	// The daemon writes its own log via --log-file, so no shell is needed
	// for that either.
	doc.Actions.Exec.Command = binPath
	var quoted []string
	for _, a := range serverArgs(logPath) {
		quoted = append(quoted, quoteArg(a))
	}
	doc.Actions.Exec.Arguments = strings.Join(quoted, " ")

	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	// UTF-16, and the bytes on disk have to match: schtasks parses the file
	// with a reader that will not switch encodings mid-document, so a UTF-8
	// declaration — whatever the bytes are — gets
	//
	//	(1,40)::ERROR: unable to switch the encoding
	//
	// and the task is never registered. The comment that used to be here
	// claimed UTF-8 "is accepted everywhere tested", which was true only in
	// the sense that it had been tested nowhere. See taskXMLFile for the
	// bytes; the two have to agree, so nothing writes this string directly.
	return `<?xml version="1.0" encoding="UTF-16"?>` + "\n" + string(body) + "\n", nil
}

// taskXMLFile is the exact byte sequence to hand schtasks /XML: UTF-16LE
// with a BOM, matching the declaration above.
//
// It exists so the caller cannot choose an encoding of its own. The first
// version of the integration test did, transcoding to UTF-16 while the
// document still said UTF-8, and produced the same rejection from the other
// direction — a test and a caller disagreeing about the file format is not
// something either of them can catch.
func taskXMLFile(binPath, logPath string) ([]byte, error) {
	doc, err := taskXML(binPath, logPath)
	if err != nil {
		return nil, err
	}
	out := []byte{0xFF, 0xFE} // BOM, little-endian
	// utf16.Encode, not a hand-rolled loop: anything above the BMP needs a
	// surrogate pair, and a path can contain one.
	for _, u := range utf16.Encode([]rune(doc)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out, nil
}

// quoteArg wraps a path for a Windows command line, where a quote inside an
// argument is escaped by doubling the preceding backslashes and prefixing the
// quote. The path contains the user's name, which can contain anything.
func quoteArg(s string) string {
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			slashes++
			b.WriteByte('\\')
		case '"':
			// Double every backslash that ran up to the quote, then escape it.
			b.WriteString(strings.Repeat(`\`, slashes+1))
			b.WriteByte('"')
			slashes = 0
		default:
			slashes = 0
			b.WriteByte(s[i])
		}
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}
