//go:build windows

package secrets

// Windows never reaches the file store — chooseStore keeps it on the
// Credential Manager, which arbitrates concurrent writers itself. These exist
// so the package builds there, not because they are a locking strategy.
//
// Stated plainly, because the signatures suggest otherwise: a FileStore
// constructed by hand on Windows is safe within one process, via the per-path
// mutex, and is NOT safe between processes. Nothing in spillway does that.
func lockFile(fd uintptr) error   { return nil }
func unlockFile(fd uintptr) error { return nil }
