package secrets

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// Two processes, the real scenario: `spillway login` writing a token while
// the daemon holds the same file. An in-process mutex does nothing here.
func TestCrossProcessWritersKeepEveryKey(t *testing.T) {
	// Windows has no advisory lock here, on purpose: chooseStore never
	// selects the file store there, because the Credential Manager always
	// exists and arbitrates concurrent writers itself. Asserting a guarantee
	// the platform is not given — and does not need — only produces a red
	// build. The in-process test above still runs everywhere.
	if runtime.GOOS == "windows" {
		t.Skip("the file store is never chosen on windows; lockFile is a documented no-op there")
	}
	if os.Getenv("SECRETS_CHILD_PATH") != "" {
		f := NewFileStore(os.Getenv("SECRETS_CHILD_PATH"))
		n, _ := strconv.Atoi(os.Getenv("SECRETS_CHILD_N"))
		for i := 0; i < 25; i++ {
			if err := f.Set(fmt.Sprintf("p%d-%d", n, i), Secrets{AccessToken: "x"}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		os.Exit(0)
	}

	path := filepath.Join(t.TempDir(), "s.json")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var kids []*exec.Cmd
	for n := 0; n < 4; n++ {
		c := exec.Command(exe, "-test.run", "TestCrossProcessWritersKeepEveryKey")
		c.Env = append(os.Environ(),
			"SECRETS_CHILD_PATH="+path, "SECRETS_CHILD_N="+strconv.Itoa(n))
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		kids = append(kids, c)
	}
	for _, c := range kids {
		if err := c.Wait(); err != nil {
			t.Fatalf("child failed: %v", err)
		}
	}

	f := NewFileStore(path)
	var missing int
	for n := 0; n < 4; n++ {
		for i := 0; i < 25; i++ {
			if _, err := f.Get(fmt.Sprintf("p%d-%d", n, i)); err != nil {
				missing++
			}
		}
	}
	if missing > 0 {
		t.Errorf("%d of 100 keys lost across processes", missing)
	}
}
