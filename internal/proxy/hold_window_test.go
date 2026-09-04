package proxy

// Tests for issue #140: a request parked because every account is
// family-rejected used to sleep the entire holdMax, because
// pool.EarliestReset only ever spoke for accounts in StateExhausted and a
// fable-only 429 deliberately leaves the account StateOK (issue #54).
//
// The direct-drive tests below call waitForReset rather than going over
// HTTP, for the reason TestHoldWakeResumesAgainstOriginalDeadline gives:
// the reset header truncates to whole seconds, which makes anything
// sub-second flaky by construction. The window-rejection deadlines here are
// written straight onto the pool, so they carry full precision and the
// assertion can be on the ACTUAL wait rather than on a log line.
//
// Every wait below is (deadline) sitting far outside (rejection deadline),
// scaled down from the reported 4h-vs-10m by the same ratio: a bug that
// falls back to the budget takes the long one, the fix takes the short one,
// and the assertion band contains neither the other.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
)

// windowHoldRig builds a hold-mode handler over n claude accounts and an
// upstream that fails the test if it is ever reached — these tests never
// get past selection, and an upstream hit would mean the premise broke.
func windowHoldRig(t *testing.T, n int, holdMax string) (*Handler, *pool.Pool) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be hit — these tests drive waitForReset directly")
	}))
	t.Cleanup(up.Close)
	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "hold"
	cfg.Pool.HoldMax = holdMax
	accts := make([]*pool.Account, n)
	for i := range accts {
		a := pool.NewAccount(string(rune('a'+i)), pool.SourceYAML, "tok", "", 0, "")
		a.Type = "claude-oauth"
		accts[i] = a
	}
	p := pool.New(accts, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	return h, p
}

// fableReq is a POST /v1/messages carrying the fable model, and the same
// bytes as a body slice — waitForReset is handed exactly what SelectExcept
// was.
func fableReq() (*http.Request, []byte) {
	return httptest.NewRequest(http.MethodPost, "/v1/messages", nil), []byte(fableReqBody)
}

// The headline. Every account is fable-rejected until T+150ms; holdMax (and
// so the deadline) is 6s — the same shape as the reported 10-minutes-out
// rejection under a 4h budget. The request must wake on the rejection
// deadline, not the budget.
func TestHoldWakesOnWindowRejectionDeadlineNotHoldMax(t *testing.T) {
	h, p := windowHoldRig(t, 2, "6s")
	req, body := fableReq()

	until := time.Now().Add(150 * time.Millisecond)
	for _, a := range p.Accounts() {
		p.MarkWindowRejected(a, "7d-fable", until)
	}

	// Premise, all three parts: the request really is parked, nothing is
	// exhausted, and #54's account-wide serving is untouched.
	if got := p.SelectExcept("s", body, nil); got != nil {
		t.Fatalf("SelectExcept(fable) = %q, want nil — the request must actually park", got.Name)
	}
	if _, ok := p.EarliestReset(); ok {
		t.Fatal("EarliestReset reported a reset — #140's premise is that a window rejection is invisible to it")
	}
	for _, a := range p.Accounts() {
		if a.State() != pool.StateOK {
			t.Fatalf("account %q state = %v, want StateOK — a fable rejection must not exhaust (#54)", a.Name, a.State())
		}
	}

	deadline := time.Now().Add(6 * time.Second)
	start := time.Now()
	got := h.waitForReset(req, body, deadline)
	elapsed := time.Since(start)

	if !got {
		t.Fatalf("waitForReset = false after %v, want true (the rejection expires well inside the budget)", elapsed)
	}
	if elapsed < 140*time.Millisecond {
		t.Errorf("waited only %v — it must actually wait out the rejection, not return immediately", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("waited %v — the wake must come from the 150ms window-rejection deadline, "+
			"not the 6s holdMax budget (that fallback is bug #140)", elapsed)
	}
	if got := p.SelectExcept("s", body, nil); got == nil {
		t.Error("selection still fails after the rejection deadline passed — the wake was premature")
	}
}

// Mixed pool, window deadline sooner: one account exhausted account-wide 3s
// out, another fable-rejected 150ms out. The wake must be the soonest of
// BOTH, so a fix that only consults EarliestReset (3s) fails here.
func TestHoldMixedPoolWakesOnSoonestWindowRejection(t *testing.T) {
	h, p := windowHoldRig(t, 2, "10s")
	req, body := fableReq()
	accts := p.Accounts()

	p.MarkExhausted(accts[0], time.Now().Add(3*time.Second))
	p.MarkWindowRejected(accts[1], "7d-fable", time.Now().Add(150*time.Millisecond))

	if got := p.SelectExcept("s", body, nil); got != nil {
		t.Fatalf("SelectExcept(fable) = %q, want nil", got.Name)
	}

	start := time.Now()
	got := h.waitForReset(req, body, time.Now().Add(10*time.Second))
	elapsed := time.Since(start)

	if !got {
		t.Fatalf("waitForReset = false after %v, want true", elapsed)
	}
	if elapsed < 140*time.Millisecond {
		t.Errorf("waited only %v — did not wait out the window rejection", elapsed)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("waited %v — must wake on the 150ms window rejection, not the 3s account-wide exhaustion", elapsed)
	}
}

// Mixed pool, the other way round: the account-wide exhaustion is the
// soonest (150ms) and the window rejection is 3s out. The wake must still
// be the soonest of both, so a fix that REPLACES EarliestReset with the
// window deadline instead of taking the minimum fails here.
func TestHoldMixedPoolWakesOnSoonestExhaustion(t *testing.T) {
	h, p := windowHoldRig(t, 2, "10s")
	req, body := fableReq()
	accts := p.Accounts()

	p.MarkExhausted(accts[0], time.Now().Add(150*time.Millisecond))
	p.MarkWindowRejected(accts[1], "7d-fable", time.Now().Add(3*time.Second))

	if got := p.SelectExcept("s", body, nil); got != nil {
		t.Fatalf("SelectExcept(fable) = %q, want nil", got.Name)
	}

	start := time.Now()
	got := h.waitForReset(req, body, time.Now().Add(10*time.Second))
	elapsed := time.Since(start)

	if !got {
		t.Fatalf("waitForReset = false after %v, want true", elapsed)
	}
	if elapsed < 140*time.Millisecond {
		t.Errorf("waited only %v — did not wait out the exhaustion", elapsed)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("waited %v — must wake on the 150ms exhaustion, not the 3s window rejection", elapsed)
	}
}

// holdMax still caps: a window rejection an hour out under a 100ms budget
// is issue #55's fail-fast case, reached through the new deadline rather
// than around it. The request must get its 429 now, not park for the
// budget to reach the identical answer.
func TestHoldMaxStillCapsAWindowRejection(t *testing.T) {
	h, p := windowHoldRig(t, 2, "100ms")
	req, body := fableReq()
	for _, a := range p.Accounts() {
		p.MarkWindowRejected(a, "7d-fable", time.Now().Add(time.Hour))
	}

	// Off the test goroutine and on a bounded wait: without the cap this
	// parks for the full hour, and a direct call would hang until the
	// package's own test timeout rather than reporting a failure.
	done := make(chan bool, 1)
	start := time.Now()
	go func() { done <- h.waitForReset(req, body, time.Now().Add(100*time.Millisecond)) }()

	select {
	case got := <-done:
		elapsed := time.Since(start)
		if got {
			t.Errorf("waitForReset = true after %v — an hour-out rejection is beyond a 100ms budget, fail fast", elapsed)
		}
		if elapsed > 50*time.Millisecond {
			t.Errorf("took %v — must fail fast, not park for the budget", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForReset never returned — it parked on an hour-out rejection instead of capping at holdMax")
	}
}

// A rejection deadline already in the past must not become the wake time:
// the hold reads a non-future reset as "re-select immediately", and this
// account is still unusable for another reason (it is exhausted), so the
// caller's loop would spin instead of waiting.
//
// Constructed so the expired-deadline filter is the ONLY thing under test:
// the account is exhausted 400ms out (that is the true, correct wake time),
// and carries a fable rejection that expired a minute ago. Dropping the
// past-deadline filter makes this return in microseconds.
func TestHoldIgnoresExpiredWindowRejectionRatherThanSpinning(t *testing.T) {
	h, p := windowHoldRig(t, 1, "10s")
	req, body := fableReq()
	a := p.Accounts()[0]

	p.MarkExhausted(a, time.Now().Add(400*time.Millisecond))
	p.MarkWindowRejected(a, "7d-fable", time.Now().Add(-time.Minute))

	if a.WindowRejectedFor("claude-opus-4-fable-preview") {
		t.Fatal("premise broken: an expired rejection must not still exclude")
	}
	if got := p.SelectExcept("s", body, nil); got != nil {
		t.Fatalf("SelectExcept = %q, want nil (the account is exhausted)", got.Name)
	}

	start := time.Now()
	got := h.waitForReset(req, body, time.Now().Add(10*time.Second))
	elapsed := time.Since(start)

	if !got {
		t.Fatalf("waitForReset = false after %v, want true (the exhaustion is well inside the budget)", elapsed)
	}
	if elapsed < 350*time.Millisecond {
		t.Errorf("returned after %v — an expired rejection was taken as the wake time, so the hold "+
			"did not wait for the 400ms exhaustion at all; the caller's loop would spin on this", elapsed)
	}
}

// #54's guarantee, restated against the new code path: a fable rejection on
// every account must leave Sonnet serving from them. Over real HTTP with a
// 2h holdMax, so a fix that exposed the deadline by exhausting the account
// would show up as a Sonnet request parking for hours instead of a 200.
func TestFableRejectionStillLeavesSonnetServing(t *testing.T) {
	front, p := holdRig(t, "hold", "2h", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	acct := p.Accounts()[0]
	p.MarkWindowRejected(acct, "7d-fable", time.Now().Add(time.Hour))

	// A bounded client: if the fable rejection did widen into the account,
	// this request parks for the hour and the failure must be a reported
	// one, not the package test timeout.
	client := &http.Client{Timeout: 3 * time.Second}
	start := time.Now()
	resp, err := client.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(testBody))
	if err != nil {
		t.Fatalf("Sonnet request failed after %v: %v — a fable rejection must not gate Sonnet (#54)", time.Since(start), err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a fable rejection must not gate Sonnet (#54)", resp.StatusCode)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Sonnet took %v — it was parked, so the fable rejection widened into the whole account", elapsed)
	}
	if acct.State() != pool.StateOK {
		t.Errorf("account state = %v, want StateOK — nothing here may exhaust the account", acct.State())
	}
}

// The hold's client-cancellation check (#142/#143) must still work when the
// wake time came from a window rejection rather than an exhaustion: the
// deadline is an hour out, so only the cancel can end this.
func TestWindowRejectionHoldCancelledByClient(t *testing.T) {
	h, p := windowHoldRig(t, 1, "2h")
	p.MarkWindowRejected(p.Accounts()[0], "7d-fable", time.Now().Add(time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	body := []byte(fableReqBody)

	done := make(chan bool, 1)
	go func() { done <- h.waitForReset(req, body, time.Now().Add(2*time.Hour)) }()

	// Synchronise on the hold registry rather than a sleep: the request is
	// parked exactly when it appears there.
	deadline := time.Now().Add(3 * time.Second)
	parked := false
	for time.Now().Before(deadline) {
		if n, _ := p.Holds(); n == 1 {
			parked = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !parked {
		t.Fatal("the request never appeared in the hold registry — it did not park on the window rejection")
	}
	cancel()

	select {
	case got := <-done:
		if got {
			t.Error("waitForReset = true after cancel, want false (the client went away)")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the hold did not release on client cancel")
	}
	if n, _ := p.Holds(); n != 0 {
		t.Errorf("%d holds still registered after cancel", n)
	}
}

// The hold's registered `until` — what the status line and dashboard show —
// must be the window-rejection deadline, not an hours-out budget end. A
// parked user reading "until 18:30" when the quota returns at 14:10 is the
// same bug wearing a different hat.
func TestWindowRejectionHoldReportsTheRejectionDeadline(t *testing.T) {
	h, p := windowHoldRig(t, 1, "2h")
	until := time.Now().Add(time.Hour)
	p.MarkWindowRejected(p.Accounts()[0], "7d-fable", until)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	go h.waitForReset(req, []byte(fableReqBody), time.Now().Add(2*time.Hour))

	var reported time.Time
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n, u := p.Holds(); n == 1 {
			reported = u
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if reported.IsZero() {
		t.Fatal("the request never parked")
	}
	if d := reported.Sub(until); d > time.Second || d < -time.Second {
		t.Errorf("hold reports until %v, want the rejection deadline %v", reported, until)
	}
}
