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
