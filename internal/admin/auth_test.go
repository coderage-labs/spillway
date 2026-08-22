package admin

import (
	"fmt"
	"github.com/coderage-labs/spillway/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

func serverWithToken(t *testing.T, token string) *Server {
	t.Helper()
	a := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, time.Now())
	return New(p, nil, events.New(), token)
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7657"+path, nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// A loopback listener is unauthenticated by design: the token would only stop
// processes running as another user, while putting a secret in every URL.
func TestNoTokenServesWhenNotRequired(t *testing.T) {
	s := serverWithToken(t, "")
	if rec := get(t, s, "/api/accounts"); rec.Code != http.StatusOK {
		t.Fatalf("loopback request without a token: got %d, want 200", rec.Code)
	}
}

// Off-loopback the token is mandatory, and a missing one must fail CLOSED —
// a misconfiguration should never silently expose the dashboard.
func TestRequireTokenWithoutTokenFailsClosed(t *testing.T) {
	s := serverWithToken(t, "")
	s.RequireToken()
	if rec := get(t, s, "/api/accounts"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("required-but-absent token: got %d, want 401", rec.Code)
	}
}

func TestRequireTokenAcceptsCorrectRejectsWrong(t *testing.T) {
	s := serverWithToken(t, "sekrit")

	if rec := get(t, s, "/api/accounts"); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", rec.Code)
	}
	if rec := get(t, s, "/api/accounts?token=wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", rec.Code)
	}
	if rec := get(t, s, "/api/accounts?token=sekrit"); rec.Code != http.StatusOK {
		t.Errorf("correct token: got %d, want 200", rec.Code)
	}
}

// Dropping the token must not drop the guards that actually stop browser
// attacks: rebinding is caught by the Host check, framing by the header.
func TestHostGuardStillAppliesWithoutToken(t *testing.T) {
	s := serverWithToken(t, "")
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7657/api/accounts", nil)
	req.Host = "evil.example.com"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("rebinding Host: got %d, want 403", rec.Code)
	}
}

func TestFramingHeaders(t *testing.T) {
	s := serverWithToken(t, "")
	rec := get(t, s, "/api/accounts")
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options: got %q, want DENY", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy: got %q, want no-referrer", got)
	}
}

func TestIsLoopback(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:7657", true},
		{"localhost:7657", true},
		{"[::1]:7657", true},
		{"127.0.0.2:7657", true}, // whole 127/8 is loopback
		{"0.0.0.0:7657", false},  // binds every interface
		{"192.168.1.10:7657", false},
		{"example.com:7657", false},
	} {
		if got := IsLoopback(tc.addr); got != tc.want {
			t.Errorf("IsLoopback(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// Read-only endpoints must reject other methods rather than falling through
// to the GET handler and answering 200.
func TestReadOnlyEndpointsRejectNonGET(t *testing.T) {
	s := serverWithToken(t, "")
	for _, path := range []string{"/api/accounts", "/api/requests", "/api/quota-history", "/api/activity", "/"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			req := httptest.NewRequest(method, "http://127.0.0.1:7657"+path, nil)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: got %d, want 405", method, path, rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
				t.Errorf("%s %s: Allow header %q", method, path, allow)
			}
		}
	}
	// HEAD stays allowed alongside GET.
	req := httptest.NewRequest(http.MethodHead, "http://127.0.0.1:7657/api/accounts", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD /api/accounts: got %d, want 200", rec.Code)
	}
}

func settingsServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "spillway.yaml")
	if err := os.WriteFile(p, []byte(`proxy:
  port: 7654
  host: 127.0.0.1
upstream: https://api.anthropic.com
pool:
  exhaustedMode: notify
  holdMax: 4h
  switchThreshold: 0.98
log:
  level: info
accounts:
  - name: work
    type: claude-oauth
`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := serverWithToken(t, "")
	s.EnableSettings(p, func(*config.Config) {})
	return s, p
}

func put(t *testing.T, s *Server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:7657/api/settings",
		strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// The CSRF guard has its first mutating endpoint behind it. Another site
// driving the user's browser at our loopback port must be refused — but the
// dashboard is itself a browser page, so its own writes must go through.
func TestSettingsCSRF(t *testing.T) {
	s, _ := settingsServer(t)

	blocked := []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"},
		{"Origin": "https://evil.example.com"},
		{"Sec-Fetch-Site": "same-origin", "Origin": "https://evil.example.com"},
	}
	for _, h := range blocked {
		if rec := put(t, s, `{"exhaustedMode":"fail"}`, h); rec.Code != http.StatusForbidden {
			t.Errorf("headers %v: got %d, want 403", h, rec.Code)
		}
	}

	allowed := []map[string]string{
		{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:7657"}, // the dashboard
		{"Sec-Fetch-Site": "none"}, // address-bar navigation
		nil,                        // curl / the CLI
	}
	for _, h := range allowed {
		if rec := put(t, s, `{"exhaustedMode":"fail"}`, h); rec.Code != http.StatusOK {
			t.Errorf("headers %v: got %d (%s), want 200", h, rec.Code, rec.Body.String())
		}
	}
}

func TestSettingsPutAppliesAndReturnsCurrent(t *testing.T) {
	s, path := settingsServer(t)
	rec := put(t, s, `{"exhaustedMode":"fail"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pool.ExhaustedMode != "fail" {
		t.Errorf("not persisted: %q", cfg.Pool.ExhaustedMode)
	}
	if !strings.Contains(rec.Body.String(), "fail") {
		t.Errorf("response should echo current settings: %s", rec.Body.String())
	}
}

// A rejected value is the user's typo, not a server fault.
func TestSettingsPutRejectsBadValueAs400(t *testing.T) {
	s, path := settingsServer(t)
	before, _ := os.ReadFile(path)
	rec := put(t, s, `{"holdMax":"4 hours"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "holdMax") {
		t.Errorf("error should name the field: %s", rec.Body.String())
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("config written despite rejection")
	}
}

// Credentials are not part of the editable surface and must be ignored
// outright rather than quietly accepted.
func TestSettingsIgnoresCredentialFields(t *testing.T) {
	s, path := settingsServer(t)
	rec := put(t, s, `{"accounts":{"work":{"label":"x","accessToken":"stolen","refreshToken":"stolen"}}}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "stolen") {
		t.Fatal("a credential field supplied over HTTP reached the config file")
	}
}

// Read-only endpoints keep their 405 now that the blanket check is per-route.
func TestOtherEndpointsStillRejectNonGET(t *testing.T) {
	s, _ := settingsServer(t)
	req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:7657/api/accounts", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/accounts: got %d, want 405", rec.Code)
	}
}

func TestIsUnixAndSocketPath(t *testing.T) {
	for addr, want := range map[string]bool{
		"/tmp/spillway.sock":        true,
		"unix:///tmp/spillway.sock": true,
		"~/.config/spillway.sock":   true,
		"./relative.sock":           true,
		"127.0.0.1:7657":            false,
		"localhost:7657":            false,
	} {
		if got := IsUnix(addr); got != want {
			t.Errorf("IsUnix(%q) = %v, want %v", addr, got, want)
		}
	}
	if got := SocketPath("unix:///tmp/x.sock"); got != "/tmp/x.sock" {
		t.Errorf("SocketPath stripped wrongly: %q", got)
	}
}

// The socket's file mode IS the access control, so it must be 0600 — a
// world-writable socket would be strictly worse than the TCP listener.
func TestListenUnixCreatesPrivateSocket(t *testing.T) {
	requireUnix(t)
	path := shortSocket(t)
	ln, desc, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if desc != path {
		t.Errorf("description = %q, want the socket path", desc)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600 — the mode is the access control", perm)
	}
}

// An unclean shutdown leaves a socket file behind; the next start must not be
// blocked by it.
func TestListenUnixReplacesStaleSocket(t *testing.T) {
	requireUnix(t)
	path := shortSocket(t)
	first, _, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	first.Close() // closes the listener but may leave the file

	second, _, err := Listen(path)
	if err != nil {
		t.Fatalf("a stale socket should not block startup: %v", err)
	}
	second.Close()
}

// But a LIVE socket must not be stolen — that would half-kill a running
// daemon rather than reporting the conflict.
func TestListenUnixRefusesWhenAlreadyServing(t *testing.T) {
	requireUnix(t)
	path := shortSocket(t)
	ln, _, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	if _, _, err := Listen(path); err == nil {
		t.Fatal("stole a socket another instance was serving")
	}
}

// shortSocket returns a path within the kernel's sun_path limit. Go's
// t.TempDir() is far too long on darwin, where the cap is 104 bytes.
func shortSocket(t *testing.T) string {
	t.Helper()
	p := filepath.Join(os.TempDir(), fmt.Sprintf("sw-%d.sock", os.Getpid()+int(time.Now().UnixNano()%9999)))
	t.Cleanup(func() { os.Remove(p) })
	return p
}

// An over-long path must say what is wrong; the kernel only says "invalid
// argument", which sends people looking at permissions.
func TestListenUnixRejectsOverlongPath(t *testing.T) {
	requireUnix(t)
	long := filepath.Join(os.TempDir(), strings.Repeat("d", 120)+".sock")
	_, _, err := Listen(long)
	if err == nil {
		t.Fatal("expected an over-long socket path to be rejected")
	}
	for _, want := range []string{"characters", "shorter path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// requireUnix skips a socket test where the feature does not exist.
//
// Not a platform quirk being papered over: Listen refuses a unix address on
// Windows outright, because the socket's protection is mode 0600 and Chmod
// there only toggles a read-only bit. TestListenRefusesUnixOnWindows covers
// that refusal; these four cover the behaviour it replaces.
func requireUnix(t *testing.T) {
	t.Helper()
	if !UnixSupported {
		t.Skip("unix-socket admin listener is not supported on this platform")
	}
}

// The refusal must be explicit and must say what to do instead. Silently
// falling back to TCP would leave someone believing the admin API was
// restricted to their user when it was listening on a port.
func TestListenRefusesUnixOnWindows(t *testing.T) {
	if UnixSupported {
		t.Skip("unix sockets are supported here")
	}
	_, _, err := Listen("unix:///tmp/spillway.sock")
	if err == nil {
		t.Fatal("a unix address was accepted on a platform that cannot protect the socket")
	}
	for _, want := range []string{"not supported", "0600", "admin.token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}
