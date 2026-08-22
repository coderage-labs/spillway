package secrets

import (
	"errors"
	"path/filepath"
	"runtime"
	"testing"
)

// On macOS and Windows the keychain ships with the OS, so a failure is the
// user declining — a locked keychain, a dismissed prompt — not an absence.
// Writing plaintext in response would answer a question nobody asked, and
// would do it silently.
func TestNeverFallsBackWhereAKeychainIsGuaranteed(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skipf("%s has no guaranteed keychain", runtime.GOOS)
	}
	store, why := Open(filepath.Join(t.TempDir(), "s.json"))
	if _, ok := store.(*FileStore); ok {
		t.Fatalf("fell back to a file on %s (reason given: %q)", runtime.GOOS, why)
	}
	if why != "" {
		t.Errorf("reported a fallback reason without falling back: %q", why)
	}
}

// Wherever it does fall back, it must say so: the store is weaker than the
// one the user would otherwise have had, and finding that out later from a
// file listing is not good enough.
func TestFallbackIsAnnounced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, why := Open(path)
	if fs, ok := store.(*FileStore); ok {
		if why == "" {
			t.Error("fell back to a file silently")
		}
		if fs.Path != path {
			t.Errorf("file store at %q, want %q", fs.Path, path)
		}
	} else if why != "" {
		t.Errorf("kept the keychain but reported %q", why)
	}
}

// Open must always return something usable — never nil, whatever it decides.
func TestOpenAlwaysReturnsAStore(t *testing.T) {
	store, _ := Open(filepath.Join(t.TempDir(), "s.json"))
	if store == nil {
		t.Fatal("Open returned no store")
	}
	// And it must satisfy the interface every caller uses.
	var _ Store = store
}

// The decision itself, on every combination — including the two that the
// machine running the test cannot produce.
func TestChooseStore(t *testing.T) {
	broken := errors.New("org.freedesktop.secrets was not provided by any .service files")
	k := NewKeyring()

	for _, tc := range []struct {
		name     string
		goos     string
		probeErr error
		wantFile bool
		wantWhy  bool
	}{
		{"linux with a keyring", "linux", nil, false, false},
		{"linux with none", "linux", broken, true, true},
		{"macOS keychain locked or refused", "darwin", broken, false, false},
		{"windows credential manager refused", "windows", broken, false, false},
		{"macOS working", "darwin", nil, false, false},
		{"freebsd with none", "freebsd", broken, true, true},
	} {
		store, why := chooseStore(tc.goos, tc.probeErr, k, "/tmp/s.json")
		_, isFile := store.(*FileStore)
		if isFile != tc.wantFile {
			t.Errorf("%s: file store = %v, want %v", tc.name, isFile, tc.wantFile)
		}
		if (why != "") != tc.wantWhy {
			t.Errorf("%s: reason %q, want reported = %v", tc.name, why, tc.wantWhy)
		}
	}
}
