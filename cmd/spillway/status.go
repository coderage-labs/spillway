package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	return statusTable(api, os.Stdout)
}

// statusTable renders the human view. Split out from runStatus, and taking
// the sink, so a test can drive it against a stub admin server: dialAdmin
// resolves the REAL config and the real daemon's listener, which no test may
// go near.
func statusTable(api *adminAPI, out io.Writer) error {
	var accounts []struct {
		Name           string `json:"name"`
		Type           string `json:"type"`
		State          string `json:"state"`
		InFlight       int    `json:"inFlight"`
		ExhaustedUntil string `json:"exhaustedUntil"`
		// OverageRefused/OverageReason drive the notes under the table
		// (issue #151). Refused is the bit spillway is acting on; Reason is
		// the provider's own words for why, and the whole answer to "why is
		// my extra usage not working" — it used to live only in
		// /api/accounts, where nothing but the dashboard looks.
		OverageRefused   bool      `json:"overageRefused"`
		OverageReason    string    `json:"overageReason"`
		OverageCheckedAt time.Time `json:"overageCheckedAt"`
		Windows          []struct {
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
		fmt.Fprintln(out, "no accounts")
		return nil
	}

	// One fetch of /api/state for both halves of this render — the pin
	// banner above the table and the unpooled note below it. Two fetches
	// would let one command report two different moments.
	//
	// A missing or failing /api/state is not fatal and adds nothing: an
	// older daemon has no such section, and the accounts table below is
	// still the answer to what was asked. statusline.go takes the same line.
	var st statusState
	_ = api.get("/api/state", &st)

	printPin(st.Pinned, out)
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
			cell := fmt.Sprintf("%d%%", headroomPct(w.Used, w.Limit))
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
	t.render(out)

	// Extra-usage refusals go below the table rather than in a column: they
	// are per-account but rare, so a column would be empty on every healthy
	// pool while a note says more in the case that matters.
	for _, a := range accounts {
		if !a.OverageRefused {
			continue
		}
		line := "extra usage: " + a.Name + " refused by the provider"
		if a.OverageReason != "" {
			line += " (" + a.OverageReason + ")"
		}
		if !a.OverageCheckedAt.IsZero() {
			line += ", checked " + compactDur(time.Since(a.OverageCheckedAt)) + " ago"
		}
		fmt.Fprintln(out, line)
	}

	printUnpooled(st, out)
	return nil
}

// statusState is the slice of /api/state the human view reads.
type statusState struct {
	// Pinned names the account selection has been directed at (#158).
	Pinned   string `json:"pinned"`
	Unpooled *struct {
		Requests        int      `json:"requests"`
		Paths           int      `json:"paths"`
		InferenceShaped []string `json:"inferenceShaped"`
	} `json:"unpooled"`
}

// printPin announces a pin above the table (issue #158).
//
// A pin overrides every routing decision spillway makes — threshold,
// priority, stickiness, rotation — so a pinned pool and an automatic one
// print an identical table while behaving nothing alike: the pinned one is a
// single-account setup wearing a multi-account dashboard. Confirming a pin
// was cleared used to mean reading /api/state and inferring from an
// omitempty field being ABSENT, which is both a check nobody performs and a
// poor signal to reason from.
//
// Above the table, not below it: this is the frame the rows are read in, not
// a footnote about them. The overage and unpooled notes below the table are
// facts about particular rows; a pin is the mode the whole table is in.
//
// A header line rather than a column or a marked row, for two reasons. The
// table is read by people who know its shape and possibly parsed by scripts,
// and a line that appears only while a pin is set is a smaller change than a
// column that appears always or a row whose account cell grows a marker. And
// naming the account outright is what keeps it unambiguous in the common
// case — the pinned account is usually also the one serving, which is
// precisely where a marker sitting in a busy row is hardest to pick out.
func printPin(pinned string, out io.Writer) {
	if pinned == "" {
		return
	}
	fmt.Fprintf(out, "pinned to %s — rotation, priority and thresholds are bypassed\n", pinned)
	// `switch --auto`, not bare `spillway switch`: bare switch REPORTS the
	// pin, it does not clear it (see runSwitchReport). Printing the wrong
	// incantation here would leave the pin set by someone who believed they
	// had followed the instruction.
	fmt.Fprintln(out, "  `spillway switch --auto` restores automatic selection; a daemon restart also clears it")
	fmt.Fprintln(out)
}

// printUnpooled reports traffic spillway passed through without a pooled
// credential because it did not recognise the path (issue #176).
//
// This is the CLI half of making the inverted routing default safe. spillway
// now pools only what it recognises as inference, which means a new
// inference endpoint would be forwarded on the user's own credential with
// the pool sitting idle — and the only thing standing between that and a
// silent regression is the user being able to see it. The daemon warns in
// the log; this is the same fact for someone who runs `spillway status`
// rather than tailing a log file.
//
// A daemon too old to serve /api/state leaves the section nil and prints
// nothing: the accounts table above is still the answer to what was asked.
// statusline.go takes the same line.
func printUnpooled(st statusState, out io.Writer) {
	if st.Unpooled == nil {
		return
	}
	fmt.Fprintf(out, "unpooled: %d requests on %d paths spillway does not recognise, passed through on the client's own credential\n",
		st.Unpooled.Requests, st.Unpooled.Paths)
	if len(st.Unpooled.InferenceShaped) == 0 {
		return
	}
	// The line that matters: these took a POST with a body, which is what
	// an inference request looks like. If one of them IS inference, the
	// pool is not being used for it.
	fmt.Fprintf(out, "  inference-shaped, not pooled: %s — add to proxy.inferencePaths to pool it\n",
		strings.Join(st.Unpooled.InferenceShaped, ", "))
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

// headroomPct renders a window's remaining headroom as a whole percentage,
// clamped to [0, 100] (issue #112).
//
// Anthropic reports utilization above 1.0 once an allowance is overshot, and
// the parse in internal/provider/claude.go deliberately keeps that raw value:
// how far past an allowance an account went is real signal. But "-8% left" is
// not a quantity a reader can act on — 0% is the truth it is trying to
// express — so the clamp belongs here, at the render, and nowhere upstream.
//
// The dashboard (headroom() in internal/admin/static/index.html) and the
// status line (slWindow.headroom) already clamp. This is the third surface
// derived from the same figure, and the reason it is a function rather than
// an inline expression is that two of three surfaces agreeing looked, from
// the outside, like the CLI reporting different data.
func headroomPct(used, limit float64) int {
	if limit <= 0 {
		return 0
	}
	h := 1 - used/limit
	if h < 0 {
		h = 0
	}
	if h > 1 {
		h = 1
	}
	return int(h*100 + 0.5)
}
