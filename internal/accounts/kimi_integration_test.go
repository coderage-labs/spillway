package accounts

// The two Kimi tests that exercise this package's Manager and poller rather
// than the wire protocol. They live here because they need internal/pool,
// and pool imports provider — so keeping them next to the protocol code
// would be an import cycle.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

func TestManagerKimiRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("refresh_token") {
		case "good-refresh":
			fmt.Fprint(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":7200}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
		}
	}))
	defer srv.Close()
	m := testManager(t, "http://unused", "", nil)
	m.KimiAuthBase = srv.URL

	// Rotation: new access + new refresh persisted.
	a := pool.NewAccount("kimi-1", pool.SourceYAML, "old", "good-refresh", expiringSoon(), "")
	a.Type = "kimi-oauth"
	if err := m.EnsureFresh(context.Background(), a); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	access, refresh, _ := a.Credentials()
	if access != "rotated-access" || refresh != "rotated-refresh" {
		t.Errorf("credentials = (%q, %q)", access, refresh)
	}
	s, err := m.Secrets.Get("kimi-1")
	if err != nil || s.AccessToken != "rotated-access" || s.RefreshToken != "rotated-refresh" {
		t.Errorf("stored = %+v, %v", s, err)
	}

	// Dead grant: disabled, loudly.
	b := pool.NewAccount("kimi-2", pool.SourceYAML, "old", "dead", expiringSoon(), "")
	b.Type = "kimi-oauth"
	if err := m.EnsureFresh(context.Background(), b); !errors.Is(err, ErrRefreshDead) {
		t.Fatalf("err = %v, want ErrRefreshDead", err)
	}
	if b.State() != pool.StateDisabled {
		t.Errorf("state = %v, want disabled", b.State())
	}
}

func TestPollKimiUsagesStoresWindows(t *testing.T) {
	reset := time.Now().Add(time.Hour).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if a := r.Header.Get("Authorization"); a != "Bearer kimi-tok" {
			t.Errorf("Authorization = %q", a)
		}
		fmt.Fprintf(w, `[{"name":"5h","limit":100,"used":10,"reset_at":%d}]`, reset)
	}))
	defer srv.Close()

	a := pool.NewAccount("kimi-1", pool.SourceYAML, "kimi-tok", "", 0, srv.URL)
	a.Type = "kimi-oauth"
	p := pool.New([]*pool.Account{a}, time.Now())
	PollKimiUsages(context.Background(), p, slog.New(slog.NewTextHandler(io.Discard, nil)))

	windows := a.QuotaWindows()
	if len(windows) != 1 || windows[0].Name != "5h" || windows[0].Used != 10 {
		t.Fatalf("windows = %+v", windows)
	}
	got, ok := a.EarliestQuotaReset()
	if !ok || got.Unix() != reset {
		t.Errorf("EarliestQuotaReset = %v, %v", got, ok)
	}
}

// sanity: url.Values encoding used by the auth calls is real form encoding.
