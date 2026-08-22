package notify

// Windows toast notifications.
//
// Deliberately not named *_windows.go: that suffix would constrain the file
// to Windows builds and take escapeXML — the part with an injection bug in it
// if anyone gets it wrong — out of reach of the test suite on every other
// platform. Nothing here is Windows-only at compile time; only the caller is.
//
// No third-party module: PowerShell can reach the WinRT toast API directly,
// and a dependency for one notification is not worth the supply chain. The
// script is long but it is the documented minimum — a toast needs a
// registered AppUserModelID, and using an existing one (PowerShell's own) is
// what avoids having to install a shortcut in the Start Menu first.

import (
	"context"
	"os/exec"
	"strings"
)

// powerShellAppID is PowerShell's own AppUserModelID. A toast must be sent
// under a registered application, and registering our own would mean writing
// a Start Menu shortcut at install time. Borrowing this one costs a
// PowerShell icon on the notification and nothing else.
const powerShellAppID = `{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe`

// escapeXML quotes a string for a toast's XML payload. Notification text
// carries account names, so this is not decorative.
func escapeXML(s string) string {
	return strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;",
	).Replace(s)
}

func notifyWindows(ctx context.Context, title, body string) error {
	script := `
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType=WindowsRuntime] > $null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom, ContentType=WindowsRuntime] > $null
$xml = New-Object Windows.Data.Xml.Dom.XmlDocument
$xml.LoadXml(@"
<toast><visual><binding template="ToastGeneric"><text>` + escapeXML(title) + `</text><text>` + escapeXML(body) + `</text></binding></visual></toast>
"@)
$toast = New-Object Windows.UI.Notifications.ToastNotification $xml
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('` + powerShellAppID + `').Show($toast)
`
	// -NoProfile so a user's profile script cannot slow down or break a
	// notification; -NonInteractive so it can never block waiting for input.
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-Command", script)
	return cmd.Run()
}
