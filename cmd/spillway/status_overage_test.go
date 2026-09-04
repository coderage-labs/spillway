package main

// Issue #151: `overageReason` was the complete answer to "why is my extra
// usage not working" and appeared in no surface a user looks at — only in
// /api/accounts, where nothing but the dashboard reads it. Diagnosing the
// live incident took six queries because of that.
//
// These drive statusTable against a stub admin server. Never dialAdmin: that
// resolves the real config and the running daemon's listener.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubAdmin serves one canned /api/accounts payload.
func stubAdmin(t *testing.T, accountsJSON string) *adminAPI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/accounts" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(accountsJSON))
	}))
	t.Cleanup(srv.Close)
	return &adminAPI{client: srv.Client(), base: srv.URL}
}

func renderStatus(t *testing.T, accountsJSON string) string {
	t.Helper()
	var buf strings.Builder
	if err := statusTable(stubAdmin(t, accountsJSON), &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestStatusNamesTheProvidersOverageRefusal(t *testing.T) {
	checked := time.Now().Add(-12 * time.Minute).UTC().Format(time.RFC3339)
	out := renderStatus(t, `[{"name":"acct-a","type":"claude-oauth","state":"exhausted",
		"overageRefused":true,"overageReason":"member_zero_credit_limit",
		"overageCheckedAt":"`+checked+`"}]`)

	// The account is still in the table.
	if !strings.Contains(out, "acct-a") {
		t.Fatalf("status lost the account row:\n%s", out)
	}
	// The reason — the whole point.
	if !strings.Contains(out, "member_zero_credit_limit") {
		t.Errorf("status does not name the provider's refusal reason:\n%s", out)
	}
	if !strings.Contains(out, "refused by the provider") {
		t.Errorf("status does not say extra usage is refused:\n%s", out)
	}
	// The age, so a reader can tell a current refusal from one about to be
	// re-tested.
	if !strings.Contains(out, "12m ago") {
		t.Errorf("status does not report when the refusal was read:\n%s", out)
	}
}

// A refusal with no reason still gets reported: "the provider will not sell
// you more" is the actionable half.
func TestStatusReportsARefusalWithNoReason(t *testing.T) {
	out := renderStatus(t, `[{"name":"acct-a","type":"claude-oauth","state":"exhausted",
		"overageRefused":true}]`)
	if !strings.Contains(out, "refused by the provider") {
		t.Errorf("a reasonless refusal was dropped entirely:\n%s", out)
	}
	if strings.Contains(out, "()") {
		t.Errorf("empty parentheses rendered for a missing reason:\n%s", out)
	}
}

// No refusal, no note. The line is an addition for the case that has one,
// not new permanent noise on every healthy pool.
func TestStatusIsQuietWithNoRefusal(t *testing.T) {
	out := renderStatus(t, `[
		{"name":"healthy","type":"claude-oauth","state":"ok"},
		{"name":"expired-refusal","type":"claude-oauth","state":"exhausted",
		 "overageReason":"member_zero_credit_limit"}
	]`)
	// The second account carries a reason but no live refusal — the state
	// after the expiry in issue #151. The reason is kept on file, but the
	// CLI must not present a refusal spillway has already stopped believing.
	if strings.Contains(out, "extra usage:") {
		t.Errorf("status announced a refusal nothing is acting on:\n%s", out)
	}
}
