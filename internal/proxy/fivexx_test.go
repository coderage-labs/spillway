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

func overloaded529(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(529)
	fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
}

// Issue #26 (a): a 529 from account A rotates to B and the client sees B's
// 200 — the buffered body must have been re-sent, and A must NOT be marked
// exhausted (a 5xx is not the account's fault).
func TestUpstream529RotatesAndResends(t *testing.T) {
	var bodyOnServing string
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) { overloaded529(w) },
		func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			bodyOnServing = string(b)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		},
	})
	rig.pool.Accounts()[0].SetPriority(0)
	rig.pool.Accounts()[1].SetPriority(1)

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
		t.Errorf("total upstream hits = %d, want 2 (one 529, one 200)", total)
	}
	if bodyOnServing == "" {
		t.Errorf("the account that served never received the buffered body")
	}
	for _, a := range rig.pool.Accounts() {
		if a.State() == pool.StateExhausted {
			t.Errorf("account %s marked exhausted — a 5xx is not the account's fault", a.Name)
		}
	}
}

// Issue #26 (b): a 529 with only one account in the pool must return the 529
// to the client, not loop — there is nowhere to rotate to, and the existing
// pool-exhaustion path synthesizes an unrelated 429, which would hide the
// real upstream outage from the client.
func TestUpstream529SingleAccountReturnsRealResponse(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		overloaded529(w)
	}))
	defer upstream.Close()

	cfg := config.Defaults()
	cfg.Pool.ExhaustedMode = "fail"
	p := pool.New([]*pool.Account{
		pool.NewAccount("solo", pool.SourceYAML, "tok", "", 0, upstream.URL),
	}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 529 {
		t.Fatalf("status = %d, want 529 (the real upstream response); body %s", resp.StatusCode, body)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (no account to rotate to)", hits.Load())
	}
}

// Issue #26 (c): a 529 that arrives once the request body has already begun
// streaming straight to the account (over the cap, so it was never buffered)
// must NOT be retried — there is no buffered copy left to re-send, and the
// invariant is the same one that already governs quota-429 in this case
// (TestOverCapBodyStreamsWithoutRetry). The pre-first-byte failover this
// issue extends only ever applies to a buffered request in the first place.
func TestUpstream529OverCapBodyNotRetried(t *testing.T) {
	var gotSize atomic.Int64
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			n, _ := io.Copy(io.Discard, r.Body)
			gotSize.Add(n)
			overloaded529(w)
		},
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("over-cap request must not be retried on another account")
		},
	})

	big := `{"model":"claude","metadata":{"user_id":"s"},"messages":[{"role":"user","content":"` +
		strings.Repeat("x", defaultBodyCap) + `"}]}`
	resp := postMessages(t, rig.front.URL, big)
	defer resp.Body.Close()

	if resp.StatusCode != 529 {
		t.Errorf("status = %d, want 529 passed through", resp.StatusCode)
	}
	if gotSize.Load() != int64(len(big)) {
		t.Errorf("upstream received %d bytes, want %d (full streamed body)", gotSize.Load(), len(big))
	}
	total := rig.hits[0].Load() + rig.hits[1].Load()
	if total != 1 {
		t.Errorf("upstream hits = %d, want 1 (no retry)", total)
	}
}

// Issue #26: 500 and 503 rotate exactly like 529 — nothing in the design
// singles 529 out. No provider classifies anything >=500 as account-specific
// (ClassifiableStatuses stays under 500 for both), so there is no basis to
// treat one 5xx differently from another.
func TestUpstream5xxVariantsAllRotate(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			rig := newRig(t, [2]http.HandlerFunc{
				func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) },
				func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true}`) },
			})
			rig.pool.Accounts()[0].SetPriority(0)
			rig.pool.Accounts()[1].SetPriority(1)

			resp := postMessages(t, rig.front.URL, testBody)
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: got %d, want 200 (rotated to B); body %s", status, resp.StatusCode, body)
			}
			total := rig.hits[0].Load() + rig.hits[1].Load()
			if total != 2 {
				t.Errorf("status %d: upstream hits = %d, want 2", status, total)
			}
		})
	}
}

// Issue #26: the rotation is bounded to a SINGLE hop, unlike quota-429 or a
// dead connection which chain through every untried account. Three accounts,
// the first two answer 529, the third would answer 200 — the client must see
// the second account's 529 (the real upstream response), and the third
// account must never be touched.
func TestUpstream5xxRotationBoundedToOneHop(t *testing.T) {
	var hits [3]atomic.Int32
	handlers := []http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) { hits[0].Add(1); overloaded529(w) },
		func(w http.ResponseWriter, r *http.Request) { hits[1].Add(1); overloaded529(w) },
		func(w http.ResponseWriter, r *http.Request) {
			hits[2].Add(1)
			t.Error("rotation must be bounded to one hop — the third account must not be tried")
		},
	}
	accts := make([]*pool.Account, 3)
	for i, hf := range handlers {
		hf := hf
		up := httptest.NewServer(http.HandlerFunc(hf))
		t.Cleanup(up.Close)
		accts[i] = pool.NewAccount(fmt.Sprintf("acct-%d", i), pool.SourceYAML, fmt.Sprintf("tok-%d", i), "", 0, up.URL)
		accts[i].SetPriority(i)
	}
	p := pool.New(accts, time.Now())
	cfg := config.Defaults()
	cfg.Pool.ExhaustedMode = "fail"
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 529 {
		t.Fatalf("status = %d, want 529 (the second account's real response); body %s", resp.StatusCode, body)
	}
	if hits[0].Load() != 1 || hits[1].Load() != 1 || hits[2].Load() != 0 {
		t.Errorf("hits = (%d, %d, %d), want (1, 1, 0) — bounded to one hop",
			hits[0].Load(), hits[1].Load(), hits[2].Load())
	}
}
