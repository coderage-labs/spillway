package main

// Issue #165, the visible half: the status line stopped rendering because
// /api/accounts no longer answered inside statusTimeout, and the failure
// path printed nothing — the same nothing a session that is not proxied
// through spillway prints.
//
// These drive runStatusline end to end against a REAL admin server backed
// by a real request log, because the whole defect lived in the seam between
// the two: every render unit test in statusline_test.go was green while the
// line was blank on the user's machine.
//
// No test here asserts a duration. What is asserted is that a daemon which
// answers produces a line, that a session which is not on spillway produces
// neither a line nor a connection, and that a daemon which cannot answer is
// distinguishable from both.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// setAttachEnv points this process's proxy environment at addr — or, with an
// empty addr, at nothing at all.
//
// The clears come FIRST and the set comes LAST, and that order is load
// bearing on Windows. Go reads and writes the environment there through
// GetEnvironmentVariableW/SetEnvironmentVariableW (syscall/env_windows.go),
// which are CASE-INSENSITIVE, so HTTPS_PROXY and https_proxy are one
// variable: setting the upper-case name and then clearing the lower-case one
// wipes what was just set. A test that did it in that order left the session
// unattached, runStatusline returned at the attach check before any HTTP, and
// the whole point of the test evaporated — silently on Windows, invisibly
// everywhere else. Callers assert attachedToSpillway afterwards rather than
// trusting this to have worked.
func setAttachEnv(t *testing.T, addr string) {
	t.Helper()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "ANTHROPIC_BASE_URL"} {
		t.Setenv(k, "")
	}
	if addr != "" {
		t.Setenv("HTTPS_PROXY", addr)
	}
}

// mustBeAttached fails unless runStatusline will treat this process as going
// through the configured proxy. Without it, every "the line rendered" test
// can pass or fail for a reason it never names — see setAttachEnv.
func mustBeAttached(t *testing.T, want bool) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got := attachedToSpillway(cfg); got != want {
		t.Fatalf("attachedToSpillway = %v, want %v — the proxy environment did not take "+
			"effect, so this test would be measuring the wrong branch (HTTPS_PROXY=%q)",
			got, want, os.Getenv("HTTPS_PROXY"))
	}
}

// statuslineFixture stands up the real admin server over a real request log
// with n accounts and rows of logged traffic, writes a config naming it, and
// returns the number of admin requests served so a test can assert that
// nothing was contacted at all. proxyPort is deliberately not spillway's
// default 7654: nothing here binds it, and a fixture should not name a port
// a real daemon might be using.
func statuslineFixture(t *testing.T, accountCount, rowsPerAccount int) (hits *atomic.Int64, proxyPort int) {
	t.Helper()

	var accts []*pool.Account
	for i := 0; i < accountCount; i++ {
		a := pool.NewAccount(fmt.Sprintf("acct-%d", i), pool.SourceYAML, "tok", "", 0, "")
		a.Type = "claude-oauth"
		a.SetQuotaWindows([]pool.QuotaWindow{{
			Name: "5h", Limit: 100, Used: float64(10 + i), Source: "headers",
			ResetAt: time.Now().Add(time.Hour), FetchedAt: time.Now(),
		}})
		accts = append(accts, a)
	}
	p := pool.New(accts, time.Now())

	l, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	for i := 0; i < accountCount; i++ {
		for j := 0; j < rowsPerAccount; j++ {
			if err := l.Record(reqlog.Entry{
				Account: fmt.Sprintf("acct-%d", i), Path: "/v1/messages",
				Status: 200, Event: reqlog.EventServed,
				CacheCreationInputTokens: 4165, CacheReadInputTokens: 1816,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	hits = &atomic.Int64{}
	inner := admin.New(p, l, events.New(), "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	proxyPort = 61988
	cfgPath := filepath.Join(t.TempDir(), "spillway.yaml")
	cfg := fmt.Sprintf("admin:\n  addr: %s\nproxy:\n  host: 127.0.0.1\n  port: %d\n",
		strings.TrimPrefix(srv.URL, "http://"), proxyPort)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPILLWAY_CONFIG", cfgPath)
	t.Setenv("NO_COLOR", "1")
	setAttachEnv(t, "")
	return hits, proxyPort
}

// A session attached to a daemon that answers within statusTimeout renders
// the account and its headroom. Eight accounts and a request log with real
// volume in it — the shape that broke — driven through the actual
// /api/accounts handler and the actual 350ms client.
func TestStatuslineRendersWhenTheDaemonAnswersInBudget(t *testing.T) {
	hits, port := statuslineFixture(t, 8, 50)
	setAttachEnv(t, fmt.Sprintf("http://127.0.0.1:%d", port))
	mustBeAttached(t, true)

	got := captureStdout(t, func() {
		if err := runStatusline(nil); err != nil {
			t.Errorf("runStatusline: %v", err)
		}
	})
	if got == "" {
		t.Fatal("attached session with a responsive daemon printed nothing — issue #165's symptom")
	}
	if !strings.Contains(got, "acct-") {
		t.Errorf("line = %q, want the serving account named", got)
	}
	// A real headroom bar, so this is the healthy render and not the
	// degraded marker wearing the account's name.
	if !strings.Contains(got, "█") || !strings.Contains(got, "%") {
		t.Errorf("line = %q, want a headroom bar and a percentage", got)
	}
	if got == "⛁ —" {
		t.Errorf("line = %q, want the healthy render, not the unreachable marker", got)
	}
	if n := hits.Load(); n < 2 {
		t.Errorf("the daemon served %d admin requests, want both /api/accounts and /api/state", n)
	}
}

// The same daemon, the same accounts, answering perfectly — but this
// session is not going through spillway. It must print nothing, and it must
// not even open a connection: the silence for an unattached session is
// decided before any HTTP happens, which is exactly why issue #165's
// degraded marker cannot leak into it.
func TestStatuslineStaysSilentAndOfflineForASessionNotOnSpillway(t *testing.T) {
	hits, _ := statuslineFixture(t, 8, 50)
	// Proxied somewhere that is emphatically not spillway.
	setAttachEnv(t, "http://127.0.0.1:3128")
	mustBeAttached(t, false)

	got := captureStdout(t, func() {
		if err := runStatusline(nil); err != nil {
			t.Errorf("runStatusline: %v", err)
		}
	})
	if got != "" {
		t.Errorf("a session not on spillway printed %q, want nothing at all", got)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("a session not on spillway made %d admin requests, want 0", n)
	}
}

// --always is the escape hatch for "tell me regardless of this session", so
// it renders for an unattached session too — and when the daemon it names
// cannot be reached, it gets the marker rather than the silence that made
// #165 unreadable. Asserted separately from the attached case because
// --always is the flag the issue singles out.
func TestStatuslineAlwaysMarksAnUnreachableDaemon(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "spillway.yaml")
	if err := os.WriteFile(cfgPath, []byte(
		"admin:\n  addr: 127.0.0.1:1\nproxy:\n  host: 127.0.0.1\n  port: 61989\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPILLWAY_CONFIG", cfgPath)
	t.Setenv("NO_COLOR", "1")
	setAttachEnv(t, "")
	// Unattached on purpose: --always is what makes this render anyway.
	mustBeAttached(t, false)

	got := captureStdout(t, func() {
		if err := runStatusline([]string{"--always"}); err != nil {
			t.Errorf("runStatusline: %v", err)
		}
	})
	if got != "⛁ —" {
		t.Errorf("`statusline --always` against a dead daemon printed %q, want %q", got, "⛁ —")
	}
}

// A reachable daemon with no accounts configured is not a failure, and the
// marker must not claim it is: that case is explained properly by the
// dashboard and by `spillway status`, and the prompt stays out of it. This
// pins the boundary of the change — only an unanswered fetch prints
// anything new.
func TestStatuslineStaysSilentForAReachableDaemonWithNoAccounts(t *testing.T) {
	hits, port := statuslineFixture(t, 0, 0)
	setAttachEnv(t, fmt.Sprintf("http://127.0.0.1:%d", port))
	mustBeAttached(t, true)

	got := captureStdout(t, func() {
		if err := runStatusline(nil); err != nil {
			t.Errorf("runStatusline: %v", err)
		}
	})
	if got != "" {
		t.Errorf("a reachable daemon with no accounts printed %q, want nothing", got)
	}
	if n := hits.Load(); n == 0 {
		t.Fatal("the daemon was never contacted — this test is not exercising the reachable path")
	}
}
