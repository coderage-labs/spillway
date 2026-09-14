package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// line is one log record of a known, fixed width, so a test can say exactly
// how many fit inside a cap instead of guessing.
func line(n int) string { return fmt.Sprintf("line %04d\n", n) }

const lineBytes = 10 // len(line(0))

// readGenerations returns every line the log still holds, oldest first:
// the highest-numbered archive down to .1, then the live file. Missing
// generations are skipped — that is what "not rotated that many times yet"
// looks like on disk.
func readGenerations(t *testing.T, path string, maxFiles int) []string {
	t.Helper()
	var lines []string
	for i := maxFiles - 1; i >= 1; i-- {
		data, err := os.ReadFile(fmt.Sprintf("%s.%d", path, i))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read generation %d: %v", i, err)
		}
		lines = append(lines, splitLines(string(data))...)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read live log: %v", err)
	}
	return append(lines, splitLines(string(data))...)
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// existingGenerations lists which of path, path.1 … path.<probe> are on disk.
func existingGenerations(t *testing.T, path string, probe int) []string {
	t.Helper()
	var found []string
	if _, err := os.Stat(path); err == nil {
		found = append(found, filepath.Base(path))
	}
	for i := 1; i <= probe; i++ {
		p := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(p); err == nil {
			found = append(found, filepath.Base(p))
		}
	}
	return found
}

// The bound is a real ceiling: the live file never exceeds maxBytes, and the
// overflow lands in an archive rather than being dropped or appended anyway.
func TestRotatesAtTheBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.log")
	// Exactly 5 lines fit; the 6th must rotate.
	l, err := Open(path, 5*lineBytes, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	for i := 1; i <= 5; i++ {
		if _, err := l.Write([]byte(line(i))); err != nil {
			t.Fatal(err)
		}
	}
	if got := existingGenerations(t, path, 3); len(got) != 1 {
		t.Fatalf("generations on disk = %v; nothing may rotate before the bound is reached", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 5*lineBytes {
		t.Fatalf("live log = %d bytes; want the 5 lines written (%d)", fi.Size(), 5*lineBytes)
	}

	if _, err := l.Write([]byte(line(6))); err != nil {
		t.Fatal(err)
	}
	if fi, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if fi.Size() != lineBytes {
		t.Fatalf("live log = %d bytes after the rotating write; want just the new line (%d)",
			fi.Size(), lineBytes)
	}
	arch, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("the line that overflowed the bound did not produce an archive: %v", err)
	}
	if len(arch) != 5*lineBytes {
		t.Fatalf("archive = %d bytes; want the 5 lines that were live before the rotation (%d)",
			len(arch), 5*lineBytes)
	}
	if got := string(arch); !strings.HasPrefix(got, line(1)) || !strings.HasSuffix(got, line(5)) {
		t.Fatalf("archive holds %q; want lines 1..5", got)
	}
}

// The generation cap is what bounds total disk use. maxFiles counts the live
// file, so maxFiles=3 means the live log plus .1 and .2 — .3 must never
// exist, however many times the daemon rotates.
func TestGenerationCapIsEnforced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.log")
	const maxFiles = 3
	l, err := Open(path, 2*lineBytes, maxFiles)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// 40 lines at 2 per file is ~20 rotations: far more than the cap.
	for i := 1; i <= 40; i++ {
		if _, err := l.Write([]byte(line(i))); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"spillway.log", "spillway.log.1", "spillway.log.2"}
	got := existingGenerations(t, path, 6)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("generations on disk = %v; want exactly %v", got, want)
	}

	var total int64
	for _, name := range got {
		fi, err := os.Stat(filepath.Join(filepath.Dir(path), name))
		if err != nil {
			t.Fatal(err)
		}
		total += fi.Size()
	}
	if cap := int64(maxFiles * 2 * lineBytes); total > cap {
		t.Fatalf("total on disk = %d bytes; the cap is maxFiles*maxBytes = %d", total, cap)
	}

	// And the cost of the cap, stated explicitly: the oldest lines are gone,
	// the newest are kept.
	lines := readGenerations(t, path, maxFiles)
	if strings.TrimSpace(lines[len(lines)-1]) != strings.TrimSpace(line(40)) {
		t.Fatalf("last line kept = %q; want the most recent line written", lines[len(lines)-1])
	}
	for _, l := range lines {
		if strings.TrimSpace(l) == strings.TrimSpace(line(1)) {
			t.Fatal("line 1 survived 20 rotations with a 3-generation cap; the cap discards the oldest")
		}
	}
}

// Nothing is lost *across* a rotation: every line written while the log
// still has room for it is readable afterwards, exactly once, in order.
// Sized so the whole run fits inside the generations kept, which is the
// case where "the cap discarded it" is not an available excuse.
func TestNothingIsLostAcrossRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.log")
	const maxFiles = 5
	l, err := Open(path, 4*lineBytes, maxFiles)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const n = 20 // 5 files' worth: 4 rotations, nothing evicted
	for i := 1; i <= n; i++ {
		if _, err := l.Write([]byte(line(i))); err != nil {
			t.Fatal(err)
		}
	}

	lines := readGenerations(t, path, maxFiles)
	if len(lines) != n {
		t.Fatalf("read back %d lines across the generations; wrote %d:\n%s",
			len(lines), n, strings.Join(lines, "\n"))
	}
	for i, got := range lines {
		if want := strings.TrimSuffix(line(i+1), "\n"); got != want {
			t.Fatalf("line %d = %q; want %q — rotation reordered or duplicated records", i, got, want)
		}
	}
}

// The bound survives a restart. launchd restarts this daemon whenever it
// dies, and a writer that counts from zero on each start would sail past the
// cap forever on a machine that reboots daily: the size has to come from the
// file on disk, not from this process's own tally.
func TestBoundSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.log")
	l, err := Open(path, 5*lineBytes, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if _, err := l.Write([]byte(line(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// The daemon comes back to a full file.
	l2, err := Open(path, 5*lineBytes, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if _, err := l2.Write([]byte(line(6))); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != lineBytes {
		t.Fatalf("live log = %d bytes after restarting onto a full file; want a rotation to have "+
			"left only the new line (%d)", fi.Size(), lineBytes)
	}
	arch, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("restart did not rotate the full file it inherited: %v", err)
	}
	if len(arch) != 5*lineBytes {
		t.Fatalf("archive = %d bytes; want the 5 pre-restart lines (%d)", len(arch), 5*lineBytes)
	}
}

// A record bigger than the whole cap is written whole rather than triggering
// a rotation per write that would empty every generation in turn. One line
// over the bound beats losing the history either side of it.
func TestOversizedRecordDoesNotEmptyEveryGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.log")
	l, err := Open(path, 4*lineBytes, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	big := strings.Repeat("x", 10*lineBytes) + "\n"
	if _, err := l.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	if got := existingGenerations(t, path, 3); len(got) != 1 {
		t.Fatalf("generations = %v; an oversized first record must not rotate an empty file", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != big {
		t.Fatalf("live log = %d bytes; the oversized record must be written whole (%d)", len(data), len(big))
	}
}

// slog writes from every request goroutine at once. Records must not
// interleave or be lost, and the rotation in the middle of that must not
// tear one.
func TestConcurrentWritesKeepRecordsWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.log")
	const maxFiles = 8
	l, err := Open(path, 10*lineBytes, maxFiles)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const writers, each = 8, 8
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := l.Write([]byte(line(w*each + i))); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	lines := readGenerations(t, path, maxFiles)
	if len(lines) != writers*each {
		t.Fatalf("read back %d records; wrote %d", len(lines), writers*each)
	}
	seen := map[string]bool{}
	for _, got := range lines {
		if len(got) != lineBytes-1 {
			t.Fatalf("torn record %q (%d bytes); every record is one whole line", got, len(got))
		}
		if _, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(got, "line"))); err != nil {
			t.Fatalf("record %q is not a whole record", got)
		}
		if seen[got] {
			t.Fatalf("record %q written once, read back twice", got)
		}
		seen[got] = true
	}
}

// Limits that cannot bound anything are refused at Open rather than
// silently treated as "no rotation" — the failure mode this issue is about.
func TestOpenRejectsLimitsThatBoundNothing(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name     string
		maxBytes int64
		maxFiles int
	}{
		{"zero size", 0, 3},
		{"negative size", -1, 3},
		{"zero generations", 1024, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Open(filepath.Join(dir, tc.name+".log"), tc.maxBytes, tc.maxFiles); err == nil {
				t.Fatal("Open accepted limits that cannot bound the file")
			}
		})
	}
}

// maxFiles=1 keeps no history, so the only way to honour the cap is to start
// the file over. It must do that rather than grow unbounded.
func TestSingleGenerationStartsOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.log")
	l, err := Open(path, 2*lineBytes, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	for i := 1; i <= 5; i++ {
		if _, err := l.Write([]byte(line(i))); err != nil {
			t.Fatal(err)
		}
	}
	if got := existingGenerations(t, path, 3); len(got) != 1 {
		t.Fatalf("generations = %v; maxFiles=1 keeps no archives", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 2*lineBytes {
		t.Fatalf("live log = %d bytes; the cap is %d", fi.Size(), 2*lineBytes)
	}
}
