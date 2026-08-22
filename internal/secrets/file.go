package secrets

// A file-backed Store, used only where the OS has no keychain to use.
//
// This is a deliberate weakening of §5 — "secrets never touch disk" — scoped
// to the one case where the alternative is not working at all. On Linux the
// keychain is Secret Service over D-Bus, which exists on a desktop and does
// not exist on a server, in a container, or over SSH. spillway exited 1 on
// every one of those, holding credentials it had been given and refusing to
// use them.
//
// The file is 0600 in a 0700 directory, which is exactly how the Claude CLI
// stores the same class of token on Linux (~/.claude/.credentials.json). So
// this adds no exposure that the machine did not already have — but it is
// still plaintext, it is still worse than a keychain, and it is chosen only
// when there is no keychain to choose.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// FileStore keeps secrets in one JSON file, 0600.
type FileStore struct {
	Path string
}

// NewFileStore returns a Store writing to path.
func NewFileStore(path string) *FileStore { return &FileStore{Path: path} }

// pathLocks gives one mutex per file, not per FileStore.
//
// The daemon opens the store once for the account pool and again for the MITM
// CA key, so a per-instance mutex guarded nothing: two instances read the same
// map, each added its own key, and whichever saved last erased the other. It
// lost half of forty keys in a test, and in the wild it would have been the CA
// key or a login, silently, on a machine where re-running login is the only
// way to notice.
var pathLocks sync.Map

func (f *FileStore) lock() func() {
	v, _ := pathLocks.LoadOrStore(f.Path, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()

	// And an advisory file lock for the other process: `spillway login`
	// writing while the daemon holds the same file.
	lf, err := os.OpenFile(f.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return mu.Unlock
	}
	if err := lockFile(lf.Fd()); err != nil {
		lf.Close()
		return mu.Unlock
	}
	return func() {
		unlockFile(lf.Fd())
		lf.Close()
		mu.Unlock()
	}
}

func (f *FileStore) load() (map[string]string, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read secrets file: %w", err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("secrets file %s malformed: %w", f.Path, err)
	}
	return m, nil
}

// save writes atomically. A torn write here costs every credential on the
// machine, and the recovery is re-running every login.
func (f *FileStore) save(m map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), ".secrets-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	// Before the content, not after: a rename beats a chmod, and a
	// world-readable window is the whole thing this file has to avoid.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.Path)
}

func (f *FileStore) getRaw(k string) ([]byte, bool, error) {
	defer f.lock()()
	m, err := f.load()
	if err != nil {
		return nil, false, err
	}
	v, ok := m[k]
	return []byte(v), ok, nil
}

func (f *FileStore) setRaw(k string, v []byte) error {
	defer f.lock()()
	m, err := f.load()
	if err != nil {
		return err
	}
	m[k] = string(v)
	return f.save(m)
}

// Get retrieves an account's secrets.
func (f *FileStore) Get(name string) (Secrets, error) {
	v, ok, err := f.getRaw(key(name))
	if err != nil {
		return Secrets{}, err
	}
	if !ok {
		return Secrets{}, fmt.Errorf("%w for account %q (run `spillway login claude %s`)", ErrNotFound, name, name)
	}
	var s Secrets
	if err := json.Unmarshal(v, &s); err != nil {
		return Secrets{}, fmt.Errorf("secrets entry for account %q malformed: %w", name, err)
	}
	return s, nil
}

// Set stores an account's secrets.
func (f *FileStore) Set(name string, s Secrets) error {
	v, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return f.setRaw(key(name), v)
}

// Delete removes an account's secrets; a missing entry is not an error.
func (f *FileStore) Delete(name string) error {
	defer f.lock()()
	m, err := f.load()
	if err != nil {
		return err
	}
	if _, ok := m[key(name)]; !ok {
		return nil
	}
	delete(m, key(name))
	return f.save(m)
}

// GetRaw retrieves a non-account blob.
func (f *FileStore) GetRaw(name string) ([]byte, error) {
	v, ok, err := f.getRaw(name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return v, nil
}

// SetRaw stores a non-account blob.
func (f *FileStore) SetRaw(name string, v []byte) error { return f.setRaw(name, v) }
