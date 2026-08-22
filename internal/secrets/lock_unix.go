//go:build !windows

package secrets

import "syscall"

// lockFile takes an exclusive advisory lock, blocking until it has one.
//
// The competing writer is another spillway process, not another goroutine:
// `spillway login` stores a token while the daemon is running and holding
// the same file. A keychain arbitrates that itself; a file has to be told.
func lockFile(fd uintptr) error   { return syscall.Flock(int(fd), syscall.LOCK_EX) }
func unlockFile(fd uintptr) error { return syscall.Flock(int(fd), syscall.LOCK_UN) }
