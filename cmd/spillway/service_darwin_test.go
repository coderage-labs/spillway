package main

import (
	"encoding/xml"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPlistXMLIsValidAndComplete(t *testing.T) {
	x := plistXML("/usr/local/bin/spillway", "/tmp/daemon.log", "/tmp/launchd.log")

	var parsed any
	if err := xml.Unmarshal([]byte(x), &parsed); err != nil {
		t.Fatalf("plist is not valid XML: %v", err)
	}
	for _, want := range []string{
		serviceLabel, "/usr/local/bin/spillway", "<string>server</string>",
		"<key>KeepAlive</key><true/>", "<key>RunAtLoad</key><true/>",
		// The daemon is told to open its own log; launchd's redirect goes
		// to the other file.
		"<string>--log-file</string>", "<string>/tmp/daemon.log</string>",
		"<key>StandardOutPath</key><string>/tmp/launchd.log</string>",
		"<key>StandardErrorPath</key><string>/tmp/launchd.log</string>",
	} {
		if !strings.Contains(x, want) {
			t.Errorf("plist missing %q", want)
		}
	}
}

// plistString returns the <string> value that follows <key>name</key>.
func plistString(t *testing.T, x, name string) string {
	t.Helper()
	m := regexp.MustCompile(
		`(?s)<key>` + regexp.QuoteMeta(name) + `</key>\s*<string>(.*?)</string>`).FindStringSubmatch(x)
	if m == nil {
		t.Fatalf("plist has no <key>%s</key><string>…</string>:\n%s", name, x)
	}
	return m[1]
}

// The one thing this plist must never do again (#171): redirect a process
// stream at the file the daemon opens for itself.
//
// That combination is worse than the unbounded log it replaced. launchd
// holds its descriptor open across a rename, so the moment the daemon
// rotated its log, launchd would carry on appending to the archived inode —
// two writers on one name, one of them moving it under the other, and the
// live file silently missing half the lines.
func TestPlistDoesNotRedirectAStreamAtTheDaemonsOwnLog(t *testing.T) {
	// A home of its own: this test only computes paths, but it must not be
	// able to name — let alone create — anything under the real
	// ~/Library/Logs, where the live daemon's log is.
	t.Setenv("HOME", t.TempDir())

	daemonLog, launchdLog, err := serviceLogPaths()
	if err != nil {
		t.Fatal(err)
	}
	if daemonLog == launchdLog {
		t.Fatal("serviceLogPaths returns one path for both the rotated log and launchd's redirect")
	}

	x := plistXML("/usr/local/bin/spillway", daemonLog, launchdLog)

	// What the daemon was told to open, read back out of the plist rather
	// than assumed.
	args := regexp.MustCompile(`<string>(.*?)</string>`).FindAllStringSubmatch(
		regexp.MustCompile(`(?s)<key>ProgramArguments</key>\s*<array>(.*?)</array>`).
			FindStringSubmatch(x)[1], -1)
	var ownLog string
	for i, a := range args {
		if a[1] == "--log-file" && i+1 < len(args) {
			ownLog = args[i+1][1]
		}
	}
	if ownLog == "" {
		t.Fatal("plist does not pass --log-file, so the daemon cannot own (or rotate) its log")
	}
	if ownLog != daemonLog {
		t.Fatalf("--log-file = %q; want the daemon log %q", ownLog, daemonLog)
	}
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		if got := plistString(t, x, key); got == ownLog {
			t.Errorf("%s redirects at %s, the file the daemon opens itself — "+
				"two writers, and launchd keeps the archived inode after a rotation", key, got)
		}
	}
}

// The upgrade path for a machine that already runs the agent: the file the
// old plist wrote is left alone and named, so the tail of it is still there
// to read and nobody has to guess why it stopped growing.
func TestInstallReportsTheLogItStoppedWriting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	legacy, err := legacyErrLog()
	if err != nil {
		t.Fatal(err)
	}
	daemonLog, launchdLog, err := serviceLogPaths()
	if err != nil {
		t.Fatal(err)
	}
	if legacy == daemonLog || legacy == launchdLog {
		t.Fatalf("the legacy path %q collides with a path the new agent writes", legacy)
	}

	// Nothing to say when there is no such file.
	var buf strings.Builder
	reportLegacyErrLog(&buf)
	if buf.String() != "" {
		t.Errorf("reported a legacy log that does not exist:\n%s", buf.String())
	}

	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("old log line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	reportLegacyErrLog(&buf)
	if !strings.Contains(buf.String(), legacy) {
		t.Errorf("install says nothing about the log it stopped writing:\n%s", buf.String())
	}

	// And it is still there, untouched: the tail of a 236 MB file is the
	// one thing an upgrade must not throw away.
	data, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatalf("the legacy log was removed or renamed by the upgrade: %v", err)
	}
	if string(data) != "old log line\n" {
		t.Errorf("legacy log content = %q; the upgrade must not touch it", data)
	}
}

func TestPlistXMLEscapesPaths(t *testing.T) {
	// A path with XML metacharacters must not break the plist.
	x := plistXML("/tmp/a&b/spill<way>", "/tmp/o.log", "/tmp/e.log")
	if strings.Contains(x, "a&b") || strings.Contains(x, "spill<way>") {
		t.Fatalf("path not escaped: %s", x)
	}
	var parsed any
	if err := xml.Unmarshal([]byte(x), &parsed); err != nil {
		t.Fatalf("escaped plist is not valid XML: %v", err)
	}
}

// The install path must come from selfPath, not from a second, private call
// to os.Executable + EvalSymlinks.
//
// This was the actual defect: selfPath was fixed to preserve symlinks and the
// service installers were not, because each had grown its own copy of the
// resolution. The status line was correct while the launchd plist still
// pointed at a Caskroom path that `brew upgrade` deletes.
//
// Parsed rather than grepped: the first version of this matched the string
// anywhere in the file and failed on the comment explaining the fix.
func TestServiceInstallUsesSelfPath(t *testing.T) {
	for _, f := range []string{"service_darwin.go", "service_windows.go"} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f, nil, 0) // no comments
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		var resolves, usesSelfPath bool
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if id, ok := fn.X.(*ast.Ident); ok &&
					id.Name == "filepath" && fn.Sel.Name == "EvalSymlinks" {
					resolves = true
				}
			case *ast.Ident:
				if fn.Name == "selfPath" {
					usesSelfPath = true
				}
			}
			return true
		})
		if resolves {
			t.Errorf("%s calls filepath.EvalSymlinks; the recorded path must survive "+
				"a package upgrade, so it has to keep the symlink", f)
		}
		if !usesSelfPath {
			t.Errorf("%s does not call selfPath to decide what to install", f)
		}
	}
}
