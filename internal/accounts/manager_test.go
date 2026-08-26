package accounts

import (
	"context"
	"errors"
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

// TestKeychainReloadFailsFastOnUnchangedExpiredCredential is #81's fail-fast
// requirement. The reported failure was a keychain entry expired at
// 02:21:28 being reloaded once a minute for an hour — 60 attempts that could
// not possibly succeed — before spillway finally disabled the account and
// printed the one useful line. Simulating that same once-a-minute sweep
// directly against the Manager (EnsureFresh does not itself check whether
// the account is already disabled — every real caller happens to, which is
// exactly the kind of scattered, easy-to-miss guard #81's own comment warns
// about) must not touch the keychain more than once.
func TestKeychainReloadFailsFastOnUnchangedExpiredCredential(t *testing.T) {
	stale := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"still-old","expiresAt":%d}}`,
		time.Now().Add(-time.Hour).UnixMilli())
	var loads atomic.Int32
	src := SourceFunc(func() ([]byte, error) { loads.Add(1); return []byte(stale), nil })

	m := testManager(t, "http://unused", "", src)
	a := pool.NewAccount("local", pool.SourceKeychain, "stale", "stale-rt", expiringSoon(), "")

	// 60 ticks of the real sweep interval (main.go's refreshSweepInterval is
	// a minute), simulated back to back.
	for i := 0; i < 60; i++ {
		err := m.EnsureFresh(context.Background(), a)
		if err == nil {
			t.Fatalf("tick %d: expected error", i)
		}
		if i > 0 && !errors.Is(err, errKeychainAlreadyDisabled) {
			t.Fatalf("tick %d: err = %v, want wrapping errKeychainAlreadyDisabled once already disabled", i, err)
		}
	}
	if got := loads.Load(); got != 1 {
		t.Errorf("keychain loads = %d, want 1 (a regression here is the exact #81 bug — 60 futile reloads)", got)
	}
	if a.State() != pool.StateDisabled {
		t.Errorf("state = %v, want disabled", a.State())
	}
}

// TestKeychainReloadPicksUpGenuineReloginAfterRestart guards the other side
// of the fail-fast fix: it must not turn into a permanent latch that ignores
// a real re-login. Once an account is disabled, nothing here revives it
// in-process — issue #81 explicitly defers "should disabled become
// recoverable" to a separate decision — but the account is expected to come
// back the way #81 says it already does: a daemon restart, which
// reconstructs pool.Account fresh (pool.New treats every keychain account as
// eligible regardless of its last known expiry, since CanRefresh is
// unconditionally true for SourceKeychain) and reloads for real. This test
// stands in for that restart with a second, independent Account sharing the
// same (now fixed) keychain source.
func TestKeychainReloadPicksUpGenuineReloginAfterRestart(t *testing.T) {
	now := time.Now()
	var raw atomic.Value
	raw.Store(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"still-old","expiresAt":%d}}`,
		now.Add(-time.Hour).UnixMilli()))
	src := SourceFunc(func() ([]byte, error) { return []byte(raw.Load().(string)), nil })

	m := testManager(t, "http://unused", "", src)
	dead := pool.NewAccount("local", pool.SourceKeychain, "stale", "stale-rt", expiringSoon(), "")
	if err := m.EnsureFresh(context.Background(), dead); err == nil {
		t.Fatal("expected the expired credential to fail")
	}
	if dead.State() != pool.StateDisabled {
		t.Fatalf("state = %v, want disabled", dead.State())
	}

	// The user re-logs in via `claude`; the keychain item is now fresh. The
	// still-disabled Account object must not un-latch on its own (out of
	// scope for #81).
	if err := m.EnsureFresh(context.Background(), dead); !errors.Is(err, errKeychainAlreadyDisabled) {
		t.Errorf("existing disabled Account err = %v, want errKeychainAlreadyDisabled (no auto-revive)", err)
	}

	// A restart, standing in as a freshly constructed Account for the same
	// account name/source, must pick the new credential up normally.
	raw.Store(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"new-access","refreshToken":"new-refresh","expiresAt":%d}}`,
		now.Add(time.Hour).UnixMilli()))
	revived := pool.NewAccount("local", pool.SourceKeychain, "stale", "stale-rt", expiringSoon(), "")
	if err := m.EnsureFresh(context.Background(), revived); err != nil {
		t.Fatalf("EnsureFresh on the post-restart account: %v", err)
	}
	access, refresh, _ := revived.Credentials()
	if access != "new-access" || refresh != "new-refresh" {
		t.Errorf("credentials = (%q, %q), want the freshly re-logged-in ones", access, refresh)
	}
	if revived.State() != pool.StateOK {
		t.Errorf("state = %v, want OK", revived.State())
	}
}
