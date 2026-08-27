package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

func postAccountRemove(t *testing.T, s *Server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7657/api/accounts/remove", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// TestAccountRemoveTakesAccountOutOfPool is the admin-layer half of issue
// #83's regression test: the CLI's `accounts remove` posts here after
// editing the config and deleting the credential, and the account must stop
// being selectable immediately — not just disappear from a list.
func TestAccountRemoveTakesAccountOutOfPool(t *testing.T) {
	a := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	b := pool.NewAccount("spare", pool.SourceYAML, "tok2", "", 0, "")
	b.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a, b}, time.Now())
	s := New(p, nil, events.New(), "")

	rec := postAccountRemove(t, s, `{"name":"work"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got accountRemoveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Removed {
		t.Error("response says removed=false for an account that existed")
	}
	for i := 0; i < 10; i++ {
		if sel := p.SelectFor("s", nil); sel != nil && sel.Name == "work" {
			t.Fatalf("removed account is still selectable")
		}
	}
}

// TestAccountRemoveUnknownNameIsNotAnError: idempotent by design — the CLI's
// goal ("this name must not be selectable") already holds for a name that
// was never there, or was already removed by an earlier call.
func TestAccountRemoveUnknownNameIsNotAnError(t *testing.T) {
	a := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	p := pool.New([]*pool.Account{a}, time.Now())
	s := New(p, nil, events.New(), "")

	rec := postAccountRemove(t, s, `{"name":"nope"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got accountRemoveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Removed {
		t.Error("response says removed=true for a name that was never present")
	}
}

func TestAccountRemoveRejectsMalformedBody(t *testing.T) {
	a := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	p := pool.New([]*pool.Account{a}, time.Now())
	s := New(p, nil, events.New(), "")

	if rec := postAccountRemove(t, s, `not json`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body: got %d, want 400", rec.Code)
	}
	if rec := postAccountRemove(t, s, `{"name":""}`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("empty name: got %d, want 400", rec.Code)
	}
}

// TestAccountRemoveMethodGuard: only POST is meaningful here; other methods
// must not silently fall through to a 200.
func TestAccountRemoveMethodGuard(t *testing.T) {
	a := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	p := pool.New([]*pool.Account{a}, time.Now())
	s := New(p, nil, events.New(), "")

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "http://127.0.0.1:7657/api/accounts/remove", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: got %d, want 405", method, rec.Code)
		}
	}
}

// TestAccountRemoveCSRF: the same cross-site write guard every other
// mutating endpoint sits behind — /api/accounts/remove must be no easier
// for another site to drive than /api/settings or /api/pin are.
func TestAccountRemoveCSRF(t *testing.T) {
	a := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	p := pool.New([]*pool.Account{a}, time.Now())
	s := New(p, nil, events.New(), "")

	blocked := []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://evil.example.com"},
	}
	for _, h := range blocked {
		if rec := postAccountRemove(t, s, `{"name":"work"}`, h); rec.Code != http.StatusForbidden {
			t.Errorf("headers %v: got %d, want 403", h, rec.Code)
		}
	}

	allowed := []map[string]string{
		{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:7657"},
		nil, // curl / the CLI
	}
	for _, h := range allowed {
		p2 := pool.New([]*pool.Account{pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")}, time.Now())
		s2 := New(p2, nil, events.New(), "")
		if rec := postAccountRemove(t, s2, `{"name":"work"}`, h); rec.Code != http.StatusOK {
			t.Errorf("headers %v: got %d (%s), want 200", h, rec.Code, rec.Body.String())
		}
	}
}

// TestSettingsPostReappliesFromDisk is issue #83's regression for
// priority/overage: the CLI writes those straight to the yaml (overage
// deliberately bypasses UpdateSettings — see config.SetAccountOverage's
// doc), so there is no new payload to PUT. POST with no body means "re-read
// what's on disk and apply it to the running pool now" — the same
// s.onSettings call PUT's tail end makes, just triggered without a rewrite.
func TestSettingsPostReappliesFromDisk(t *testing.T) {
	s, path := settingsServer(t)
	var applied *config.Config
	calls := 0
	s.onSettings = func(c *config.Config) { applied = c; calls++ }

	// Simulate the CLI: it already wrote the file directly (as
	// config.SetAccountPriority does) before calling this.
	if err := config.SetAccountPriority(path, "work", 7); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7657/api/settings", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("onSettings called %d times, want 1", calls)
	}
	if applied == nil || len(applied.Accounts) != 1 || applied.Accounts[0].Priority != 7 {
		t.Fatalf("onSettings did not see the on-disk priority change: %+v", applied)
	}
}

func TestSettingsPostCSRF(t *testing.T) {
	s, _ := settingsServer(t)

	post := func(headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7657/api/settings", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}

	if rec := post(map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST: got %d, want 403", rec.Code)
	}
	if rec := post(nil); rec.Code != http.StatusOK {
		t.Errorf("curl-shaped POST: got %d (%s), want 200", rec.Code, rec.Body.String())
	}
}

func TestSettingsMethodNotAllowedListsPost(t *testing.T) {
	s, _ := settingsServer(t)
	req := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:7657/api/settings", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, PUT, POST" {
		t.Errorf("Allow header = %q", allow)
	}
}
