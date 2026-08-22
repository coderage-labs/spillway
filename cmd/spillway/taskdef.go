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
	"fmt"
	"strings"
)

// taskName is what appears in Task Scheduler. Backslash-prefixed so it lands
// in the root folder rather than under \Microsoft\Windows\.
const taskName = `\` + serviceLabel

// taskXML renders the Scheduled Task definition.
//
// Built with encoding/xml rather than string concatenation: the binary path
// contains the user's name, which can contain anything, and a mis-escaped
// path here is an injection into a scheduled command.
func taskXML(binPath, logPath string) (string, error) {
	// *>> redirects every stream, including stderr, which is where slog
	// writes. Without the redirect the daemon's log goes nowhere at all.
	command := fmt.Sprintf(`& '%s' server *>> '%s'`,
		strings.ReplaceAll(binPath, "'", "''"),
		strings.ReplaceAll(logPath, "'", "''"))

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
	doc.Actions.Exec.Command = "powershell.exe"
	doc.Actions.Exec.Arguments = "-NoProfile -NonInteractive -WindowStyle Hidden -Command " +
		`"` + strings.ReplaceAll(command, `"`, `\"`) + `"`

	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	// schtasks /XML rejects a file without the UTF-16 declaration on some
	// systems; UTF-8 with an explicit declaration is accepted everywhere
	// tested and avoids having to transcode.
	return xml.Header + string(body) + "\n", nil
}
