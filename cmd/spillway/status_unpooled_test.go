package main

// Issue #176: `spillway status` must report traffic spillway passed through
// without a pooled credential because it did not recognise the path.
//
// The inverted routing default trades a loud wrong behaviour (a pooled
// credential injected into anything unrecognised) for a quiet one (a new
// inference endpoint silently unpooled). That trade is only worth making if
// the quiet case is visible, and a log line that scrolls away is not
// visible enough on its own. This is the surface a user actually looks at.
//
// Drives statusTable against a stub admin server. Never dialAdmin: that
// resolves the real config and the running daemon's listener.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubAdminWithState serves /api/accounts and /api/state.
func stubAdminWithState(t *testing.T, accountsJSON, stateJSON string) *adminAPI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/accounts":
			_, _ = w.Write([]byte(accountsJSON))
		case "/api/state":
			_, _ = w.Write([]byte(stateJSON))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &adminAPI{client: srv.Client(), base: srv.URL}
}

const oneHealthyAccount = `[{"name":"acct-a","type":"claude-oauth","state":"ok"}]`

func renderStatusWithState(t *testing.T, stateJSON string) string {
	t.Helper()
	var buf strings.Builder
	if err := statusTable(stubAdminWithState(t, oneHealthyAccount, stateJSON), &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestStatusReportsUnpooledTraffic: the count and the path total both
// appear, so a user can see spillway is declining to claim traffic without
// tailing the daemon log.
func TestStatusReportsUnpooledTraffic(t *testing.T) {
	out := renderStatusWithState(t, `{"usable":1,"total":1,
		"unpooled":{"requests":7500,"paths":11}}`)

	if !strings.Contains(out, "acct-a") {
		t.Fatalf("status lost the account row:\n%s", out)
	}
	if !strings.Contains(out, "7500") || !strings.Contains(out, "11") {
		t.Errorf("status does not report the unpooled counts:\n%s", out)
	}
	if !strings.Contains(out, "does not recognise") {
		t.Errorf("status does not say what unpooled means:\n%s", out)
	}
}

// TestStatusNamesTheInferenceShapedPath is the line that matters. An
// unrecognised path taking POSTs with bodies is the one that might BE a new
// inference endpoint, so status must name it and say what to do about it.
func TestStatusNamesTheInferenceShapedPath(t *testing.T) {
	out := renderStatusWithState(t, `{"usable":1,"total":1,
		"unpooled":{"requests":40,"paths":2,"inferenceShaped":["/v1/responses"]}}`)

	if !strings.Contains(out, "/v1/responses") {
		t.Errorf("status does not name the inference-shaped path:\n%s", out)
	}
	if !strings.Contains(out, "inferencePaths") {
		t.Errorf("status does not say how to pool it:\n%s", out)
	}
}

// TestStatusSaysNothingWhenNothingIsUnpooled: a healthy pool must not grow
// a line about a thing that is not happening.
func TestStatusSaysNothingWhenNothingIsUnpooled(t *testing.T) {
	out := renderStatusWithState(t, `{"usable":1,"total":1}`)
	if strings.Contains(out, "unpooled") {
		t.Errorf("status reported unpooled traffic when there is none:\n%s", out)
	}
}

// TestStatusSurvivesADaemonWithoutTheStateSection: an older daemon has no
// /api/state at all. The accounts table is still the answer to what was
// asked, so a missing section must print nothing rather than fail the
// command.
func TestStatusSurvivesADaemonWithoutTheStateSection(t *testing.T) {
	var buf strings.Builder
	// stubAdmin (status_overage_test.go) 404s everything but /api/accounts.
	if err := statusTable(stubAdmin(t, oneHealthyAccount), &buf); err != nil {
		t.Fatalf("status failed because /api/state was missing: %v", err)
	}
	if !strings.Contains(buf.String(), "acct-a") {
		t.Errorf("status lost the account row:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "unpooled") {
		t.Errorf("status invented an unpooled line from a missing endpoint:\n%s", buf.String())
	}
}
