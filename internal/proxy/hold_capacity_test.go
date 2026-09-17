package proxy

// Tests for issue #105: a held request (hold.go's park) waking on the
// pool's capacity-changed signal, not only on its own timer.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
)

// TestHoldServedByAccountAddedWhileWaiting is the regression test for the
// reported bug: every account exhausted, a working account added mid-hold,
// and the held request must be served by it PROMPTLY — not merely
// eventually once the (far-off) reset arrives. Before issue #105, park only
// ever woke on that reset timer, so a good account sitting idle was
// invisible to a request already parked.
func TestHoldServedByAccountAddedWhileWaiting(t *testing.T) {
	// holdMax comfortably covers the reset (mirrors TestHoldCancelledByClient's
	// 2h/1h shape) so the request actually parks instead of failing fast
	// (#55) — the reset itself is deliberately far out: if the fix regresses
	// to timer-only waking, this test times out waiting on the response
	// rather than failing fast, which is the point. It proves "served
	// promptly", not just "served eventually".
	front, p := holdRig(t, "hold", "2h", func(w http.ResponseWriter, r *http.Request) {
		quota429WithReset(time.Now().Add(time.Hour))(w, r)
	})

	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := holdPost(t, front, testBody)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		done <- result{status: resp.StatusCode}
	}()

	waitForHolds(t, p, 1)

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(healthy.Close)

	start := time.Now()
	if !p.Add(pool.NewAccount("b", pool.SourceYAML, "tok2", "", 0, healthy.URL)) {
		t.Fatal("Add reported the account was not actually added")
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("request failed: %v", r.err)
		}
		if r.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", r.status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("held request was not served after a working account was added")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v after Add — should wake promptly, not wait out the hour-long reset", elapsed)
	}
}

// TestHoldServedByEarlyReprobeRecovery covers the second named trigger:
// issue #90's re-probe can clear an account's bench before its stated
// reset. A held request must see that too, not just a brand-new account.
func TestHoldServedByEarlyReprobeRecovery(t *testing.T) {
	var calls atomic.Int32
	front, p := holdRig(t, "hold", "2h", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			quota429WithReset(time.Now().Add(time.Hour))(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})

	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := holdPost(t, front, testBody)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		done <- result{status: resp.StatusCode}
	}()

	waitForHolds(t, p, 1)

	start := time.Now()
	// Simulate #90's re-probe finding the account healthy again, long
	// before its recorded hour-out exhaustedUntil.
	p.ClearExhausted(p.Accounts()[0])

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("request failed: %v", r.err)
		}
		if r.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", r.status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("held request was not served after ClearExhausted")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v after ClearExhausted — should wake promptly", elapsed)
	}
}

// TestHoldWakeResumesAgainstOriginalDeadline: a capacity wake that turns out
// not to help (selection still fails) must fall back into waiting against
// the SAME deadline the request originally committed to — not a fresh
// holdMax-sized budget. Driven directly against waitForReset/SelectExcept
// (rather than over real HTTP) so the timing is precise: the reset-header
// path truncates to whole seconds and would make anything narrower than
// ~1s flaky by construction.
//
// The account starts exhausted with a reset comfortably inside `deadline`.
// A spurious wake fires shortly after, extending the exhaustion (as if a
// real 429 elsewhere had just discovered a much later true reset) to
// something far beyond `deadline` and signalling capacity. The fix must
// re-check against the untouched `deadline` and fail fast (issue #55);
// a bug that re-derived a fresh budget from the wake, or that took the
// wake to mean "selection will now succeed", would instead keep the
// request holding.
func TestHoldWakeResumesAgainstOriginalDeadline(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be hit — this test drives waitForReset directly")
	}))
	t.Cleanup(up.Close)
	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "hold"
	cfg.Pool.HoldMax = "10s" // large on purpose — `deadline` below is what actually governs
	acct := pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, "")
	p := pool.New([]*pool.Account{acct}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(300 * time.Millisecond) // the ORIGINAL, fixed budget
	p.MarkExhausted(acct, time.Now().Add(200*time.Millisecond))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	go func() {
		time.Sleep(80 * time.Millisecond)
		// Push the reset to 2s out — comfortably beyond `deadline` — and
		// wake anything parked. Selection must still fail (the account is
		// still exhausted, just on a different reset).
		p.MarkExhausted(acct, time.Now().Add(2*time.Second))
		p.SignalCapacity()
	}()

	tried := map[string]bool{}
	start := time.Now()
	var got *pool.Account
	for time.Now().Before(deadline.Add(2 * time.Second)) { // generous outer safety bound only
		// Subscribed before the selection it is about to wait on the
		// failure of, exactly as route() does (issue #187).
		wake := p.CapacitySignal()
		got = p.SelectExcept("sess", nil, tried)
		if got != nil {
			break
		}
		if !h.waitForReset(req, nil, deadline, wake) {
			break // expected: the 2s-out reset now exceeds `deadline`
		}
		clear(tried)
	}
	elapsed := time.Since(start)

	if got != nil {
		t.Fatalf("selection succeeded after %v — expected a fail-fast 429 instead", elapsed)
	}
	if elapsed < 60*time.Millisecond {
		t.Errorf("failed after only %v — suspiciously immediate; check the wake actually fired", elapsed)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("took %v to fail fast — either the wake never fired (falling back to the "+
			"original ~450ms reset timer) or the budget was extended by it; want well under "+
			"`deadline`'s 300ms+slack, and well under the ~450ms the unmodified reset timer "+
			"would take on its own", elapsed)
	}
}

// TestHeldRequestsDoNotThunderOnSingleNewAccount is the anti-thunder
// assertion issue #91 asked for: many requests held on one exhausted
// account, then a single working account appears. They must all still be
// served (the fix must not regress into never waking anyone), but not all
// in the same instant — otherwise a mass wake just recreates the herd on a
// faster clock.
func TestHeldRequestsDoNotThunderOnSingleNewAccount(t *testing.T) {
	up := httptest.NewServer(quota429WithReset(time.Now().Add(time.Hour)))
	t.Cleanup(up.Close)
	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "hold"
	cfg.Pool.HoldMax = "2h" // comfortably covers the hour-out reset — see holdRig callers above
	acct := pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, "")
	p := pool.New([]*pool.Account{acct}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)

	const n = 20
	type result struct {
		status int
		err    error
	}
	results := make(chan result, n)
	for i := range n {
		go func(i int) {
			body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","max_tokens":16,`+
				`"metadata":{"user_id":"session-%d"},"messages":[]}`, i)
			resp, err := holdPost(t, front, body)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			results <- result{status: resp.StatusCode}
		}(i)
	}

	waitForHolds(t, p, n)

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(healthy.Close)

	start := time.Now()
	p.Add(pool.NewAccount("b", pool.SourceYAML, "tok2", "", 0, healthy.URL))

	completed := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Errorf("request %d failed: %v", i, r.err)
				continue
			}
			if r.status != http.StatusOK {
				t.Errorf("request %d status = %d, want 200", i, r.status)
			}
			completed = append(completed, time.Since(start))
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d/%d requests completed after Add", len(completed), n)
		}
	}

	sort.Slice(completed, func(i, j int) bool { return completed[i] < completed[j] })
	spread := completed[len(completed)-1] - completed[0]
	// wakeStagger*n comfortably exceeds this; plain OS/goroutine jitter
	// among 20 in-process requests to a local test server does not — this
	// threshold is chosen to separate "staggered" from "thundered", not to
	// pin the exact stagger constant.
	if spread < 150*time.Millisecond {
		t.Errorf("spread across completions = %v — requests appear to have thundered in lockstep", spread)
	}
	// Still fast in absolute terms: the anti-thunder measure must not
	// reintroduce a meaningful wait of its own.
	if completed[len(completed)-1] > 3*time.Second {
		t.Errorf("slowest completion took %v — anti-thunder stagger grew too large", completed[len(completed)-1])
	}
}

// TestHoldWakesOnCapacitySignalledBeforeItParked is issue #187's hang,
// reproduced without a race.
//
// The capacity change is made to land in the exact gap the bug lived in:
// after the selection that failed, before the request is waiting on
// anything. In production that gap is a handful of instructions wide and
// CI hit it once in twenty parked requests; here it is simply where the
// Add is written, so the test is deterministic and needs no sleep, no
// loaded machine and no repetition.
//
// The fix is that route() subscribes to the capacity generation BEFORE it
// selects and hands that generation down, so a signal in the gap closes a
// channel this request is already holding. Restore the old behaviour —
// park taking h.pool.CapacitySignal() itself — and the generation it gets
// is the fresh one Add installed, which nothing will ever close: the
// request sleeps out the hour-long reset with a usable account idle, and
// the bounded wait below reports that by name.
func TestHoldWakesOnCapacitySignalledBeforeItParked(t *testing.T) {
	h, p := windowHoldRig(t, 1, "2h") // budget covers the reset, so it really parks
	acct := p.Accounts()[0]
	p.MarkExhausted(acct, time.Now().Add(time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // so a request left parked by a regression cannot outlive the test
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	body := []byte(testBody)

	// Exactly route()'s order: subscribe, then select.
	wake := p.CapacitySignal()
	if got := p.SelectExcept("s", body, nil); got != nil {
		t.Fatalf("SelectExcept = %q, want nil — the request must actually have nothing to run on", got.Name)
	}

	// The gap. Capacity appears after selection failed and before the park.
	healthy := pool.NewAccount("b", pool.SourceYAML, "tok2", "", 0, "")
	healthy.Type = "claude-oauth"
	if !p.Add(healthy) {
		t.Fatal("Add reported the account was not actually added")
	}

	done := make(chan bool, 1)
	go func() { done <- h.waitForReset(req, body, time.Now().Add(2*time.Hour), wake) }()

	select {
	case got := <-done:
		if !got {
			t.Error("waitForReset = false, want true — capacity arrived, so the caller must be sent back to re-select")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForReset never returned: the capacity change that landed between selection and park " +
			"was signalled to a generation this request never held, so it is parked on the hour-out " +
			"reset with a usable account sitting idle (#187)")
	}

	if got := p.SelectExcept("s", body, nil); got == nil {
		t.Error("selection still fails after the wake — the premise broke, not the hold")
	}
}

// waitForHolds polls until the pool reports exactly want parked requests,
// failing the test if that never happens — a held request has to clear
// selection and a 429 before it parks, so this can't be asserted
// synchronously right after firing off the request(s).
func waitForHolds(t *testing.T, p *pool.Pool, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := p.Holds(); n == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	n, _ := p.Holds()
	t.Fatalf("pool reports %d holds, want %d", n, want)
}
