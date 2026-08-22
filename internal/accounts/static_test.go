package accounts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

// A static-key account (access token only, no refresh token, no expiry) must
// never be refresh-attempted nor disabled on that basis.
func TestStaticKeyNeverRefreshed(t *testing.T) {
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	m := testManager(t, srv.URL, "", nil)
	m.KimiAuthBase = srv.URL
	a := pool.NewAccount("static-kimi", pool.SourceYAML, "sk-static-key", "", 0, "")
	a.Type = "kimi-oauth"

	if !a.IsStaticKey() {
		t.Fatal("account should classify as static key")
	}
	if err := m.EnsureFresh(context.Background(), a); err != nil {
		t.Fatalf("EnsureFresh on static key: %v", err)
	}
	if refreshCalls.Load() != 0 {
		t.Errorf("refresh endpoint called %d times for a static key", refreshCalls.Load())
	}
	if a.State() != pool.StateOK {
		t.Errorf("state = %v, want OK", a.State())
	}
}

// A static key only dies when the upstream rejects it: Recover (401) fails,
// disables with a re-add message.
func TestStaticKeyRecoverDisablesWithReAddMessage(t *testing.T) {
	m := testManager(t, "http://unused", "", nil)
	a := pool.NewAccount("static-kimi", pool.SourceYAML, "sk-bad", "", 0, "")
	a.Type = "kimi-oauth"

	err := m.Recover(context.Background(), a)
	if err == nil || !strings.Contains(err.Error(), "re-add the key") {
		t.Errorf("err = %v, want re-add message", err)
	}
	if a.State() != pool.StateDisabled {
		t.Errorf("state = %v, want disabled after rejected static key", a.State())
	}
}

// Sanity: an oauth account WITH expiry but missing refresh token keeps the
// old semantics (disabled as unrefreshable).
func TestOAuthMissingRefreshStillDisables(t *testing.T) {
	m := testManager(t, "http://unused", "", nil)
	a := pool.NewAccount("oauth-broken", pool.SourceYAML, "tok", "", time.Now().Add(-time.Hour).UnixMilli(), "")
	if a.IsStaticKey() {
		t.Fatal("expired oauth account is not a static key")
	}
	if err := m.EnsureFresh(context.Background(), a); err == nil || !strings.Contains(err.Error(), "no refresh token") {
		t.Errorf("err = %v, want no-refresh-token", err)
	}
	if a.State() != pool.StateDisabled {
		t.Errorf("state = %v, want disabled", a.State())
	}
}
