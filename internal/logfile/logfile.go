// Package logfile gives the daemon a log file it owns and can bound.
//
// Issue #171: the daemon's log had grown to 236 MB and nothing on the
// machine was ever going to shrink it. That is not an oversight in the
// service definition — it is structural. launchd (and the Windows Scheduled
// Task, and any `>>` a user types) hands the process an already-open
// descriptor pointing at a fixed path, so the daemon cannot roll that file:
// renaming it out from under the descriptor just means the writer keeps
// filling the renamed inode and the fresh file stays empty forever.
//
// The only writer that can rotate a file is the one that opened it. So the
// daemon opens its own, and the service definitions stop redirecting a
// stream at the same path — see cmd/spillway/service_darwin.go, where the
// two-writer hazard is the thing the plist has to avoid.
//
// Deliberately not a dependency: this is a size cap and a generation cap
// over os.Rename, which is all the daemon needs. Time-based rolling,
// compression and external-tool coordination are the parts of a logging
// library that earn their keep in a fleet, and none of them apply to one
// local process.
package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// File is an append-only log file that rotates itself at a size bound.
//
// Archives are the live path with a generation suffix: spillway.log.1 is the
// most recent, spillway.log.2 the one before it. maxFiles counts the live
// file, so maxFiles=5 means spillway.log plus .1 … .4, and the total on disk
// can never exceed maxFiles * maxBytes.
//
// Safe for concurrent use by many goroutines, which is what slog needs. It
// is NOT safe for two processes to open the same path: one of them would
// rename the file the other is still writing to. Nothing in spillway does
// that — the daemon is single-instance by virtue of binding a port — and a
// second `spillway server` on the same log file exits on the port conflict
// before it has written much.
type File struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	maxFiles int
	f        *os.File
	size     int64
}

// Open opens path for appending, creating it and its directory if needed,
// and bounds it at maxBytes per file across maxFiles generations.
//
// The current size is read from the file on disk rather than counted from
// zero, so a restart does not reset the bound: a daemon that comes back to
// an over-size log rotates it on its first write instead of carrying on
// past the cap until the next restart.
func Open(path string, maxBytes int64, maxFiles int) (*File, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("logfile %s: maxBytes must be positive, got %d", path, maxBytes)
	}
	if maxFiles < 1 {
		return nil, fmt.Errorf("logfile %s: maxFiles must be at least 1, got %d", path, maxFiles)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &File{path: path, maxBytes: maxBytes, maxFiles: maxFiles}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

// open attaches to path, picking up whatever is already there.
func (l *File) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	size := int64(0)
	if fi, serr := f.Stat(); serr == nil {
		size = fi.Size()
	}
	l.f, l.size = f, size
	return nil
}

// Write appends p, rotating first if p would take the file past its bound.
//
// Rotating before the write rather than after it keeps the cap a real
// ceiling and keeps a record whole: slog hands us exactly one line per
// call, and a line split across two generations is a line nobody can grep.
func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return 0, os.ErrClosed
	}
	// size > 0 guards the pathological case: a single record larger than
	// the whole cap would otherwise rotate on every write and empty every
	// generation. Better to let one oversized line exceed the bound.
	if l.size > 0 && l.size+int64(len(p)) > l.maxBytes {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// rotate closes the live file, shifts the generations up and starts a new
// one. Called with l.mu held.
func (l *File) rotate() error {
	if err := l.f.Close(); err != nil {
		return err
	}
	l.f = nil
	if l.maxFiles > 1 {
		// Drop the oldest, then shift each survivor one generation older.
		// Downwards from the top, so nothing overwrites a file that has
		// not moved yet. A missing generation is normal (the daemon has
		// not rotated that many times yet), so ENOENT is not an error.
		_ = os.Remove(l.archive(l.maxFiles - 1))
		for i := l.maxFiles - 2; i >= 1; i-- {
			_ = os.Rename(l.archive(i), l.archive(i+1))
		}
		if err := os.Rename(l.path, l.archive(1)); err != nil {
			// Reattach before giving up: a daemon that cannot rotate must
			// still be a daemon that can log, and the alternative is
			// silence from the moment the rename starts failing.
			if oerr := l.open(); oerr != nil {
				return oerr
			}
			return fmt.Errorf("rotate log file: %w", err)
		}
	} else {
		// One generation means no history is kept: the live file is all
		// there is, so the cap can only be honoured by starting over.
		if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
			if oerr := l.open(); oerr != nil {
				return oerr
			}
			return fmt.Errorf("rotate log file: %w", err)
		}
	}
	return l.open()
}

// archive names generation i (1 is the most recent).
func (l *File) archive(i int) string { return fmt.Sprintf("%s.%d", l.path, i) }

// Close closes the live file. Writes after Close return os.ErrClosed rather
// than panicking: the daemon's fatal-error path writes one last line, and
// racing that against shutdown must not turn an error report into a crash.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
