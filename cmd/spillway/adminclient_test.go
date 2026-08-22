package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeCfg points SPILLWAY_CONFIG at a config naming addr, and returns its
// directory so a test can drop a token file beside it.
func writeCfg(t *testing.T, addr string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "spillway.yaml")
	if err := os.WriteFile(path, []byte("admin:\n  addr: "+addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPILLWAY_CONFIG", path)
	return dir
}

func TestStatusJSONCollectsAllThreeViews(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RequestURI())
		switch r.URL.Path {
		case "/api/state":
			io.WriteString(w, `{"usable":2,"total":3}`)
		case "/api/accounts":
			io.WriteString(w, `[{"name":"a"}]`)
		case "/api/requests":
			io.WriteString(w, `[{"event":"served"}]`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()
	writeCfg(t, strings.TrimPrefix(srv.URL, "http://"))

	api, err := dialAdmin()
	if err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := statusJSON(api); err != nil {
			t.Fatal(err)
		}
	})

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	for _, k := range []string{"state", "accounts", "requests"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %q; got keys %v", k, keysOf(got))
		}
	}
	// The requests view is bounded — the plugin feeds this to a model.
	if len(seen) != 3 || !strings.Contains(seen[2], "limit=") {
		t.Errorf("requests fetched unbounded: %v", seen)
	}
}

// A unix admin listener has to be dialled as a socket. "http://"+addr made a
// nonsense URL out of a path, so `spillway status` could not reach a daemon
// configured this way at all.
func TestDialAdminReachesAUnixListener(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are refused on windows by admin.Listen")
	}
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"usable":1}`)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	writeCfg(t, sock)

	api, err := dialAdmin()
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Usable int `json:"usable"`
	}
	if err := api.get("/api/state", &got); err != nil {
		t.Fatalf("unix admin listener unreachable: %v", err)
	}
	if got.Usable != 1 {
		t.Errorf("usable = %d, want 1", got.Usable)
	}
}

// An admin token beside the config must be presented; without it a
// token-protected daemon answers 401 and status reports "unreachable".
func TestAdminTokenIsSent(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	dir := writeCfg(t, strings.TrimPrefix(srv.URL, "http://"))
	if err := os.WriteFile(filepath.Join(dir, "spillway-admin-token"), []byte("sekrit\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	api, err := dialAdmin()
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := api.get("/api/state", &v); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sekrit" {
		t.Errorf("Authorization = %q, want the trimmed token", auth)
	}
}

func keysOf(m map[string]any) []string {
	var k []string
	for s := range m {
		k = append(k, s)
	}
	return k
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = old
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}
