package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRestartNotice drives restartNotice against a real admin listener (same
// httptest pattern as adminclient_test.go) rather than a hand-rolled fake, so
// it exercises the real HTTP/decode path a live daemon would produce.
func TestRestartNotice(t *testing.T) {
	cases := []struct {
		name string
		// body is what /api/accounts answers with. Ignored when unreachable
		// is set.
		body        string
		unreachable bool
		wantMessage bool
	}{
		{
			name:        "daemon reports the account disabled",
			body:        `[{"name":"work","state":"disabled"}]`,
			wantMessage: true,
		},
		{
			name:        "daemon reports the account fine",
			body:        `[{"name":"work","state":"ok"}]`,
			wantMessage: false,
		},
		{
			name:        "daemon unreachable",
			unreachable: true,
			wantMessage: false,
		},
		{
			name:        "daemon returns garbage",
			body:        `not json at all`,
			wantMessage: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var addr string
			if tc.unreachable {
				// Bind, learn the address, then close it before dialing —
				// nothing answers there any more, which is what "daemon
				// unreachable" means (as opposed to a token/auth failure).
				srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				addr = strings.TrimPrefix(srv.URL, "http://")
				srv.Close()
			} else {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.WriteString(w, tc.body)
				}))
				defer srv.Close()
				addr = strings.TrimPrefix(srv.URL, "http://")
			}
			writeCfg(t, addr)

			api, err := dialAdmin()
			if err != nil {
				t.Fatalf("dialAdmin: %v", err)
			}

			msg := restartNotice(api, "work")

			if tc.wantMessage {
				if msg == "" {
					t.Fatal("want a restart notice, got none")
				}
				if !strings.Contains(msg, "work") {
					t.Errorf("message doesn't name the account: %q", msg)
				}
				if !strings.Contains(msg, "spillway service install") {
					t.Errorf("message doesn't name `spillway service install`: %q", msg)
				}
				if !strings.Contains(msg, "spillway server") {
					t.Errorf("message doesn't cover the foreground-`spillway server` case: %q", msg)
				}
			} else if msg != "" {
				t.Errorf("want no message, got %q", msg)
			}
		})
	}
}

// A daemon that has never heard of this account (name mismatch, or the
// account was only just added to config) must not be mistaken for one
// reporting it disabled.
func TestRestartNoticeUnknownAccountIsSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"name":"someone-else","state":"disabled"}]`)
	}))
	defer srv.Close()
	writeCfg(t, strings.TrimPrefix(srv.URL, "http://"))

	api, err := dialAdmin()
	if err != nil {
		t.Fatal(err)
	}
	if msg := restartNotice(api, "work"); msg != "" {
		t.Errorf("want no message for an account the daemon never mentioned, got %q", msg)
	}
}
