package proxy

// Issue #142: a client that goes away mid-request must be dropped, not
// rotated. The failure it caused was purely one of reporting — no quota was
// spent — so these tests assert on what was REPORTED: the events published,
// the request-log row, the bytes written, and the pool's own in-flight
// bookkeeping.
//
// `tried` is a local map with no accessor, so it is proven the way it is
// felt: one entry is added per rotation, and every rotation publishes an
// event naming the account it just blamed. Zero rotation events therefore
// means zero accounts marked — and it is the events, not the upstream hit
// counts, that carry that. (A rotation on an already-cancelled request never
// reaches the wire at all: the transport fails the retry before dialling, so
// the second account's upstream records no hit even while the pool walks
// past it. That is exactly how this bug stayed invisible except in the
// telemetry.)

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// cancelRig is newRig plus the two observability sinks these tests assert on
// (a real request log and a broker subscription opened before any request
// runs, so nothing can be published into an empty broker) and the Handler
// itself, so a test can pin its dialler.
type cancelRig struct {
	front  *httptest.Server
	pool   *pool.Pool
	h      *Handler
	log    *reqlog.Log
	events chan events.Event
	hits   [2]*atomic.Int32
}

func newCancelRig(t *testing.T, handlers [2]http.HandlerFunc) *cancelRig {
	t.Helper()
	rig := &cancelRig{hits: [2]*atomic.Int32{{}, {}}}
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
	cfg.Pool.ExhaustedMode = "fail" // no holding: these tests want the decision
	h, err := NewHandler(&cfg, testLogger(), rig.pool)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	rig.h = h
	rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rl.Close() })
	rig.log = rl
	broker := events.New()
	rig.events = broker.Subscribe()
	h.SetHooks(Hooks{Log: rl, Events: broker})
	rig.front = httptest.NewServer(h)
	t.Cleanup(rig.front.Close)
	return rig
}

// drainEvents collects everything published so far. Safe to call once the
// request-log row exists: route publishes before it returns, and the row is
// written after it does.
func (rig *cancelRig) drainEvents() []events.Event {
	var out []events.Event
	for {
		select {
		case e := <-rig.events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func (rig *cancelRig) countEvents(kind string) int {
	n := 0
	for _, e := range rig.drainEvents() {
		if e.Type == kind {
			n++
		}
	}
	return n
}

func (rig *cancelRig) totalHits() int32 { return rig.hits[0].Load() + rig.hits[1].Load() }

// assertNoInFlight proves pool.Done ran for the account that was selected.
// The early return added for #142 sits before the rotation branch that used
// to be the only path out of a RoundTrip error, so it has to release the
// account itself; a leaked slot permanently misreports the account as busier
// than it is, and selection is load-based.
func (rig *cancelRig) assertNoInFlight(t *testing.T) {
	t.Helper()
	for _, a := range rig.pool.Accounts() {
		if n := a.InFlight(); n != 0 {
			t.Errorf("%s still reports %d in flight — pool.Done was skipped", a.Name, n)
		}
	}
}

// blockUntilClientGone is an upstream that reports its arrival and then hangs
// until the request is cancelled — long enough for the client to go away
// while spillway is inside RoundTrip.
func blockUntilClientGone(arrived chan<- struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Drain first: net/http only starts the background read that
		// notices a closed connection — and so only cancels this handler's
		// context — once the request body has hit EOF. A mock upstream that
		// skipped this would sit here for the full fallback timeout after
		// spillway had already dropped the request, which looks exactly
		// like a leak.
		io.Copy(io.Discard, r.Body)
		select {
		case arrived <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"late":true}`)
		}
	}
}

// pinFirstDialFailure makes the first outbound dial fail with err and every
// dial after it succeed normally. Which account selection picks first is
// load-based and not fixed, so failing "the first dial" rather than "account
// 0's address" is what makes these tests deterministic.
func pinFirstDialFailure(h *Handler, err error) {
	base := h.Transport.DialContext
	var dials atomic.Int32
	h.Transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			return nil, err
		}
		return base(ctx, network, addr)
	}
}

// The bug: a cancelled client walked the pool. One account is contacted, no
// rotation event is published, no account is left blamed in the request log,
// and nothing is written back to a client that is no longer there.
func TestClientCancelMidRequestDoesNotRotate(t *testing.T) {
	arrived := make(chan struct{}, 2)
	rig := newCancelRig(t, [2]http.HandlerFunc{
		blockUntilClientGone(arrived),
		blockUntilClientGone(arrived),
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		rig.front.URL+"/v1/messages", strings.NewReader(testBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		done <- err
	}()

	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached an upstream")
	}
	cancel() // Escape in Claude Code, mid-flight

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the cancelled request came back with a response")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the client never returned after cancel")
	}

	e := waitForEntry(t, rig.log)
	if e.Event == reqlog.EventRotatedConn {
		t.Errorf("request logged as %q — a client hanging up is not a connection failure",
			e.Event)
	}
	if e.Account != "(cancelled)" {
		t.Errorf("request logged against account %q, want %q — the account did nothing wrong",
			e.Account, "(cancelled)")
	}
	if e.Bytes != 0 {
		t.Errorf("wrote %d bytes back to a client that had gone away", e.Bytes)
	}
	if n := rig.countEvents(reqlog.EventRotatedConn); n != 0 {
		t.Errorf("published %d %s events for a cancelled request, want 0 — these are what "+
			"corrupt the rotation counts", n, reqlog.EventRotatedConn)
	}
	if n := rig.totalHits(); n != 1 {
		t.Errorf("%d upstream hits, want 1 — the cancelled request reached a second account", n)
	}
	rig.assertNoInFlight(t)
}

// The behaviour that must survive the fix: a dead upstream connection is
// still the account's problem, still rotates, and is still reported.
func TestGenuineDialFailureStillRotates(t *testing.T) {
	serve := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}
	rig := newCancelRig(t, [2]http.HandlerFunc{serve, serve})
	pinFirstDialFailure(rig.h, errors.New("dial tcp 127.0.0.1:1: connect: connection refused"))

	resp := postMessages(t, rig.front.URL, testBody)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (rotated to the healthy account); body %s",
			resp.StatusCode, body)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q", body)
	}

	e := waitForEntry(t, rig.log)
	if e.Event != reqlog.EventRotatedConn {
		t.Errorf("event = %q, want %q — a genuine dial failure must still be reported",
			e.Event, reqlog.EventRotatedConn)
	}
	if n := rig.countEvents(reqlog.EventRotatedConn); n != 1 {
		t.Errorf("published %d %s events, want exactly 1", n, reqlog.EventRotatedConn)
	}
	if n := rig.totalHits(); n != 1 {
		t.Errorf("%d upstream hits, want 1 (the dead account was never reached)", n)
	}
	rig.assertNoInFlight(t)
}

// The half of the gate that is not about the client: a context.Canceled that
// did NOT come from the client's context is spillway's own, and must rotate
// and be reported exactly like any other pre-first-byte failure. Matching on
// the error alone would drop this request silently — the client is still
// waiting, and would get nothing.
func TestSpillwaySideCancelStillRotates(t *testing.T) {
	serve := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}
	rig := newCancelRig(t, [2]http.HandlerFunc{serve, serve})
	// Shaped like a real one: net wraps ctx.Err() from the dialler.
	pinFirstDialFailure(rig.h, fmt.Errorf("dial tcp 127.0.0.1:1: %w", context.Canceled))

	resp := postMessages(t, rig.front.URL, testBody)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a cancellation the client did not cause must "+
			"still rotate, not be dropped; body %s", resp.StatusCode, body)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q", body)
	}
	e := waitForEntry(t, rig.log)
	if e.Event != reqlog.EventRotatedConn {
		t.Errorf("event = %q, want %q", e.Event, reqlog.EventRotatedConn)
	}
	if e.Account == "(cancelled)" {
		t.Error("a spillway-side cancellation was logged as a client cancellation")
	}
	if n := rig.countEvents(reqlog.EventRotatedConn); n != 1 {
		t.Errorf("published %d %s events, want exactly 1", n, reqlog.EventRotatedConn)
	}
	rig.assertNoInFlight(t)
}

// The classifier on its own, including the cases the integration tests above
// cannot reach: a client deadline, and an unrelated error arriving while the
// client's context happens to be done.
func TestClientGone(t *testing.T) {
	live, cancelLive := context.WithCancel(context.Background())
	defer cancelLive()
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"client cancelled, ctx error returned", cancelled, context.Canceled, true},
		{"client cancelled, wrapped ctx error", cancelled,
			fmt.Errorf("dial tcp: %w", context.Canceled), true},
		{"client deadline expired", expired,
			fmt.Errorf("dial tcp: %w", context.DeadlineExceeded), true},
		// The gate's second half: without it this row is true, and a
		// spillway-side cancel would be dropped instead of rotated.
		{"spillway-side cancel, client still waiting", live, context.Canceled, false},
		{"genuine dial failure, client still waiting", live,
			errors.New("connect: connection refused"), false},
		// A client can hang up in the same instant a connection genuinely
		// dies; that dial failure is still the account's story.
		{"genuine dial failure racing a client cancel", cancelled,
			errors.New("connect: connection refused"), false},
		{"no error at all", cancelled, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.NewRequestWithContext(tc.ctx, http.MethodPost, "http://x/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := clientGone(r, tc.err); got != tc.want {
				t.Errorf("clientGone = %v, want %v", got, tc.want)
			}
		})
	}
}

// The hold path (§6.11), the other place a vanished client could be
// mistaken for something an account did: a request parked on an exhausted
// pool whose client hangs up must be dropped, not rotated, and not answered.
func TestHoldCancelledByClientDoesNotRotate(t *testing.T) {
	up := httptest.NewServer(quota429WithReset(time.Now().Add(time.Hour)))
	t.Cleanup(up.Close)
	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "hold"
	cfg.Pool.HoldMax = "2h" // must cover the reset or the hold never engages
	p := pool.New([]*pool.Account{pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rl.Close() })
	broker := events.New()
	sub := broker.Subscribe()
	h.SetHooks(Hooks{Log: rl, Events: broker})
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		front.URL+"/v1/messages", strings.NewReader(testBody))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		done <- err
	}()

	// Poll for the park itself rather than sleeping: the request has to get
	// through selection and a 429 first.
	parked := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if n, _ := p.Holds(); n == 1 {
			parked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !parked {
		t.Fatal("the request never parked")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the cancelled request came back with a response")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hold did not release on client cancel")
	}

	e := waitForEntry(t, rl)
	if e.Account != "(cancelled)" {
		t.Errorf("held-then-cancelled request logged against account %q, want %q",
			e.Account, "(cancelled)")
	}
	if e.Bytes != 0 {
		t.Errorf("wrote %d bytes back to a client that had gone away", e.Bytes)
	}
	for {
		select {
		case ev := <-sub:
			if ev.Type == reqlog.EventRotatedConn {
				t.Fatalf("hold path published %s for a vanished client", reqlog.EventRotatedConn)
			}
			if ev.Type == reqlog.EventExhausted {
				t.Fatal("hold path reported pool exhaustion to a client that had gone away")
			}
			continue
		default:
		}
		break
	}
	if n := p.Accounts()[0].InFlight(); n != 0 {
		t.Errorf("account still reports %d in flight after the hold was cancelled", n)
	}
}
