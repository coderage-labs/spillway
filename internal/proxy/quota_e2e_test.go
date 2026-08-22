package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// Predictive selection end-to-end: an account over the switch threshold is
// skipped while another eligible account serves.
func TestPredictiveSelectionEndToEnd(t *testing.T) {
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("over-threshold account served while another was eligible")
		},
		func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true}`) },
	})
	rig.pool.Accounts()[0].SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.995, Source: "headers", FetchedAt: time.Now()},
	})

	resp := postMessages(t, rig.front.URL, testBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if rig.hits[0].Load() != 0 || rig.hits[1].Load() != 1 {
		t.Errorf("hits = (%d, %d), want (0, 1)", rig.hits[0].Load(), rig.hits[1].Load())
	}
}

// Request-log hook: a served request lands in the log with its event.
func TestRequestLogHook(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	cfg := config.Defaults()
	cfg.Upstream = upstream.URL
	cfg.Pool.ExhaustedMode = "fail"
	p := pool.New([]*pool.Account{pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	h.SetHooks(Hooks{Log: rl, Events: events.New()})
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, testBody)
	resp.Body.Close()

	e := waitForEntry(t, rl)
	if e.Account != "a" || e.Path != "/v1/messages" || e.Status != 200 || e.Event != reqlog.EventServed || e.Bytes == 0 {
		t.Errorf("entry = %+v", e)
	}
}

// Issue #14: a request that rotates off a quota-429 and then ends on one of
// the early writeCaptured returns (not the streamed success path) must still
// log the model the SERVING account actually sent upstream. The account that
// classified this response as terminal is a Kimi account with a ModelMap, so
// if modelServed still equalled modelAsked here, the fix regressed.
func TestModelServedRecordsMappedModelOnEarlyReturn(t *testing.T) {
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		quota429(w) // rotates away — A never gets to answer
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 403 with no quota/billing wording: kimi.go classifies this ErrNone,
		// which proxy.go treats as terminal — writeCaptured, not a rotation.
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"blocked"}`)
	}))
	defer upstreamB.Close()

	acctA := pool.NewAccount("a", pool.SourceYAML, "tok-a", "", 0, upstreamA.URL)
	acctB := pool.NewAccount("b", pool.SourceYAML, "tok-b", "", 0, upstreamB.URL)
	acctB.Type = "kimi-oauth"
	acctB.ModelMap = map[string]string{"claude-sonnet-4-6": "k3[1m]"}
	// Priority forces selection order: A must be tried (and rotated off of)
	// before B, or this never exercises the rotate-then-early-return path.
	acctA.SetPriority(0)
	acctB.SetPriority(1)
	p := pool.New([]*pool.Account{acctA, acctB}, time.Now())
	// A and B are different providers (default vs kimi-oauth) — this is
	// exactly the cross-provider rotation §6.18 has in mind, so it must be
	// allowed or SelectExcept keeps the session pinned to A's provider and
	// B is never reached. NewHandler doesn't read this off cfg — only
	// cmd/spillway wires it — so it must be set on the pool directly.
	// Through Apply, not a field write: #13 unexported these so the selector
	// and the dashboard cannot race over them.
	p.Apply(pool.Settings{CrossProvider: true})

	rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	cfg := config.Defaults()
	cfg.Pool.ExhaustedMode = "fail"
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	h.SetHooks(Hooks{Log: rl, Events: events.New()})
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (kimi terminal passthrough); body %s", resp.StatusCode, body)
	}

	e := waitForEntry(t, rl)
	if e.Account != "b" {
		t.Fatalf("account = %q, want b to have served (a should have rotated off on quota-429)", e.Account)
	}
	if e.ModelAsked != "claude-sonnet-4-6" {
		t.Fatalf("ModelAsked = %q", e.ModelAsked)
	}
	if e.ModelServed != "k3[1m]" {
		t.Errorf("ModelServed = %q, want %q (the mapped model B actually received) — issue #14",
			e.ModelServed, "k3[1m]")
	}

	// Neither account actually served a completion: A was rotated off on a
	// quota-429 before any response reached the client, and B's response was
	// a passed-through 403 error, not an answer. SetLastModel must stay
	// empty for both — an account claiming a "last model served" it did not
	// serve is exactly the failure mode this issue warns against for the
	// dashboard's "last model" column.
	if lm := acctA.LastModel(); lm != "" {
		t.Errorf("A.LastModel() = %q, want empty — A was rotated away from, it never served this", lm)
	}
	if lm := acctB.LastModel(); lm != "" {
		t.Errorf("B.LastModel() = %q, want empty — B returned a terminal error, not a served completion", lm)
	}
}

// waitForEntry polls for the log write.
//
// The record happens after the response is flushed: net/http sends the body
// as the handler writes it, so the client can have its answer and be
// asserting while serve() has not reached Record yet. Asserting immediately
// passed on a quiet laptop and failed on a loaded CI runner, which is the
// worst possible way for a test to be wrong.
func waitForEntry(t *testing.T, rl *reqlog.Log) reqlog.Entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := rl.Recent(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 1 {
			return entries[0]
		}
		if len(entries) > 1 {
			t.Fatalf("one request produced %d log entries: %+v", len(entries), entries)
		}
		if time.Now().After(deadline) {
			t.Fatal("the served request never reached the request log")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
