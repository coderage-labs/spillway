package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
)

// fakeManager implements pool.TokenManager for recovery tests.
type fakeManager struct {
	recoverCalls atomic.Int32
	recover      func(a *pool.Account) error
}

func (f *fakeManager) EnsureFresh(ctx context.Context, a *pool.Account) error { return nil }

func (f *fakeManager) Recover(ctx context.Context, a *pool.Account) error {
	f.recoverCalls.Add(1)
	return f.recover(a)
}

// On upstream 401 pre-first-byte: one recovery, one retry with the fresh
// token; the client sees only the final 200.
func Test401RecoveryRefreshesAndRetries(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			if a := r.Header.Get("Authorization"); a != "Bearer stale-tok" {
				t.Errorf("first attempt Authorization = %q", a)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"expired"}}`)
		default:
			if a := r.Header.Get("Authorization"); a != "Bearer fresh-tok" {
				t.Errorf("retry Authorization = %q, want fresh token", a)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer upstream.Close()

	acct := pool.NewAccount("a", pool.SourceYAML, "stale-tok", "rt", 0, upstream.URL)
	p := pool.New([]*pool.Account{acct}, time.Now())
	fm := &fakeManager{recover: func(a *pool.Account) error {
		a.SetCredentials("fresh-tok", "rt", 0)
		return nil
	}}
	p.SetTokenManager(fm)

	cfg := config.Defaults()
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if fm.recoverCalls.Load() != 1 {
		t.Errorf("recover calls = %d, want 1", fm.recoverCalls.Load())
	}
	if calls.Load() != 2 {
		t.Errorf("upstream hits = %d, want 2 (401 + retry)", calls.Load())
	}
}

// A second 401 after recovery passes through untouched.
func Test401SecondFailurePassesThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-401")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"nope"}}`)
	}))
	defer upstream.Close()

	acct := pool.NewAccount("a", pool.SourceYAML, "tok", "rt", 0, upstream.URL)
	p := pool.New([]*pool.Account{acct}, time.Now())
	fm := &fakeManager{recover: func(a *pool.Account) error { return nil }}
	p.SetTokenManager(fm)

	cfg := config.Defaults()
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 passed through", resp.StatusCode)
	}
	if resp.Header.Get("X-Request-Id") != "req-401" {
		t.Errorf("upstream headers not preserved on 401 pass-through")
	}
	if fm.recoverCalls.Load() != 1 {
		t.Errorf("recover calls = %d, want exactly 1", fm.recoverCalls.Load())
	}
}

// Dead credential (recovery fails, account disabled) rotates to the next
// account when one is available.
func Test401DeadCredentialRotates(t *testing.T) {
	var aHits, bHits atomic.Int32
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"dead"}`)
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits.Add(1)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upB.Close()

	acctA := pool.NewAccount("a", pool.SourceYAML, "tok-a", "rt", 0, upA.URL)
	acctB := pool.NewAccount("b", pool.SourceYAML, "tok-b", "rt", 0, upB.URL)
	p := pool.New([]*pool.Account{acctA, acctB}, time.Now())
	fm := &fakeManager{recover: func(a *pool.Account) error {
		a.Disable()
		return errors.New("refresh token dead")
	}}
	p.SetTokenManager(fm)

	cfg := config.Defaults()
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via rotation", resp.StatusCode)
	}
	if aHits.Load() != 1 || bHits.Load() != 1 {
		t.Errorf("hits = (%d, %d), want (1, 1)", aHits.Load(), bHits.Load())
	}
	if acctA.State() != pool.StateDisabled {
		t.Errorf("account A state = %v, want disabled", acctA.State())
	}
}
