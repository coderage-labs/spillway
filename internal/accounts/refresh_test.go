package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func tokenServer(t *testing.T, status int, body string, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Assert the request contract.
		if r.Method != http.MethodPost {
			t.Errorf("method = %q", r.Method)
		}
		var req map[string]string
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			t.Errorf("request body not JSON: %v", err)
		}
		if req["grant_type"] != "refresh_token" {
			t.Errorf("grant_type = %q", req["grant_type"])
		}
		if req["client_id"] != claudeClientID {
			t.Errorf("client_id = %q", req["client_id"])
		}
		if req["refresh_token"] == "" {
			t.Error("refresh_token empty")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
}

func TestRefreshSuccess(t *testing.T) {
	var calls atomic.Int32
	srv := tokenServer(t, 200, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`, &calls)
	defer srv.Close()

	r := &Refresher{TokenURL: srv.URL}
	res, err := r.Refresh(context.Background(), "old-refresh")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.AccessToken != "new-access" || res.RefreshToken != "new-refresh" {
		t.Errorf("result = %+v", res)
	}
	wantExp := time.Now().UnixMilli() + 3600*1000
	if d := res.ExpiresAt - wantExp; d < -5000 || d > 5000 {
		t.Errorf("expiresAt = %d, want ~%d", res.ExpiresAt, wantExp)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestRefreshKeepsOldRefreshTokenWhenOmitted(t *testing.T) {
	var calls atomic.Int32
	srv := tokenServer(t, 200, `{"access_token":"a","expires_in":60}`, &calls)
	defer srv.Close()
	r := &Refresher{TokenURL: srv.URL}
	res, err := r.Refresh(context.Background(), "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if res.RefreshToken != "" {
		t.Errorf("RefreshToken = %q, want empty (caller keeps old)", res.RefreshToken)
	}
}

func TestRefreshExpiryNormalization(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{"expires_at seconds epoch", `{"access_token":"a","expires_at":4102444800}`, 4102444800000},
		{"expires_at ms epoch", `{"access_token":"a","expires_at":4102444800000}`, 4102444800000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := tokenServer(t, 200, tc.body, &calls)
			defer srv.Close()
			r := &Refresher{TokenURL: srv.URL}
			res, err := r.Refresh(context.Background(), "rt")
			if err != nil {
				t.Fatal(err)
			}
			if res.ExpiresAt != tc.want {
				t.Errorf("expiresAt = %d, want %d", res.ExpiresAt, tc.want)
			}
		})
	}
}

func TestRefreshDeadOn400And401(t *testing.T) {
	for _, status := range []int{400, 401} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := tokenServer(t, status, `{"error":"invalid_grant"}`, &calls)
			defer srv.Close()
			r := &Refresher{TokenURL: srv.URL}
			_, err := r.Refresh(context.Background(), "rt")
			if !errors.Is(err, ErrRefreshDead) {
				t.Errorf("err = %v, want ErrRefreshDead", err)
			}
			if calls.Load() != 1 {
				t.Errorf("calls = %d, want 1 (no retry on dead token)", calls.Load())
			}
		})
	}
}

func TestRefreshRetriesOnceOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"a","expires_in":60}`)
	}))
	defer srv.Close()

	r := &Refresher{TokenURL: srv.URL}
	res, err := r.Refresh(context.Background(), "rt")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.AccessToken != "a" {
		t.Errorf("result = %+v", res)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (one retry)", calls.Load())
	}
}

func TestRefresh5xxTwiceFails(t *testing.T) {
	var calls atomic.Int32
	srv := tokenServer(t, 502, ``, &calls)
	defer srv.Close()
	r := &Refresher{TokenURL: srv.URL}
	_, err := r.Refresh(context.Background(), "rt")
	if err == nil || errors.Is(err, ErrRefreshDead) || !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %v, want transient 502 error", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}
