package proxy

import (
	"fmt"
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
