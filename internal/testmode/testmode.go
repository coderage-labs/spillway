// Package testmode holds assertions that mean different things on different
// platforms.
//
// It exists for one recurring problem: spillway protects its config, its CA
// private key and its admin token with mode 0600, and that number is
// meaningless on Windows. os.Chmod there toggles a read-only bit and nothing
// else, so Stat reports 0666 no matter what was asked for. A test asserting
// 0600 does not fail on Windows because the file is exposed — it fails
// because the question does not apply.
package testmode

import (
	"os"
	"runtime"
	"testing"
)

// AssertPrivateFile checks that a file is owner-only.
//
// On Windows the check is skipped, loudly, because there is nothing to check:
// access there is governed by the inherited NTFS ACL on %AppData%, which
// grants the user, SYSTEM and Administrators and nobody else. That is at
// least as restrictive as 0600 under a 0755 home directory — but it is
// inherited rather than asserted, and a mode check is the wrong instrument
// for an ACL.
func AssertPrivateFile(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if runtime.GOOS == "windows" {
		t.Logf("mode check skipped: Chmod only toggles the read-only bit on windows, "+
			"so %s reports %o whatever was requested; access is governed by the "+
			"inherited %%AppData%% ACL instead", path, fi.Mode().Perm())
		return
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s has mode %o, want 600 — this file holds credential material", path, perm)
	}
}
