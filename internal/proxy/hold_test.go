package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// holdRig: single-account pool whose upstream quota-429s until told
// otherwise; handler configured with the given mode/holdMax.
func holdRig(t *testing.T, mode, holdMax string, upstream http.HandlerFunc) (*httptest.Server, *pool.Pool) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = mode
	cfg.Pool.HoldMax = holdMax
	acct := pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, "")
	p := pool.New([]*pool.Account{acct}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	return front, p
}

func quota429WithReset(reset time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", fmt.Sprint(reset.Unix()))
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"quota"}`)
	}
}

// quota429RetryAfter signals quota rejection with a precise retry-after (the
// reset header truncates to wall seconds — too coarse for short test holds).
func quota429RetryAfter(sec int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
		w.Header().Set("Retry-After", fmt.Sprint(sec))
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"quota"}`)
	}
}

// Hold mode: request parks until the reset, then goes through — the client
// sees a 200, never a 429.
func TestHoldThenSuccess(t *testing.T) {
	var calls atomic.Int32
	front, _ := holdRig(t, "hold", "30s", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			quota429RetryAfter(1)(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})

	start := time.Now()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(testBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after hold", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("returned after %v — did not actually hold", elapsed)
	}
	if calls.Load() != 2 {
		t.Errorf("upstream hits = %d, want 2 (429 then post-reset retry)", calls.Load())
	}
}

// Reset beyond the hold budget (issue #55): the synthetic 429 comes back
// immediately, not after parking for holdMax. holdMax is 100ms and the
// reset is an hour out — the bound is tight enough (150ms) that a planted
// "hold for the capped budget, then fail" regression (the literal bug #55
// reports) fails this, not just an absolute "didn't take forever" check.
func TestHoldCapFallsThrough(t *testing.T) {
	front, p := holdRig(t, "hold", "100ms", func(w http.ResponseWriter, r *http.Request) {
		quota429WithReset(time.Now().Add(time.Hour))(w, r)
	})
	_ = p

	start := time.Now()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(testBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("took %v — should fail fast past the budget, not park for it", elapsed)
	}
}

// Unknown reset (issue #55's design note): nothing is exhausted with a
// reset to reason about — the only account is disabled, not merely
// spent — so there is no "soonest reset" to compare against the deadline.
// Today's behaviour (pre-#55) is to hold for the request's bounded budget
// rather than guess "unknown means far away" and fail before the budget is
// even spent; a disabled account never recovers on its own, but treating
// unknown as an instant failure would do the same to a genuinely transient
// unknown (e.g. every account momentarily mid-recovery).
func TestHoldUnknownResetHoldsToBudget(t *testing.T) {
	front, p := holdRig(t, "hold", "150ms", func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be hit — the only account is disabled before selection")
	})
	p.Accounts()[0].Disable()

	start := time.Now()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(testBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("returned after %v — did not hold the unknown-reset budget", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v — the unknown-reset hold must still be bounded by holdMax", elapsed)
	}
}

// A fail-fast 429 must not be reported as a hold: reqlog.EventHeld means
// the request actually parked, and this one never did.
func TestFailFastNeverLogsHeld(t *testing.T) {
	up := httptest.NewServer(quota429WithReset(time.Now().Add(time.Hour)))
	t.Cleanup(up.Close)
	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "hold"
	cfg.Pool.HoldMax = "100ms"
	acct := pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, "")
	p := pool.New([]*pool.Account{acct}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	broker := events.New()
	h.SetHooks(Hooks{Events: broker})
	sub := broker.Subscribe()
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)

	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(testBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}

	deadline := time.After(time.Second)
	sawExhausted := false
	for !sawExhausted {
		select {
		case ev := <-sub:
			if ev.Type == reqlog.EventHeld {
				t.Fatal("fail-fast path published EventHeld — the request never held")
			}
			if ev.Type == reqlog.EventExhausted {
				sawExhausted = true
			}
		case <-deadline:
			t.Fatal("never observed EventExhausted for the fail-fast request")
		}
	}
}

// fail mode never holds. The reset (5s) sits comfortably inside holdMax
// (30s), so a mode check that got lost in a refactor would hold for the
// full 5s+ — the 500ms bound below only passes when "fail" is actually
// honoured, not just when the reset happens to be sooner than the assertion
// window.
func TestFailModeReturns429Immediately(t *testing.T) {
	front, _ := holdRig(t, "fail", "30s", func(w http.ResponseWriter, r *http.Request) {
		quota429WithReset(time.Now().Add(5*time.Second))(w, r)
	})
	start := time.Now()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(testBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("took %v — fail mode held the request", elapsed)
	}
}

// A client disconnect during the hold cancels it cleanly.
func TestHoldCancelledByClient(t *testing.T) {
	// holdMax (2h) must cover the reset (1h) or the hold never engages.
	front, _ := holdRig(t, "hold", "2h", func(w http.ResponseWriter, r *http.Request) {
		quota429WithReset(time.Now().Add(time.Hour))(w, r)
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/messages", strings.NewReader(testBody))
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
		}
		done <- err
	}()
	time.Sleep(300 * time.Millisecond) // let the request reach the hold
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("request succeeded despite cancel")
		}
	case <-time.After(3 * time.Second):
		t.Error("hold did not release on client cancel")
	}
}

// A parked request must be visible from outside. Without this the status
// line and the dashboard cannot tell a hold from a hang, which is the whole
// reason the registry exists (§6.11).
func TestHoldIsObservableWhileParked(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	front, p := holdRig(t, "hold", "2h", func(w http.ResponseWriter, r *http.Request) {
		quota429WithReset(reset)(w, r)
	})

	if n, _ := p.Holds(); n != 0 {
		t.Fatalf("pool reports %d holds before any request", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/messages", strings.NewReader(testBody))
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	// Poll rather than sleep-and-assert: the request has to get through
	// selection and a 429 before it parks.
	var until time.Time
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n, u := p.Holds(); n == 1 {
			until = u
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if until.IsZero() {
		t.Fatal("the parked request never appeared in the hold registry")
	}
	if d := until.Sub(reset); d > time.Second || d < -time.Second {
		t.Errorf("hold reports until %v, want the account reset %v", until, reset)
	}

	cancel()
	for time.Now().Before(deadline.Add(2 * time.Second)) {
		if n, _ := p.Holds(); n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	n, _ := p.Holds()
	t.Errorf("hold not released after the client went away: %d still registered", n)
}
