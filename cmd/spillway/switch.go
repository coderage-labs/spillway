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
	"os"
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
	// Bare `spillway switch`: not a usage error (#23). There is no target to
	// parse, so this is handled before parseSwitchArgs ever sees it — that
	// function's own contract (no name and not auto is an error) still holds
	// for every caller that actually reaches it.
	if len(argv) == 0 {
		return runSwitchReport()
	}

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

	// Resolve here, at the edge — internal/pool's Pin still matches `name`
	// exactly. What the daemon gets from here on is a real account name.
	accounts, err := fetchSwitchAccounts(api)
	if err != nil {
		return err
	}
	resolved, err := resolveAccountName(accounts, name)
	if err != nil {
		return err
	}

	body, err := json.Marshal(map[string]any{"account": resolved, "force": force})
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

// fetchSwitchAccounts reads /api/accounts and keeps only the fields the
// resolver and the bare report need. The fetch stays separate from
// resolveAccountName so the matching logic can be tested as a pure function
// over a slice, with no daemon involved.
func fetchSwitchAccounts(api *adminAPI) ([]switchAccount, error) {
	var raw []struct {
		Name          string `json:"name"`
		Label         string `json:"label"`
		State         string `json:"state"`
		Paid          bool   `json:"paid"`
		OverThreshold bool   `json:"overThreshold"`
	}
	if err := api.get("/api/accounts", &raw); err != nil {
		return nil, err
	}
	out := make([]switchAccount, len(raw))
	for i, r := range raw {
		out[i] = switchAccount{
			Name:          r.Name,
			Label:         r.Label,
			State:         r.State,
			Paid:          r.Paid,
			OverThreshold: r.OverThreshold,
		}
	}
	return out, nil
}

// runSwitchReport implements bare `spillway switch` (#23): what is pinned,
// or that selection is automatic, and what you could switch to. Not
// `spillway status` — no headroom, windows or traffic, just enough to decide
// what to type next. Anything that would refuse a pin or cost money is
// marked so a reader does not have to attempt the switch to find out.
func runSwitchReport() error {
	api, err := dialAdmin()
	if err != nil {
		return err
	}
	var st struct {
		Pinned string `json:"pinned"`
	}
	if err := api.get("/api/state", &st); err != nil {
		return err
	}
	accounts, err := fetchSwitchAccounts(api)
	if err != nil {
		return err
	}

	if st.Pinned == "" {
		fmt.Println("selection: automatic")
	} else {
		pinned := st.Pinned
		for _, a := range accounts {
			if a.Name == st.Pinned {
				pinned = describeAccount(a)
				break
			}
		}
		fmt.Printf("pinned to %s\n", pinned)
	}

	if len(accounts) == 0 {
		fmt.Println("no accounts configured")
		return nil
	}

	fmt.Println()
	fmt.Println("switch to:")
	t := newTable("account", "note")
	for _, a := range accounts {
		t.add(describeAccount(a), switchNote(a, st.Pinned))
	}
	t.render(os.Stdout)
	return nil
}

// switchNote marks the two things #23 asks a reader not to have to discover
// by attempting the switch: an account that would serve from paid extra
// usage, and one that a pin would be refused on outright (parked, disabled)
// or that is already spent (overThreshold).
func switchNote(a switchAccount, pinned string) string {
	switch {
	case a.Name == pinned:
		return "pinned"
	case a.State == "parked":
		return "parked"
	case a.State == "disabled":
		return "disabled"
	case a.Paid:
		return "would spend money (paid extra usage)"
	case a.OverThreshold:
		return "spent"
	default:
		return ""
	}
}

// hintedError carries a next step alongside the failure, so the advice prints
// after the error rather than before it.
type hintedError struct {
	err  error
	hint string
}

func (e *hintedError) Error() string { return e.err.Error() + "\n  " + e.hint }
func (e *hintedError) Unwrap() error { return e.err }
