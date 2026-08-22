//go:build windows

package secrets

// Windows never reaches the file store — chooseStore keeps it on the
// Credential Manager, which arbitrates concurrent writers itself. These exist
// so the package builds there, not because they are a locking strategy.
func lockFile(fd uintptr) error   { return nil }
func unlockFile(fd uintptr) error { return nil }
