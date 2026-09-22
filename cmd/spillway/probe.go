package main

// `spillway probe <account> [--force]` — issue #192: ask the daemon to
// re-measure one account's quota right now.
//
// The dashboard has the same control, but the dashboard is not always where
// someone is: the person who has just bought a reset is usually in the
// terminal the reset was for. Through the admin API for the same reason
// `spillway switch` is — this is a live instruction to a running daemon, not
// a setting, and it survives nothing.
//
// --force is the acknowledgement that the probe will be charged. It is
// needed only on an account whose quota is spent AND whose extra usage is
// permitted, which the daemon answers with a 409; everywhere else the probe
// is free and the flag does nothing. Same shape as `spillway switch
// --force` (#139), deliberately: one vocabulary for "this costs money, say
// it again if you mean it".

import (
	"fmt"
	"os"
	"strings"
)

// probeArgs is the parsed command line. Split out for the same reason
// switchArgs is: runProbe dials the daemon before it can reject anything, so
// a test driving it only ever sees a connection failure, and the parsing
// would be untested.
type probeArgs struct {
	name  string
	force bool
}

func parseProbeArgs(args []string) (probeArgs, error) {
	var out probeArgs
	for _, a := range args {
		switch {
		case a == "--force" || a == "-f":
			out.force = true
		case strings.HasPrefix(a, "-"):
			return out, fmt.Errorf("unknown flag %q", a)
		default:
			if out.name != "" {
				return out, fmt.Errorf("probe takes one account, got %q and %q", out.name, a)
			}
			out.name = a
		}
	}
	if out.name == "" {
		return out, fmt.Errorf("usage: spillway probe <account> [--force]")
	}
	return out, nil
}

// probeWindow mirrors pool.QuotaWindow's JSON, narrowed to what the report
// prints. Declared here rather than importing internal/pool so the CLI keeps
// reading the endpoint's wire shape, as every other command here does.
type probeWindow struct {
	Name    string  `json:"name"`
	Limit   float64 `json:"limit"`
	Used    float64 `json:"used"`
	Expired bool    `json:"expired"`
}

func runProbe(argv []string) error {
	args, err := parseProbeArgs(argv)
	if err != nil {
		return err
	}

	api, err := dialAdmin()
	if err != nil {
		return err
	}
	// Resolved at the edge, exactly as `switch` does it: the daemon matches
	// the name exactly, so a label or a unique prefix has to become a real
	// account name before the POST, and an ambiguous one must be refused
	// here rather than probed by guess.
	accounts, err := fetchSwitchAccounts(api)
	if err != nil {
		return err
	}
	resolved, err := resolveAccountName(accounts, args.name)
	if err != nil {
		return err
	}

	var out struct {
		Account string        `json:"account"`
		Billed  bool          `json:"billed"`
		Windows []probeWindow `json:"quotaWindows"`
	}
	req := map[string]any{"name": resolved, "force": args.force}
	if err := api.postJSON("/api/accounts/probe", req, &out); err != nil {
		// 409 is the money refusal, and the only one --force answers. After
		// the error, not before it: main prints the error itself, so a hint
		// written here would otherwise sit above the thing it advises on.
		if strings.Contains(err.Error(), "409") && !args.force {
			return &hintedError{err: err,
				hint: "re-run with --force to buy this probe, or wait for the window to reset"}
		}
		return err
	}

	fmt.Printf("probed %s\n", out.Account)
	if out.Billed {
		fmt.Println("  this probe was charged: the account was out of quota with extra usage permitted")
	}
	if len(out.Windows) == 0 {
		fmt.Println("  the probe returned no quota windows")
		return nil
	}
	t := newTable("window", "used")
	for _, w := range out.Windows {
		t.add(w.Name, describeProbeWindow(w))
	}
	t.render(os.Stdout)
	return nil
}

// describeProbeWindow renders one window's level. A window with no limit
// reported has no percentage to show — headroom is undefined, not 100% — and
// one whose own reset has passed with nothing re-measuring it is stale rather
// than current (issue #135), so neither is printed as a bare number.
func describeProbeWindow(w probeWindow) string {
	if w.Expired {
		return "expired (no reading since its reset)"
	}
	if w.Limit <= 0 {
		return "no limit reported"
	}
	return fmt.Sprintf("%.0f%%", 100*w.Used/w.Limit)
}
