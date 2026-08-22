// Package secrets stores account token material in the OS keychain (design
// doc §5: config holds metadata only, secrets never touch disk). Lives in its
// own package so config (import) and accounts (refresh write-back) can both
// use it without an import cycle.
package secrets

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// Service is the keychain service name spillway stores under.
const Service = "spillway"

// Secrets is one account's token material.
type Secrets struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// ErrNotFound marks a missing keychain entry.
var ErrNotFound = errors.New("no credentials in keychain")

// Store keeps per-account secrets. Implemented by Keyring; faked in tests.
type Store interface {
	Get(name string) (Secrets, error)
	Set(name string, s Secrets) error
	// Delete removes an account's secrets; missing is not an error.
	Delete(name string) error
	// GetRaw/SetRaw store non-account blobs (e.g. the MITM CA private key).
	GetRaw(name string) ([]byte, error)
	SetRaw(name string, v []byte) error
}

// Keyring is the OS-keychain Store. Key format: "account:<name>".
type Keyring struct {
	Service string // empty → Service
}

// NewKeyring returns a Store backed by the OS keychain under spillway's
// service name (never the claude CLI's item — that one is read-only to us).
func NewKeyring() *Keyring { return &Keyring{} }

func (k *Keyring) service() string {
	if k.Service != "" {
		return k.Service
	}
	return Service
}

func key(name string) string { return "account:" + name }

// Get retrieves an account's secrets.
func (k *Keyring) Get(name string) (Secrets, error) {
	v, err := keyring.Get(k.service(), key(name))
	if errors.Is(err, keyring.ErrNotFound) {
		return Secrets{}, fmt.Errorf("%w for account %q (run `spillway login claude %s`)", ErrNotFound, name, name)
	}
	if err != nil {
		return Secrets{}, fmt.Errorf("keychain read for account %q: %w", name, err)
	}
	var s Secrets
	if err := json.Unmarshal([]byte(v), &s); err != nil {
		return Secrets{}, fmt.Errorf("keychain entry for account %q malformed: %w", name, err)
	}
	return s, nil
}

// Set stores an account's secrets.
func (k *Keyring) Set(name string, s Secrets) error {
	v, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := keyring.Set(k.service(), key(name), string(v)); err != nil {
		return fmt.Errorf("keychain write for account %q: %w", name, err)
	}
	return nil
}

// Delete removes an account's secrets; a missing entry is not an error.
func (k *Keyring) Delete(name string) error {
	if err := keyring.Delete(k.service(), key(name)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("keychain delete for account %q: %w", name, err)
	}
	return nil
}

// GetRaw retrieves a non-account blob.
func (k *Keyring) GetRaw(name string) ([]byte, error) {
	v, err := keyring.Get(k.service(), name)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("keychain read %q: %w", name, err)
	}
	return []byte(v), nil
}

// SetRaw stores a non-account blob.
func (k *Keyring) SetRaw(name string, v []byte) error {
	if err := keyring.Set(k.service(), name, string(v)); err != nil {
		return fmt.Errorf("keychain write %q: %w", name, err)
	}
	return nil
}
