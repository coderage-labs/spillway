package secrets

// Choosing between the keychain and the file.
//
// The rule is narrow on purpose: fall back only when the platform has no
// keychain service running at all. A locked macOS keychain, a cancelled
// Windows prompt, a Secret Service that answers and says no — those are the
// user declining, and silently writing their tokens to a file instead would
// turn a refusal into a downgrade they never agreed to.

import (
	"errors"
	"runtime"

	"github.com/zalando/go-keyring"
)

// Open returns the Store to use, and a reason string when it had to fall back
// to a file — non-empty means "tell the user", since this is a weaker place
// to keep a token than the one they would have got.
//
// fallbackPath is used only if the keychain is unavailable.
func Open(fallbackPath string) (Store, string) {
	k := NewKeyring()
	return chooseStore(runtime.GOOS, probe(k), k, fallbackPath)
}

// chooseStore is the decision on its own, with the two facts it turns on
// passed in. Split out because the interesting cases are a machine with no
// keychain and a macOS keychain that says no, and a test can be run on
// neither — the version before this could only ever exercise the branch the
// test machine happened to be on.
func chooseStore(goos string, probeErr error, k *Keyring, fallbackPath string) (Store, string) {
	if probeErr == nil {
		return k, ""
	}
	// macOS and Windows both ship a keychain with the OS. A failure there is
	// a real fault — locked, or a denied prompt — and answering it by writing
	// plaintext would be answering a question nobody asked.
	if goos == "darwin" || goos == "windows" {
		return k, ""
	}
	return NewFileStore(fallbackPath), probeErr.Error()
}

// probe reports whether the keychain can be reached, without writing
// anything. A "not found" answer is a working keychain: it was asked a
// question and gave one.
func probe(k *Keyring) error {
	_, err := keyring.Get(k.service(), "spillway-availability-probe")
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
