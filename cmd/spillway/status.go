package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
)

// tokenPathFor locates the admin token beside the config. Absent on a
// loopback listener, which needs none.
func tokenPathFor(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "spillway-admin-token")
}

// runStatus implements `spillway status`: query the running daemon's admin
// API and print a compact pool summary. A loopback listener needs no token,
// so the token file is used only when one exists.
func runStatus() error {
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	addr := cfg.Admin.Addr
	if addr == "" {
		addr = admin.DefaultAddr
	}

	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/accounts", nil)
	if err != nil {
		return err
	}
	if b, err := os.ReadFile(tokenPathFor(cfgPath)); err == nil {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("admin API unreachable (is `spillway server` running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("admin API %d", resp.StatusCode)
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
		} `json:"quotaWindows"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&accounts); err != nil {
		return fmt.Errorf("decode admin response: %w", err)
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
