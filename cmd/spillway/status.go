package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// tokenPathFor locates the admin token beside the config. Absent on a
// loopback listener, which needs none.
func tokenPathFor(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "spillway-admin-token")
}

// runStatus implements `spillway status`: query the running daemon's admin
// API and print a compact pool summary. With --json it prints the pool view
// the dashboard sees instead, for anything that wants to read it rather than
// look at it — the spillway Claude Code plugin, mainly, which would otherwise
// have to hardcode the port and could not present an admin token.
func runStatus(jsonOut bool) error {
	api, err := dialAdmin()
	if err != nil {
		return err
	}
	if jsonOut {
		return statusJSON(api)
	}
	var accounts []struct {
		Name           string `json:"name"`
		Type           string `json:"type"`
		State          string `json:"state"`
		InFlight       int    `json:"inFlight"`
		ExhaustedUntil string `json:"exhaustedUntil"`
		Windows        []struct {
			Name    string    `json:"name"`
			Limit   float64   `json:"limit"`
			Used    float64   `json:"used"`
			ResetAt time.Time `json:"resetAt"`
			Expired bool      `json:"expired"` // issue #135: past its reset, unknown not spent
		} `json:"quotaWindows"`
	}
	if err := api.get("/api/accounts", &accounts); err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Println("no accounts")
		return nil
	}
	// One column per quota window, in the order they are first seen. Accounts
	// can report different windows (5h/7d on Claude, one on Kimi), so the
	// columns are the union rather than the first account's set.
	var wins []string
	seen := map[string]bool{}
	for _, a := range accounts {
		for _, w := range a.Windows {
			if w.Limit > 0 && !seen[w.Name] {
				seen[w.Name] = true
				wins = append(wins, w.Name)
			}
		}
	}

	head := []string{"account", "type", "state", "in flight"}
	for _, w := range wins {
		// "left", not "used". This printed the used fraction while the status
		// line and the dashboard both show what remains — three surfaces, two
		// meanings, no label saying which.
		head = append(head, w+" left")
	}
	t := newTable(head...).rightAlign(3)
	for i := range wins {
		t.rightAlign(4 + i)
	}

	for _, a := range accounts {
		row := []string{a.Name, a.Type, a.State, strconv.Itoa(a.InFlight)}
		by := map[string]string{}
		for _, w := range a.Windows {
			if w.Limit <= 0 {
				continue
			}
			if w.Expired {
				by[w.Name] = "expired"
				continue
			}
			cell := fmt.Sprintf("%d%%", int((1-w.Used/w.Limit)*100+0.5))
			if !w.ResetAt.IsZero() {
				cell += " · " + compactDur(time.Until(w.ResetAt))
			}
			by[w.Name] = cell
		}
		for _, w := range wins {
			c := by[w]
			if c == "" {
				c = "—"
			}
			row = append(row, c)
		}
		t.add(row...)
	}
	t.render(os.Stdout)
	return nil
}

// statusJSON prints state, accounts and recent requests as one object. It
// stays raw rather than summarising: the caller is a model, and pre-digesting
// this into prose here would only mean two places deciding what matters.
func statusJSON(api *adminAPI) error {
	out := map[string]any{}
	for _, e := range []struct {
		key, path string
	}{
		{"state", "/api/state"},
		{"accounts", "/api/accounts"},
		{"requests", "/api/requests?limit=20"},
	} {
		var v any
		if err := api.get(e.path, &v); err != nil {
			return err
		}
		out[e.key] = v
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
