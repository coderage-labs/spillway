package main

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

// Issue #169: a new account used to land at priority 0 — the top tier,
// competing for first-choice traffic with whatever was already there — and
// nothing said so. These cover the CLI end: the flag that overrides the
// default, the number reaching the running pool, and the number being said
// out loud.

func TestParseLoginPriority(t *testing.T) {
	two := 2
	zero := 0
	tests := []struct {
		name     string
		args     []string
		wantRest []string
		wantPrio *int
	}{
		{"no flag", []string{"work"}, []string{"work"}, nil},
		{"spaced", []string{"work", "--priority", "2"}, []string{"work"}, &two},
		{"equals", []string{"work", "--priority=2"}, []string{"work"}, &two},
		{"before the name", []string{"--priority", "2", "work"}, []string{"work"}, &two},
		{"explicit zero", []string{"work", "--priority", "0"}, []string{"work"}, &zero},
		{"negative", []string{"work", "--priority", "-1"}, []string{"work"}, ptr(-1)},
		{"no args at all", nil, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rest, prio, err := parseLoginPriority(tc.args)
			if err != nil {
				t.Fatalf("parseLoginPriority(%q): %v", tc.args, err)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("leftover args = %q, want %q — the account name is args[0], so a "+
					"leftover flag becomes a login name", rest, tc.wantRest)
			}
			switch {
			case tc.wantPrio == nil && prio != nil:
				t.Errorf("priority = %d, want nil (nothing asked for)", *prio)
			case tc.wantPrio != nil && prio == nil:
				t.Errorf("priority = nil, want %d", *tc.wantPrio)
			case tc.wantPrio != nil && *prio != *tc.wantPrio:
				t.Errorf("priority = %d, want %d", *prio, *tc.wantPrio)
			}
		})
	}
}

func TestParseLoginPriorityRejectsRubbish(t *testing.T) {
	for _, args := range [][]string{
		{"work", "--priority", "soon"},
		{"work", "--priority=soon"},
		{"work", "--priority"},
	} {
		if _, _, err := parseLoginPriority(args); err == nil {
			t.Errorf("parseLoginPriority(%q) = nil error, want a complaint — "+
				"silently ignoring it would put the account somewhere the user did not ask for", args)
		}
	}
}

func ptr(n int) *int { return &n }

// downDaemonConfig writes a scratch config naming an admin address nothing
// is listening on, so recordLogin exercises its config write and its
// output with the live-add landing in its "no daemon running" branch.
func downDaemonConfig(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(nil)
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	return liveTestConfig(t, addr)
}

// TestRecordLoginDefaultsToTheBackOfTheQueue: the headline. A pool holding
// 5, 6 and 9 gets 10 — one past the maximum, not the free slot at 7.
func TestRecordLoginDefaultsToTheBackOfTheQueue(t *testing.T) {
	cfgPath := downDaemonConfig(t)
	for name, p := range map[string]int{"a": 5, "b": 6, "c": 9} {
		if _, err := config.UpsertAccountWithPriority(cfgPath,
			config.AccountConfig{Name: name, Type: "claude-oauth"}, &p); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
	}

	var out bytes.Buffer
	if err := recordLogin(&out, cfgPath, recordedLogin{
		Account: config.AccountConfig{Name: "spare", Type: "claude-oauth"},
		Add:     accountAddPayload{Name: "spare", Type: "claude-oauth"},
		Banner:  "logged in: spare",
	}); err != nil {
		t.Fatalf("recordLogin: %v", err)
	}

	if got := configPriority(t, cfgPath, "spare"); got != 10 {
		t.Errorf("new account after 5,6,9 recorded at priority %d, want 10", got)
	}
	// Said out loud, not just written down: a silent default is how someone
	// is surprised by routing later.
	if !strings.Contains(out.String(), "priority 10") {
		t.Errorf("output never states the assigned priority; got:\n%s", out.String())
	}
}

// TestRecordLoginExplicitPriorityWins: --priority beats the default, and is
// what both the config and the output report.
func TestRecordLoginExplicitPriorityWins(t *testing.T) {
	cfgPath := downDaemonConfig(t)
	nine := 9
	if _, err := config.UpsertAccountWithPriority(cfgPath,
		config.AccountConfig{Name: "a", Type: "claude-oauth"}, &nine); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	want := 3
	if err := recordLogin(&out, cfgPath, recordedLogin{
		Account:  config.AccountConfig{Name: "spare", Type: "claude-oauth"},
		Priority: &want,
		Add:      accountAddPayload{Name: "spare", Type: "claude-oauth"},
		Banner:   "logged in: spare",
	}); err != nil {
		t.Fatalf("recordLogin: %v", err)
	}
	if got := configPriority(t, cfgPath, "spare"); got != want {
		t.Errorf("--priority %d recorded as %d", want, got)
	}
	if !strings.Contains(out.String(), "priority 3") {
		t.Errorf("output never states the explicit priority; got:\n%s", out.String())
	}
}

// TestRecordLoginLeavesExistingAccountsAlone: an add decides its own
// number and nothing else's — including in a hand-edited config where two
// accounts already share one.
func TestRecordLoginLeavesExistingAccountsAlone(t *testing.T) {
	cfgPath := downDaemonConfig(t)
	before := map[string]int{"twin-a": 3, "twin-b": 3, "solo": 1}
	for name, p := range before {
		if _, err := config.UpsertAccountWithPriority(cfgPath,
			config.AccountConfig{Name: name, Type: "claude-oauth"}, &p); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
	}

	if err := recordLogin(&bytes.Buffer{}, cfgPath, recordedLogin{
		Account: config.AccountConfig{Name: "spare", Type: "claude-oauth"},
		Add:     accountAddPayload{Name: "spare", Type: "claude-oauth"},
		Banner:  "logged in: spare",
	}); err != nil {
		t.Fatalf("recordLogin: %v", err)
	}
	if got := configPriority(t, cfgPath, "spare"); got != 4 {
		t.Errorf("new account after 3,3,1 got priority %d, want 4", got)
	}
	for name, want := range before {
		if got := configPriority(t, cfgPath, name); got != want {
			t.Errorf("add renumbered %q: priority %d, want %d", name, got, want)
		}
	}
}

// TestRecordLoginNewAccountRanksBehindExistingInRunningPool is the live
// half, asserted against a real selection on a real pool behind a real
// admin server — not against a response flag or the config. The existing
// account is deliberately the LOADED one, so if the new account arrived at
// priority 0 (the #169 bug) the selector's load tie-break would hand it
// first-choice traffic immediately.
func TestRecordLoginNewAccountRanksBehindExistingInRunningPool(t *testing.T) {
	existing := pool.NewAccount("existing", pool.SourceYAML, "t", "", 0, "")
	existing.Type = "claude-oauth"
	p := pool.New([]*pool.Account{existing}, time.Now())

	srv := admin.New(p, nil, events.New(), "")
	front := httptest.NewServer(srv)
	defer front.Close()
	cfgPath := liveTestConfig(t, strings.TrimPrefix(front.URL, "http://"), "existing")

	// Load the incumbent up: three requests in flight, never released.
	if err := p.Pin("existing", false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got := p.SelectFor(fmt.Sprintf("warmup-%d", i), nil); got == nil || got.Name != "existing" {
			t.Fatalf("warmup selection = %v, want existing", got)
		}
	}
	p.Unpin()

	var out bytes.Buffer
	if err := recordLogin(&out, cfgPath, recordedLogin{
		Account: config.AccountConfig{Name: "fresh", Type: "claude-oauth"},
		Add:     accountAddPayload{Name: "fresh", Type: "claude-oauth", AccessToken: "tok"},
		Banner:  "logged in: fresh",
	}); err != nil {
		t.Fatalf("recordLogin: %v", err)
	}
	if msg := out.String(); strings.Contains(strings.ToLower(msg), "not applied") {
		t.Fatalf("live add did not reach the daemon: %s", msg)
	}

	got := p.Select("session-a")
	if got == nil {
		t.Fatal("nothing selected")
	}
	if got.Name != "existing" {
		t.Fatalf("selected %q — a brand-new account must rank BEHIND the incumbent in the "+
			"running pool, even when the incumbent is the loaded one; the config's priority "+
			"did not reach the daemon", got.Name)
	}
}

// TestRecordLoginExplicitPriorityReachesRunningPool is the same wiring in
// the other direction: --priority 0 must make the new account the pool's
// first choice immediately, past an incumbent that is idle and would
// otherwise win on the default.
func TestRecordLoginExplicitPriorityReachesRunningPool(t *testing.T) {
	existing := pool.NewAccount("existing", pool.SourceYAML, "t", "", 0, "")
	existing.Type = "claude-oauth"
	existing.SetPriority(5)
	p := pool.New([]*pool.Account{existing}, time.Now())

	srv := admin.New(p, nil, events.New(), "")
	front := httptest.NewServer(srv)
	defer front.Close()
	cfgPath := liveTestConfig(t, strings.TrimPrefix(front.URL, "http://"))
	five := 5
	if _, err := config.UpsertAccountWithPriority(cfgPath,
		config.AccountConfig{Name: "existing", Type: "claude-oauth"}, &five); err != nil {
		t.Fatal(err)
	}

	zero := 0
	if err := recordLogin(&bytes.Buffer{}, cfgPath, recordedLogin{
		Account:  config.AccountConfig{Name: "fresh", Type: "claude-oauth"},
		Priority: &zero,
		Add:      accountAddPayload{Name: "fresh", Type: "claude-oauth", AccessToken: "tok"},
		Banner:   "logged in: fresh",
	}); err != nil {
		t.Fatalf("recordLogin: %v", err)
	}

	got := p.Select("session-a")
	if got == nil {
		t.Fatal("nothing selected")
	}
	if got.Name != "fresh" {
		t.Fatalf("selected %q, want fresh — an explicit --priority 0 must reach the running "+
			"pool, not be replaced by the next-free default", got.Name)
	}
}

// configPriority reads one account's recorded priority back off disk.
func configPriority(t *testing.T, path, name string) int {
	t.Helper()
	accts, err := config.ListAccountConfigs(path)
	if err != nil {
		t.Fatalf("ListAccountConfigs: %v", err)
	}
	for _, a := range accts {
		if a.Name == name {
			return a.Priority
		}
	}
	t.Fatalf("account %q not in config %s", name, path)
	return 0
}
