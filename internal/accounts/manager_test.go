package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/notify"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/secrets"

	"github.com/coderage-labs/spillway/internal/testmode"
)

func testManager(t *testing.T, tokenURL, cfgPath string, keychain Source) *Manager {
	t.Helper()
	return &Manager{
		Refresher:  &Refresher{TokenURL: tokenURL},
		Keychain:   keychain,
		Secrets:    secrets.NewFake(),
		ConfigPath: cfgPath,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		inflight:   map[*pool.Account]*refreshCall{},
	}
}

const managerConfigYAML = `proxy:
  port: 7654
  host: 127.0.0.1
upstream: https://api.anthropic.com
accounts:
  - name: work
    type: claude-oauth
    expiresAt: 1
log:
  level: info
`

func expiringSoon() int64 { return time.Now().Add(time.Minute).UnixMilli() }

func TestEnsureFreshNoopWhenNotNearExpiry(t *testing.T) {
	m := testManager(t, "http://unused", "", nil)
	a := pool.NewAccount("work", pool.SourceYAML, "tok", "rt", time.Now().Add(time.Hour).UnixMilli(), "")
	if err := m.EnsureFresh(context.Background(), a); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
}

func TestRefreshWritesBackToSecretsAndConfig(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_at":4102444800}`)
	}))
	defer srv.Close()

	cfgPath := filepath.Join(t.TempDir(), "spillway.yaml")
	if err := os.WriteFile(cfgPath, []byte(managerConfigYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	m := testManager(t, srv.URL, cfgPath, nil)
	a := pool.NewAccount("work", pool.SourceYAML, "old-access", "old-refresh", expiringSoon(), "")

	if err := m.EnsureFresh(context.Background(), a); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}

	access, refresh, exp := a.Credentials()
	if access != "fresh-access" || refresh != "fresh-refresh" || exp != 4102444800000 {
		t.Errorf("credentials = (%q, %q, %d)", access, refresh, exp)
	}

	// Tokens persisted to the secret store, not the yaml.
	s, err := m.Secrets.Get("work")
	if err != nil {
		t.Fatalf("Secrets.Get: %v", err)
	}
	if s.AccessToken != "fresh-access" || s.RefreshToken != "fresh-refresh" {
		t.Errorf("stored secrets = %+v", s)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "4102444800000") {
		t.Errorf("config missing refreshed expiry:\n%s", data)
	}
	for _, tok := range []string{"fresh-access", "fresh-refresh", "accessToken", "refreshToken"} {
		if strings.Contains(string(data), tok) {
			t.Errorf("config contains token material %q:\n%s", tok, data)
		}
	}
	testmode.AssertPrivateFile(t, cfgPath)
}

func TestRefreshDeadDisablesAccount(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	}))
	defer srv.Close()

	m := testManager(t, srv.URL, "", nil)
	a := pool.NewAccount("work", pool.SourceYAML, "old", "dead-refresh", expiringSoon(), "")
	err := m.EnsureFresh(context.Background(), a)
	if err == nil {
		t.Fatal("expected error")
	}
	if a.State() != pool.StateDisabled {
		t.Errorf("state = %v, want disabled", a.State())
	}
}

// Disabling an account (issue #101) must raise the "account-disabled"
// event, and it must reach an actually-configured channel — this wires a
// real webhook provider at an httptest server through the exported
// notify.New/SetChannels API, the same path buildNotifier uses.
func TestDisableNotifiesAccountDisabled(t *testing.T) {
	type payload struct {
		Event string `json:"event"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	received := make(chan payload, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p payload
		_ = json.NewDecoder(r.Body).Decode(&p)
		received <- p
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := notify.New()
	warnings := n.SetChannels(
		[]notify.ChannelSpec{{Name: "test", Provider: "webhook", Events: []string{notify.EventAccountDisabled}}},
		func(string) (notify.Destination, error) { return notify.Destination{URL: srv.URL}, nil },
	)
	if len(warnings) != 0 {
		t.Fatalf("SetChannels warnings = %v, want none", warnings)
	}

	m := testManager(t, "http://unused", "", nil)
	m.Notifier = n
	// No refresh token and not a static key: refreshOne's "no refresh
	// token, disabling" path.
	a := pool.NewAccount("work", pool.SourceYAML, "old", "", expiringSoon(), "")

	if err := m.EnsureFresh(context.Background(), a); err == nil {
		t.Fatal("expected an error disabling an account with no refresh token")
	}
	if a.State() != pool.StateDisabled {
		t.Fatalf("state = %v, want disabled", a.State())
	}

	select {
	case p := <-received:
		if p.Event != notify.EventAccountDisabled {
			t.Errorf("event = %q, want %q", p.Event, notify.EventAccountDisabled)
		}
		if !strings.Contains(p.Body, "work") {
			t.Errorf("body does not name the account: %q", p.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the account-disabled webhook")
	}
}

// A Manager with no Notifier wired (the common case in every other test in
// this file) must not panic when it disables an account.
func TestDisableWithNoNotifierDoesNotPanic(t *testing.T) {
	m := testManager(t, "http://unused", "", nil)
	a := pool.NewAccount("work", pool.SourceYAML, "old", "", expiringSoon(), "")
	if err := m.EnsureFresh(context.Background(), a); err == nil {
		t.Fatal("expected an error disabling an account with no refresh token")
	}
}

func TestSingleflightCollapses(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		<-release // hold the first refresh open so waiters pile up
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"fresh","expires_in":3600}`)
	}))
	defer srv.Close()

	m := testManager(t, srv.URL, "", nil)
	a := pool.NewAccount("work", pool.SourceYAML, "old", "rt", expiringSoon(), "")

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- m.EnsureFresh(context.Background(), a)
		}()
	}
	// Let the first caller reach the endpoint and the rest queue behind it.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("EnsureFresh: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("refresh HTTP calls = %d, want 1 (singleflight)", got)
	}
}

func TestKeychainReload(t *testing.T) {
	fresh := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"reloaded-access","refreshToken":"reloaded-refresh","expiresAt":%d,"scopes":["user:inference"],"subscriptionType":"max"}}`,
		time.Now().Add(time.Hour).UnixMilli())
	var loads atomic.Int32
	src := SourceFunc(func() ([]byte, error) { loads.Add(1); return []byte(fresh), nil })

	m := testManager(t, "http://unused", "", src)
	a := pool.NewAccount("local", pool.SourceKeychain, "stale", "stale-rt", expiringSoon(), "")

	if err := m.EnsureFresh(context.Background(), a); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	access, refresh, _ := a.Credentials()
	if access != "reloaded-access" || refresh != "reloaded-refresh" {
		t.Errorf("credentials = (%q, %q)", access, refresh)
	}
	if loads.Load() != 1 {
		t.Errorf("keychain loads = %d, want 1", loads.Load())
	}
}

func TestKeychainReloadStillExpiredDisables(t *testing.T) {
	stale := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"still-old","expiresAt":%d}}`,
		time.Now().Add(-time.Hour).UnixMilli())
	src := SourceFunc(func() ([]byte, error) { return []byte(stale), nil })

	m := testManager(t, "http://unused", "", src)
	a := pool.NewAccount("local", pool.SourceKeychain, "stale", "stale-rt", expiringSoon(), "")

	if err := m.EnsureFresh(context.Background(), a); err == nil {
		t.Fatal("expected error")
	}
	if a.State() != pool.StateDisabled {
		t.Errorf("state = %v, want disabled", a.State())
	}
}
