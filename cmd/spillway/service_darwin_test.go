package main

import (
	"encoding/xml"
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
