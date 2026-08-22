package main

import (
	"encoding/xml"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestPlistXMLIsValidAndComplete(t *testing.T) {
	x := plistXML("/usr/local/bin/spillway", "/tmp/out.log", "/tmp/err.log")

	var parsed any
	if err := xml.Unmarshal([]byte(x), &parsed); err != nil {
		t.Fatalf("plist is not valid XML: %v", err)
	}
	for _, want := range []string{
		serviceLabel, "/usr/local/bin/spillway", "<string>server</string>",
		"<key>KeepAlive</key><true/>", "<key>RunAtLoad</key><true/>",
		"/tmp/out.log", "/tmp/err.log",
	} {
		if !strings.Contains(x, want) {
			t.Errorf("plist missing %q", want)
		}
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
