package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/coderage-labs/spillway/internal/testmode"
)

func newFile(t *testing.T) *FileStore {
	t.Helper()
	return NewFileStore(filepath.Join(t.TempDir(), "nested", "secrets.json"))
}

func TestFileStoreRoundTrip(t *testing.T) {
	f := newFile(t)
	want := Secrets{AccessToken: "at", RefreshToken: "rt"}
	if err := f.Set("acct", want); err != nil {
		t.Fatal(err)
	}
	got, err := f.Get("acct")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	// A second account must not disturb the first.
	if err := f.Set("other", Secrets{AccessToken: "x"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.Get("acct"); got != want {
		t.Errorf("writing another account clobbered the first: %+v", got)
	}

	if err := f.Delete("acct"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get("acct"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete, err = %v, want ErrNotFound", err)
	}
	// Deleting what is not there is not an error.
	if err := f.Delete("acct"); err != nil {
		t.Errorf("second delete: %v", err)
	}
	// And the other account survived it all.
	if got, _ := f.Get("other"); got.AccessToken != "x" {
		t.Errorf("delete took the wrong account with it: %+v", got)
	}
}

// The whole justification for this store is that the file is private. If the
// mode is wrong it is strictly worse than refusing to start.
func TestFileStoreIsPrivate(t *testing.T) {
	f := newFile(t)
	if err := f.Set("acct", Secrets{AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	testmode.AssertPrivateFile(t, f.Path)

	if runtime.GOOS == "windows" {
		return
	}
	fi, err := os.Stat(filepath.Dir(f.Path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("containing directory is %o, want no group or other access", perm)
	}
}

func TestFileStoreRawBlobs(t *testing.T) {
	f := newFile(t)
	if _, err := f.GetRaw("ca-key"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing blob: err = %v, want ErrNotFound", err)
	}
	if err := f.SetRaw("ca-key", []byte("pem")); err != nil {
		t.Fatal(err)
	}
	got, err := f.GetRaw("ca-key")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pem" {
		t.Errorf("got %q, want pem", got)
	}
	// Account keys are namespaced, so a blob and an account cannot collide.
	if err := f.Set("ca-key", Secrets{AccessToken: "different"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.GetRaw("ca-key"); string(got) != "pem" {
		t.Errorf("an account named ca-key overwrote the blob: %q", got)
	}
}

func TestFileStoreSurvivesAMissingFile(t *testing.T) {
	f := newFile(t)
	if _, err := f.Get("nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("reading before anything is written: err = %v, want ErrNotFound", err)
	}
}

func TestFileStoreReportsAMangledFile(t *testing.T) {
	f := newFile(t)
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := f.Get("acct")
	if err == nil {
		t.Fatal("a corrupt secrets file read as empty")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("corrupt file reported as 'not found', which reads as "+
			"'log in again' and would overwrite it: %v", err)
	}
}

// Two stores on one path is not hypothetical: the daemon opens the store for
// the pool and again for the MITM CA, and each call made its own instance
// with its own mutex. Load-modify-save from two of them loses one writer.
func TestConcurrentWritersKeepEveryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	a, b := NewFileStore(path), NewFileStore(path)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) { defer wg.Done(); a.Set(fmt.Sprintf("a%d", i), Secrets{AccessToken: "x"}) }(i)
		go func(i int) { defer wg.Done(); b.SetRaw(fmt.Sprintf("b%d", i), []byte("y")) }(i)
	}
	wg.Wait()

	var missing []string
	for i := 0; i < 20; i++ {
		if _, err := a.Get(fmt.Sprintf("a%d", i)); err != nil {
			missing = append(missing, fmt.Sprintf("a%d", i))
		}
		if _, err := a.GetRaw(fmt.Sprintf("b%d", i)); err != nil {
			missing = append(missing, fmt.Sprintf("b%d", i))
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of 40 keys lost to concurrent writes: %v", len(missing), missing)
	}

	// And nothing half-written left lying around holding token material.
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".secrets-*"))
	if len(leftovers) > 0 {
		t.Errorf("temp files left behind with secrets in them: %v", leftovers)
	}
}
