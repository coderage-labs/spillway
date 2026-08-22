package main

// `spillway switch` — point the pool at one account, or let it choose again
// (#11).
//
// Through the admin API rather than the config, because this is a live
// instruction and not a setting: it applies to the running daemon and does
// not survive a restart. Priority is the setting; this is the override.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// switchArgs is the parsed command line. Split out because the interesting
// behaviour is here and it is unreachable through runSwitch in a test: that
// dials the daemon before it can reject anything, so a test asserting on the
// error only ever sees a connection failure. The first version of this test
// passed with the parsing broken.
type switchArgs struct {
	name  string
	force bool
	auto  bool
}

func parseSwitchArgs(args []string) (switchArgs, error) {
	var out switchArgs
	for _, a := range args {
		switch {
		case a == "--force" || a == "-f":
			out.force = true
		// Bare "auto" too. Somebody typing the word rather than the flag got
		// `no account named "auto"`, which is a confusing way to be told
		// about a flag.
		case a == "--auto" || a == "auto" || a == "off" || a == "none":
			out.auto = true
		case strings.HasPrefix(a, "-"):
			return out, fmt.Errorf("unknown flag %q", a)
		default:
			out.name = a
		}
	}
	if out.name == "" && !out.auto {
		return out, fmt.Errorf("usage: spillway switch <account>|--auto [--force]")
	}
	return out, nil
}

func runSwitch(argv []string) error {
	args, err := parseSwitchArgs(argv)
	if err != nil {
		return err
	}
	name, force, auto := args.name, args.force, args.auto

	api, err := dialAdmin()
	if err != nil {
		return err
	}
	if auto {
		if err := api.do(http.MethodDelete, "/api/pin", nil, nil); err != nil {
			return err
		}
		fmt.Println("switched back to automatic selection")
		return nil
	}

	body, err := json.Marshal(map[string]any{"account": name, "force": force})
	if err != nil {
		return err
	}
	var out struct {
		Pinned  string `json:"pinned"`
		Warning string `json:"warning"`
	}
	if err := api.do(http.MethodPost, "/api/pin", bytes.NewReader(body), &out); err != nil {
		// The two refusals force overrides say so, rather than leaving the
		// reader to guess which kind of no this was.
		// After the error, not before: main prints the error itself, so
		// writing the hint here put the advice above the thing it advises on.
		if strings.Contains(err.Error(), "409") {
			return &hintedError{err: err, hint: "re-run with --force if you mean it"}
		}
		return err
	}
	fmt.Printf("pinned to %s\n", out.Pinned)
	if out.Warning != "" {
		fmt.Printf("  note: %s\n", out.Warning)
	}
	fmt.Println("  `spillway switch --auto` returns to normal selection; a daemon restart also clears it")
	return nil
}

// hintedError carries a next step alongside the failure, so the advice prints
// after the error rather than before it.
type hintedError struct {
	err  error
	hint string
}

func (e *hintedError) Error() string { return e.err.Error() + "\n  " + e.hint }
func (e *hintedError) Unwrap() error { return e.err }
