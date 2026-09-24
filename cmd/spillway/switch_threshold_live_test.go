package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

// putSettings sends the body the dashboard's rotate-away slider sends —
// one key, over the real PUT /api/settings the Save button also uses.
func putSettings(t *testing.T, baseURL, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, baseURL+"/api/settings", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT /api/settings = %d: %s", resp.StatusCode, b)
	}
}

// TestSliderThresholdReachesConfigAndRunningPool is issue #168's load-bearing
// claim: the slider is not a UI toy. The value it writes has to land in the
// config file AND change what the running pool selects, with no restart.
//
// Proved by a selection outcome rather than by reading a field back: the
// pool's own reader (Threshold) agreeing with what was written says nothing
// about whether the selector consults it. So "preferred" is given the better
// priority and 60% of its window, and the test asserts that moving the
// threshold below 0.60 is enough to hand the next request to "reserve" —
// which only happens if the selector is using the new number.
//
// The wiring is main.go's, verbatim: admin.EnableSettings with
// p.Apply(poolSettings(nc)) as the apply callback.
func TestSliderThresholdReachesConfigAndRunningPool(t *testing.T) {
	now := time.Now()
	preferred := pool.NewAccount("preferred", pool.SourceYAML, "t", "", 0, "")
	preferred.Type = "claude-oauth"
	preferred.SetQuotaWindows([]pool.QuotaWindow{{
		Name: "5h", Limit: 1, Used: 0.60, Source: "headers",
		ResetAt: now.Add(time.Hour), FetchedAt: now,
	}})
	reserve := pool.NewAccount("reserve", pool.SourceYAML, "t", "", 0, "")
	reserve.Type = "claude-oauth"
	reserve.SetQuotaWindows([]pool.QuotaWindow{{
		Name: "5h", Limit: 1, Used: 0.10, Source: "headers",
		ResetAt: now.Add(time.Hour), FetchedAt: now,
	}})
	p := pool.New([]*pool.Account{preferred, reserve}, now)

	srv := admin.New(p, nil, events.New(), "")
	front := httptest.NewServer(srv)
	defer front.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "spillway.yaml")
	// exhaustedMode/holdMax are set to non-defaults deliberately: they are
	// the unrelated settings the one-key PUT must not disturb.
	seed := "admin:\n  addr: " + strings.TrimPrefix(front.URL, "http://") + "\n" +
		"pool:\n  exhaustedMode: hold\n  holdMax: 9h\n  switchThreshold: 0.98\n"
	if err := os.WriteFile(cfgPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.UpsertAccount(cfgPath, config.AccountConfig{
		Name: "preferred", Type: "claude-oauth", Priority: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := config.UpsertAccount(cfgPath, config.AccountConfig{
		Name: "reserve", Type: "claude-oauth", Priority: 5,
	}); err != nil {
		t.Fatal(err)
	}
	srv.EnableSettings(cfgPath, func(nc *config.Config) { p.Apply(poolSettings(nc)) })

	// Bring the pool up to what the config says, the way buildPool does at
	// startup. Without this the priorities live only in the file.
	start, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p.Apply(poolSettings(start))

	// Before: at 0.98 the preferred account is nowhere near the threshold,
	// so its better priority wins despite being the busier of the two.
	got := p.Select("before")
	if got == nil || got.Name != "preferred" {
		t.Fatalf("baseline selection = %v, want preferred (the fixture is wrong, not the feature)", got)
	}
	p.Done(got)

	// What the slider sends: one key, as a string, through the shared
	// settings endpoint.
	putSettings(t, front.URL, `{"switchThreshold":"0.55"}`)

	// 1. It reached the config file.
	after, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Pool.SwitchThreshold != 0.55 {
		t.Errorf("config switchThreshold = %v, want 0.55 — the slider's value never reached the file", after.Pool.SwitchThreshold)
	}

	// 2. Unrelated settings are untouched. The write path is shared with
	// every other control, so a partial body that resets what it did not
	// name would silently undo them.
	if after.Pool.ExhaustedMode != "hold" {
		t.Errorf("exhaustedMode = %q, want %q — a one-key PUT clobbered an unrelated setting", after.Pool.ExhaustedMode, "hold")
	}
	if after.Pool.HoldMax != "9h" {
		t.Errorf("holdMax = %q, want %q — a one-key PUT clobbered an unrelated setting", after.Pool.HoldMax, "9h")
	}
	byName := map[string]int{}
	for _, a := range after.Accounts {
		byName[a.Name] = a.Priority
	}
	if byName["reserve"] != 5 || byName["preferred"] != 0 {
		t.Errorf("account priorities = %v, want preferred 0 / reserve 5 — a one-key PUT clobbered them", byName)
	}

	// 3. It reached the running pool's own reader...
	if p.Threshold() != 0.55 {
		t.Errorf("pool.Threshold() = %v, want 0.55 — the running pool never saw it", p.Threshold())
	}

	// 4. ...and, the part that matters, the selector is using it. preferred
	// is at 0.60 of its window, now over the threshold, so it is skipped
	// while an eligible account exists — even though its priority is better.
	got = p.Select("after")
	if got == nil {
		t.Fatal("nothing selected after lowering the threshold")
	}
	if got.Name != "reserve" {
		t.Fatalf("selected %q after lowering the threshold to 0.55; preferred is at 0.60 of its window "+
			"and must now be rotated away from — the new threshold is not reaching the selector", got.Name)
	}
}

// TestSliderThresholdBoundsRoundTrip covers what the slider sends at its two
// extremes, which is not what it renders: JavaScript stringifies the top of
// the range as "1", not "1.00", and the bottom as "0.5". Both have to be
// accepted, land as the right number, and come back through CurrentSettings
// in a form the control can be rebuilt from — a bound that 400s or reads back
// as something else is a control that fails only at the ends.
func TestSliderThresholdBoundsRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		sent string
		want float64
	}{
		{"1", 1},
		{"0.5", 0.5},
	} {
		t.Run(tc.sent, func(t *testing.T) {
			a := pool.NewAccount("only", pool.SourceYAML, "t", "", 0, "")
			a.Type = "claude-oauth"
			p := pool.New([]*pool.Account{a}, time.Now())
			srv := admin.New(p, nil, events.New(), "")
			front := httptest.NewServer(srv)
			defer front.Close()

			cfgPath := filepath.Join(t.TempDir(), "spillway.yaml")
			seed := "admin:\n  addr: " + strings.TrimPrefix(front.URL, "http://") + "\n" +
				"pool:\n  switchThreshold: 0.98\n"
			if err := os.WriteFile(cfgPath, []byte(seed), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := config.UpsertAccount(cfgPath, config.AccountConfig{Name: "only", Type: "claude-oauth"}); err != nil {
				t.Fatal(err)
			}
			srv.EnableSettings(cfgPath, func(nc *config.Config) { p.Apply(poolSettings(nc)) })

			putSettings(t, front.URL, `{"switchThreshold":"`+tc.sent+`"}`)

			after, err := config.LoadFrom(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if after.Pool.SwitchThreshold != tc.want {
				t.Errorf("config switchThreshold = %v, want %v", after.Pool.SwitchThreshold, tc.want)
			}
			if p.Threshold() != tc.want {
				t.Errorf("pool.Threshold() = %v, want %v", p.Threshold(), tc.want)
			}
			// And back out again: the panel is rebuilt from this, so a bound
			// that reads back unparseable is a slider that resets on reload.
			cur := config.CurrentSettings(after)
			if cur.SwitchThreshold == nil {
				t.Fatal("CurrentSettings dropped switchThreshold")
			}
			var back float64
			if _, err := fmt.Sscanf(*cur.SwitchThreshold, "%g", &back); err != nil || back != tc.want {
				t.Errorf("CurrentSettings switchThreshold = %q (parsed %v, err %v), want %v",
					*cur.SwitchThreshold, back, err, tc.want)
			}
		})
	}
}

// TestSliderThresholdRejectsAValueOutsideTheConfigsRange keeps the browser
// honest about the one thing the slider's bounds cannot guarantee: bounds
// are markup, and markup is editable. The server is what decides.
func TestSliderThresholdRejectsAValueOutsideTheConfigsRange(t *testing.T) {
	a := pool.NewAccount("only", pool.SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, time.Now())
	srv := admin.New(p, nil, events.New(), "")
	front := httptest.NewServer(srv)
	defer front.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "spillway.yaml")
	seed := "admin:\n  addr: " + strings.TrimPrefix(front.URL, "http://") + "\n" +
		"pool:\n  switchThreshold: 0.98\n"
	if err := os.WriteFile(cfgPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.UpsertAccount(cfgPath, config.AccountConfig{Name: "only", Type: "claude-oauth"}); err != nil {
		t.Fatal(err)
	}
	srv.EnableSettings(cfgPath, func(nc *config.Config) { p.Apply(poolSettings(nc)) })

	req, err := http.NewRequest(http.MethodPut, front.URL+"/api/settings",
		bytes.NewBufferString(`{"switchThreshold":"1.5"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT switchThreshold 1.5 = %d, want 400", resp.StatusCode)
	}
	// And nothing was written: an invalid value that reaches disk stops the
	// daemon coming up.
	after, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Pool.SwitchThreshold != 0.98 {
		t.Errorf("config switchThreshold = %v after a rejected write, want 0.98 untouched", after.Pool.SwitchThreshold)
	}
}
