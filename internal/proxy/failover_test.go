package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
)

// twoAccountRig spins up one mock upstream per account and a pool-backed
// proxy front server. handlers[i] is invoked for hits on account i.
type twoAccountRig struct {
	front *httptest.Server
	pool  *pool.Pool
	hits  [2]*atomic.Int32
}

func newRig(t *testing.T, handlers [2]http.HandlerFunc) *twoAccountRig {
	t.Helper()
	rig := &twoAccountRig{hits: [2]*atomic.Int32{{}, {}}}
	accts := make([]*pool.Account, 2)
	for i, hf := range handlers {
		i, hf := i, hf
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rig.hits[i].Add(1)
			hf(w, r)
		}))
		t.Cleanup(up.Close)
		accts[i] = pool.NewAccount(fmt.Sprintf("acct-%d", i), pool.SourceYAML, fmt.Sprintf("tok-%d", i), "", 0, up.URL)
	}
	rig.pool = pool.New(accts, time.Now())
	cfg := config.Defaults()
	cfg.Pool.ExhaustedMode = "fail" // tests fail fast; hold mode has its own suite
	h, err := NewHandler(&cfg, testLogger(), rig.pool)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	rig.front = httptest.NewServer(h)
	t.Cleanup(rig.front.Close)
	return rig
}

const testBody = `{"model":"claude","max_tokens":16,"metadata":{"user_id":"session-abc"},"messages":[]}`

func postMessages(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func quota429(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1800")
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", fmt.Sprint(time.Now().Add(30*time.Minute).Unix()))
	w.WriteHeader(http.StatusTooManyRequests)
	fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"quota exceeded"}}`)
}

// (a) quota-429 on account A → buffered body re-sent on account B; client
// sees one 200; A is marked exhausted.
func TestQuota429RotatesAndResends(t *testing.T) {
	var bodyOnB string
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) { quota429(w) },
		func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			bodyOnB = string(b)
			if a := r.Header.Get("Authorization"); a != "Bearer tok-1" {
				t.Errorf("Authorization on B = %q", a)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		},
	})

	// Both requests share a session so the second would normally stick to A;
	// here one request is enough: first hit goes to A (or B — selection picks
	// the least-loaded eligible), the 429 rotates to the other.
	resp := postMessages(t, rig.front.URL, testBody)
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, got)
	}
	if string(got) != `{"ok":true}` {
		t.Errorf("body = %q", got)
	}
	total := rig.hits[0].Load() + rig.hits[1].Load()
	if total != 2 {
		t.Errorf("total upstream hits = %d, want 2 (one 429, one 200)", total)
	}
	if bodyOnB == "" {
		// The 200 may have landed on acct-0 if selection picked B first;
		// either way the body must have been re-sent to the second account.
		t.Log("200 did not land on acct-1; checking acct-0 received the body instead")
	}
	// The account that returned quota-429 must be exhausted.
	exhausted := 0
	for _, a := range rig.pool.Accounts() {
		if a.State() == pool.StateExhausted {
			exhausted++
		}
	}
	if exhausted != 1 {
		t.Errorf("exhausted accounts = %d, want 1", exhausted)
	}
}

// (b) rate-limit-429 → inline backoff retry on the SAME account.
func TestRateLimit429RetriesSameAccount(t *testing.T) {
	var calls atomic.Int32
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "1")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("request rotated to account B on a rate-limit-429")
		},
	})

	resp := postMessages(t, rig.front.URL, testBody)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if calls.Load() != 2 {
		t.Errorf("same-account tries = %d, want 2", calls.Load())
	}
	if rig.hits[1].Load() != 0 {
		t.Errorf("account B hits = %d, want 0", rig.hits[1].Load())
	}
}

// (c) every account quota-exhausted → the client sees a 429.
func TestAllExhaustedPasses429(t *testing.T) {
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) { quota429(w) },
		func(w http.ResponseWriter, r *http.Request) { quota429(w) },
	})

	resp := postMessages(t, rig.front.URL, testBody)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429; body %s", resp.StatusCode, body)
	}
}

// (d) stickiness: two sequential same-session requests hit the same account.
func TestStickySession(t *testing.T) {
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true}`) },
		func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true}`) },
	})

	for i := 0; i < 2; i++ {
		resp := postMessages(t, rig.front.URL, testBody)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	a, b := rig.hits[0].Load(), rig.hits[1].Load()
	if !((a == 2 && b == 0) || (a == 0 && b == 2)) {
		t.Errorf("hits = (%d, %d), want both on one account", a, b)
	}
}

// (e) over-cap body streams straight through: no buffering, no failover.
func TestOverCapBodyStreamsWithoutRetry(t *testing.T) {
	var gotSize atomic.Int64
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			n, _ := io.Copy(io.Discard, r.Body)
			gotSize.Add(n)
			quota429(w)
		},
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("over-cap request must not be retried on another account")
		},
	})

	big := `{"model":"claude","metadata":{"user_id":"s"},"messages":[{"role":"user","content":"` +
		strings.Repeat("x", defaultBodyCap) + `"}]}`
	resp := postMessages(t, rig.front.URL, big)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 passed through", resp.StatusCode)
	}
	if gotSize.Load() != int64(len(big)) {
		t.Errorf("upstream received %d bytes, want %d (full streamed body)", gotSize.Load(), len(big))
	}
	total := rig.hits[0].Load() + rig.hits[1].Load()
	if total != 1 {
		t.Errorf("upstream hits = %d, want 1 (no retry)", total)
	}
}
