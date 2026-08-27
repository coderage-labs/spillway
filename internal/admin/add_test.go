package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

func postAccountAdd(t *testing.T, s *Server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7657/api/accounts/add", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// TestAccountAddPutsNewAccountIntoPool is the admin-layer half of issue
// #87: a brand new account posted here must be selectable immediately —
// asserted against selection, not just a response flag.
func TestAccountAddPutsNewAccountIntoPool(t *testing.T) {
	existing := pool.NewAccount("existing", pool.SourceYAML, "tok", "", 0, "")
	existing.Type = "claude-oauth"
	p := pool.New([]*pool.Account{existing}, time.Now())
	s := New(p, nil, events.New(), "")

	rec := postAccountAdd(t, s, `{"name":"fresh","type":"claude-oauth","accessToken":"newtok","expiresAt":1234}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got accountAddResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Added {
		t.Error("response says added=false for a brand new name")
	}
	if got.RestartRequired {
		t.Errorf("a plain claude-oauth account (no custom upstream) must not need a restart: %q", got.Reason)
	}

	if err := p.Pin("fresh", false); err != nil {
		t.Fatalf("the added account is not resolvable by the pool: %v", err)
	}
	sel := p.SelectFor("s", nil)
	if sel == nil || sel.Name != "fresh" {
		t.Fatalf("SelectFor while pinned to the added account = %v, want fresh", sel)
	}
	if tok, _, _ := sel.Credentials(); tok != "newtok" {
		t.Errorf("access token = %q, want the one just posted", tok)
	}
}

// TestAccountAddReauthHotSwapsCredentials is issue #87's fold-in of #46's
// re-auth gap: posting the same name again must update the EXISTING
// account's credentials in place, not create a duplicate, and must revive
// an account that had gone StateDisabled.
func TestAccountAddReauthHotSwapsCredentials(t *testing.T) {
	a := pool.NewAccount("work", pool.SourceYAML, "old-tok", "old-refresh", 1, "")
	a.Type = "claude-oauth"
	a.Disable() // simulate a dead refresh token
	p := pool.New([]*pool.Account{a}, time.Now())
	s := New(p, nil, events.New(), "")

	rec := postAccountAdd(t, s, `{"name":"work","type":"claude-oauth","accessToken":"new-tok","refreshToken":"new-refresh","expiresAt":999}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got accountAddResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Added {
		t.Error("response says added=true for a name that already existed — should be a hot-swap")
	}

	if len(p.Accounts()) != 1 {
		t.Fatalf("re-auth created a duplicate: %d accounts", len(p.Accounts()))
	}
	tok, refresh, expiresAt := a.Credentials()
	if tok != "new-tok" || refresh != "new-refresh" || expiresAt != 999 {
		t.Errorf("credentials not hot-swapped: got (%q, %q, %d)", tok, refresh, expiresAt)
	}
	if a.State() != pool.StateOK {
		t.Errorf("re-auth must revive a disabled account, got state %v", a.State())
	}
}

// TestAccountAddRejectsMalformedBody mirrors TestAccountRemoveRejectsMalformedBody.
func TestAccountAddRejectsMalformedBody(t *testing.T) {
	p := pool.New(nil, time.Now())
	s := New(p, nil, events.New(), "")

	if rec := postAccountAdd(t, s, `not json`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body: got %d, want 400", rec.Code)
	}
	if rec := postAccountAdd(t, s, `{"name":""}`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("empty name: got %d, want 400", rec.Code)
	}
}

// TestAccountAddMethodGuard mirrors TestAccountRemoveMethodGuard.
func TestAccountAddMethodGuard(t *testing.T) {
	p := pool.New(nil, time.Now())
	s := New(p, nil, events.New(), "")

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "http://127.0.0.1:7657/api/accounts/add", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: got %d, want 405", method, rec.Code)
		}
	}
}

// TestAccountAddCSRF mirrors TestAccountRemoveCSRF: /api/accounts/add must
// be no easier for another site to drive than /api/settings, /api/pin or
// /api/accounts/remove are.
func TestAccountAddCSRF(t *testing.T) {
	newServer := func() *Server {
		p := pool.New(nil, time.Now())
		return New(p, nil, events.New(), "")
	}

	blocked := []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://evil.example.com"},
	}
	for _, h := range blocked {
		s := newServer()
		if rec := postAccountAdd(t, s, `{"name":"work","type":"claude-oauth","accessToken":"t"}`, h); rec.Code != http.StatusForbidden {
			t.Errorf("headers %v: got %d, want 403", h, rec.Code)
		}
	}

	allowed := []map[string]string{
		{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:7657"},
		nil, // curl / the CLI
	}
	for _, h := range allowed {
		s := newServer()
		if rec := postAccountAdd(t, s, `{"name":"work","type":"claude-oauth","accessToken":"t"}`, h); rec.Code != http.StatusOK {
			t.Errorf("headers %v: got %d (%s), want 200", h, rec.Code, rec.Body.String())
		}
	}
}

// TestAccountAddCustomUpstreamReportsRestartRequired: a genuinely custom
// upstream host (not any provider's default) must be flagged as needing a
// restart for CONNECT-mode coverage — this is #87's "still restart-only"
// case. With no EnableLiveMITM wired (hostCovered nil), the handler must
// not silently assume it's fine.
func TestAccountAddCustomUpstreamReportsRestartRequired(t *testing.T) {
	p := pool.New(nil, time.Now())
	s := New(p, nil, events.New(), "")

	rec := postAccountAdd(t, s, `{"name":"custom-1","type":"claude-oauth","upstream":"https://custom.example.com/v1","accessToken":"t"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got accountAddResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Added {
		t.Error("a custom-upstream account must still be added to the pool for base-URL-mode requests")
	}
	if !got.RestartRequired {
		t.Error("a custom, non-provider-default upstream host must report RestartRequired")
	}
	if !strings.Contains(got.Reason, "custom.example.com") {
		t.Errorf("reason does not name the host: %q", got.Reason)
	}

	// It must still be selectable right now, for ordinary (non-CONNECT)
	// requests — the restart caveat is about CONNECT-mode only.
	if err := p.Pin("custom-1", false); err != nil {
		t.Fatalf("custom-upstream account is not selectable: %v", err)
	}
}

// TestAccountAddCoveredUpstreamNeedsNoRestart: EnableLiveMITM wired with a
// coverage predicate that says yes must NOT report RestartRequired — the
// ordinary case for any provider-default host (issue #87 point 2).
func TestAccountAddCoveredUpstreamNeedsNoRestart(t *testing.T) {
	p := pool.New(nil, time.Now())
	s := New(p, nil, events.New(), "")
	refreshed := false
	s.EnableLiveMITM(func(host string) bool { return true }, func() { refreshed = true })

	rec := postAccountAdd(t, s, `{"name":"kimi-1","type":"kimi-oauth","upstream":"https://api.kimi.com/coding","accessToken":"t"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got accountAddResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RestartRequired {
		t.Errorf("a covered host must not report RestartRequired: %q", got.Reason)
	}
	if !refreshed {
		t.Error("a successful Add must call the wired refreshHosts callback")
	}
}
