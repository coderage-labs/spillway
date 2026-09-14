package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
)

// newLevelRig is newClaimRig with the daemon's live level knob: the same
// *slog.LevelVar the daemon holds (main.go builds one, reload.go's
// syncLogLevel retunes it), wired into the handler's real logger.
func newLevelRig(t *testing.T) (front *httptest.Server, log *syncBuffer,
	served *atomic.Int64, level *slog.LevelVar) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An unrecognised representative claim: a real production path
		// that logs at Info, so the test can tell "the per-request line is
		// gone" apart from "the logger is muted".
		w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "some_future_bucket_name")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Defaults()
	cfg.Upstream = upstream.URL
	cfg.Pool.ExhaustedMode = "fail"
	p := pool.New([]*pool.Account{pool.NewAccount("acct-a", pool.SourceYAML, "tok", "", 0, "")}, time.Now())

	log = &syncBuffer{}
	level = new(slog.LevelVar)
	level.Set(slog.LevelInfo) // config.Defaults().Log.Level
	logger := slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: level}))
	h, err := NewHandler(&cfg, logger, p)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	served = new(atomic.Int64)
	front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		served.Add(1)
	}))
	t.Cleanup(front.Close)
	return front, log, served, level
}

// serveOne posts one request and waits for the handler to finish with it,
// so anything serve() logs is already in the buffer when this returns.
func serveOne(t *testing.T, url string, served *atomic.Int64, log *syncBuffer, want int) {
	t.Helper()
	resp := postMessages(t, url, testBody)
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for served.Load() < int64(want) {
		if time.Now().After(deadline) {
			t.Fatalf("handler did not finish request %d; served=%d. log:\n%s",
				want, served.Load(), log.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Issue #171, both halves of the volume decision, on one running handler.
//
// At the default level there must be no line per request — that line was
// 99.5% of a 236 MB file and duplicates spillway-requests.db. Turning
// log.level up to debug must start it again on the daemon that is already
// running, and turning it back down must stop it: the live flip is the
// entire reason demoting it is acceptable rather than a loss. A restart
// would mean the tail is unavailable exactly when someone needs it, mid
// incident, on the daemon whose behaviour they are trying to watch.
func TestRequestLineFollowsTheLiveLevel(t *testing.T) {
	front, log, served, level := newLevelRig(t)

	// 1. Default level: no line per request — but not silent. The
	//    representative-claim line comes from production code at Info on
	//    this same request, so "no msg=request" here means the demotion is
	//    targeted rather than a muted logger.
	serveOne(t, front.URL, served, log, 1)
	if n := strings.Count(log.String(), "msg=request"); n != 0 {
		t.Fatalf("%d per-request lines at the default level; want none:\n%s", n, log.String())
	}
	if !strings.Contains(log.String(), "representative-claim") {
		t.Fatalf("the default level lost an operational Info line too; the demotion must be "+
			"the per-request line only:\n%s", log.String())
	}

	// 2. `log.level: debug` on the running daemon — exactly what
	//    reload.go's syncLogLevel does to this same LevelVar. No new
	//    handler, no new logger, no restart.
	level.Set(slog.LevelDebug)
	serveOne(t, front.URL, served, log, 2)
	if n := strings.Count(log.String(), "msg=request"); n != 1 {
		t.Fatalf("%d per-request lines after flipping to debug; want exactly the one request "+
			"served since the flip:\n%s", n, log.String())
	}

	// The tail has to be worth turning on: the line still carries what it
	// always did.
	out := log.String()
	for _, want := range []string{"method=POST", "path=/v1/messages", "status=200", "account=acct-a"} {
		if !strings.Contains(out, want) {
			t.Errorf("debug request line is missing %s:\n%s", want, out)
		}
	}

	// 3. Back down: it stops, on the same running handler.
	level.Set(slog.LevelInfo)
	serveOne(t, front.URL, served, log, 3)
	if n := strings.Count(log.String(), "msg=request"); n != 1 {
		t.Fatalf("%d per-request lines after flipping back to info; want the debug one only "+
			"— the flip has to stop it as well as start it:\n%s", n, log.String())
	}
}

// The level the rig starts at is the one the daemon ships with, not a
// level chosen to make the test pass. If config.Defaults().Log.Level ever
// becomes debug, the test above proves nothing about a real installation.
func TestRigStartsAtTheShippedDefaultLevel(t *testing.T) {
	_, _, _, level := newLevelRig(t)
	if got, want := level.Level().String(),
		strings.ToUpper(config.Defaults().Log.Level); got != want {
		t.Fatalf("rig level = %s; config.Defaults().Log.Level = %s", got, want)
	}
	if level.Level() != slog.LevelInfo {
		t.Fatalf("the shipped default level is %v; the per-request line is demoted to debug, "+
			"so a default of debug would put the 236 MB firehose straight back", level.Level())
	}
}
