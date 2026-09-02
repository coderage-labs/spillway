package secrets

import (
	"fmt"
	"sync"
)

// Fake is an in-memory Store for tests.
type Fake struct {
	mu   sync.Mutex
	data map[string]Secrets
	raw  map[string][]byte

	// rawErr, when non-nil, is returned by GetRaw instead of the normal
	// lookup — for tests simulating a locked/denied/transient keychain,
	// which callers must treat differently from a genuine ErrNotFound
	// (issue #65: conflating the two silently regenerates the MITM CA
	// over a working one).
	rawErr error
}

// SetGetRawErr makes every subsequent GetRaw call return err instead of
// consulting the map. Pass nil to go back to normal lookups. err must not
// be (and must not wrap) ErrNotFound — that has a distinct meaning
// ("genuinely absent") that real Store implementations only return when
// the entry truly isn't there; use Delete/plain absence for that case.
func (f *Fake) SetGetRawErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rawErr = err
}

// NewFake returns an empty in-memory Store.
func NewFake() *Fake { return &Fake{data: map[string]Secrets{}, raw: map[string][]byte{}} }

// Get implements Store.
func (f *Fake) Get(name string) (Secrets, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[name]
	if !ok {
		return Secrets{}, fmt.Errorf("%w for account %q (run `spillway login claude %s`)", ErrNotFound, name, name)
	}
	return s, nil
}

// Set implements Store.
func (f *Fake) Set(name string, s Secrets) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[name] = s
	return nil
}

// Delete implements Store.
func (f *Fake) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, name)
	return nil
}

// GetRaw implements Store.
func (f *Fake) GetRaw(name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rawErr != nil {
		return nil, f.rawErr
	}
	v, ok := f.raw[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return v, nil
}

// SetRaw implements Store.
func (f *Fake) SetRaw(name string, v []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raw[name] = v
	return nil
}

// DeleteRaw implements Store.
func (f *Fake) DeleteRaw(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.raw, name)
	return nil
}
