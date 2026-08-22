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

func runSwitch(args []string) error {
	var (
		name  string
		force bool
		auto  bool
	)
	for _, a := range args {
		switch {
		case a == "--force" || a == "-f":
			force = true
		case a == "--auto" || a == "off" || a == "none":
			auto = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %q", a)
		default:
			name = a
		}
	}
	if name == "" && !auto {
		return fmt.Errorf("usage: spillway switch <account>|--auto [--force]")
	}

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
		if strings.Contains(err.Error(), "409") {
			fmt.Fprintf(os.Stderr, "  re-run with --force if you mean it\n")
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
