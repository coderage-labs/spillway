package main

// The one thing about the Windows path that only a real Task Scheduler can
// answer: does the XML we generate actually register?
//
// Everything else here is unit-tested against a fake schtasks, which proves
// the call sequence and proves nothing about the document. A missing element,
// a wrong namespace or a version the scheduler rejects would pass every test
// in the suite and fail on the first machine that ran it. Nobody developing
// this has Windows; CI does.
//
// It registers under its own name, never the real one — running `go test` on
// a machine where spillway is installed must not disturb the service.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const probeTaskName = `\dev.coderage.spillway-test`

func TestTaskXMLRegistersWithARealScheduler(t *testing.T) {
	if _, err := exec.LookPath("schtasks"); err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("no schtasks on the CI runner, so this test is not running anywhere")
		}
		t.Skip("no schtasks on this machine")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "spillway.exe")
	log := filepath.Join(dir, "spillway.log")
	if err := os.WriteFile(bin, []byte("not a real binary"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := taskXML(bin, log)
	if err != nil {
		t.Fatal(err)
	}
	// UTF-16LE with a BOM: schtasks /XML rejects UTF-8 on some systems, and
	// this is the encoding the scheduler's own export produces.
	xmlPath := filepath.Join(dir, "task.xml")
	if err := os.WriteFile(xmlPath, utf16LE(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("schtasks", "/Create", "/TN", probeTaskName,
		"/XML", xmlPath, "/F").CombinedOutput()
	if err != nil {
		s := string(out)
		if strings.Contains(s, "Access is denied") || strings.Contains(s, "privilege") {
			// A locked-down machine may genuinely forbid this. CI may not:
			// skipping there would leave the only real coverage of this
			// document silently doing nothing, which is indistinguishable
			// from passing in a run without -v.
			if os.Getenv("GITHUB_ACTIONS") == "true" {
				t.Fatalf("CI could not register a task, so this never actually ran: %s", s)
			}
			t.Skipf("not permitted to register a task here: %s", s)
		}
		t.Fatalf("the scheduler rejected our XML: %v\n%s", err, s)
	}
	t.Cleanup(func() {
		exec.Command("schtasks", "/End", "/TN", probeTaskName).Run()
		exec.Command("schtasks", "/Delete", "/TN", probeTaskName, "/F").Run()
	})

	// Registered is not enough — it has to have kept the path we gave it.
	q, err := exec.Command("schtasks", "/Query", "/TN", probeTaskName, "/XML").CombinedOutput()
	if err != nil {
		t.Fatalf("registered but not queryable: %v\n%s", err, q)
	}
	got := strings.ReplaceAll(string(q), "\x00", "") // /XML answers in UTF-16
	if !strings.Contains(got, filepath.Base(bin)) {
		t.Errorf("the registered task does not mention our binary:\n%s", got)
	}
	if !strings.Contains(got, "server") {
		t.Errorf("the registered task does not run `server`:\n%s", got)
	}
}

// utf16LE encodes with a BOM, which is what schtasks /XML expects.
func utf16LE(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, r := range s {
		if r > 0xFFFF {
			r = 0xFFFD
		}
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}
