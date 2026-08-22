package main

// Build identity.
//
// A daemon that cannot say what it is makes every later bug report guesswork:
// spillway runs unattended for days under launchd, and "which binary produced
// this log line" has no answer without it.
//
// Values are injected at link time by the release workflow. A plain
// `go build` leaves them empty, so the fallback reads Go's own embedded VCS
// stamp — which covers the case that matters most here, a binary built from a
// local clone.

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

var (
	// version is the release tag, e.g. "v0.2.0". Empty for a local build.
	version string
	// commit and date are set alongside it.
	commit string
	date   string
)

// buildInfo is the one-line identity, e.g.
// "v0.2.0 (60ae732, 2026-08-22, go1.27.0)".
func buildInfo() string {
	v, c, d := version, commit, date
	if v == "" {
		v = "dev"
	}
	// go build stamps VCS state into the binary. Use it when the linker did
	// not, so a locally built binary still identifies itself — including
	// whether the tree was dirty, which is exactly when you need to know.
	if info, ok := debug.ReadBuildInfo(); ok {
		dirty := false
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if c == "" {
					c = s.Value
				}
			case "vcs.time":
				if d == "" {
					d = s.Value
				}
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if dirty {
			v += "-dirty"
		}
	}
	if len(c) > 7 {
		c = c[:7]
	}
	parts := []string{}
	if c != "" {
		parts = append(parts, c)
	}
	if d != "" {
		parts = append(parts, strings.SplitN(d, "T", 2)[0])
	}
	parts = append(parts, runtime.Version())
	return v + " (" + strings.Join(parts, ", ") + ")"
}

func runVersion() error {
	fmt.Println("spillway " + buildInfo())
	return nil
}
