package accounts

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

// A kimi /usages payload in the verified live shape: weekly + 5h + parallel.
const usagesBody = `{
	"usage": {"limit": 100, "used": 96, "resetTime": "2026-08-23T00:00:00Z"},
	"limits": [{"window": {"duration": 300, "timeUnit": "TIME_UNIT_MINUTE"},
	            "detail": {"limit": 100, "used": 40, "resetTime": "2026-08-22T18:00:00Z"}}],
	"parallel": {"limit": 4}
}`

// Polling repeatedly must leave the window count where it started.
//
// The live daemon had a kimi account at 105 windows — 35 copies of each — and
// growing by three a minute. OverThreshold scans every window it holds, so a
// stale copy recording a spent quota would keep the account out of rotation
// long after the real window reset.
func TestRepeatedPollsDoNotAccumulateWindows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usages" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		io.WriteString(w, usagesBody)
	}))
	defer srv.Close()

	a := pool.NewAccount("kimi", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "kimi-oauth"
	a.Upstream = srv.URL
	p := pool.New([]*pool.Account{a}, time.Now())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var counts []int
	for i := 0; i < 4; i++ {
		PollKimiUsages(context.Background(), p, logger)
		counts = append(counts, len(a.QuotaWindows()))
	}
	for i, n := range counts {
		if n != 3 {
			t.Fatalf("poll %d left %d windows, want 3 (counts across polls: %v)", i+1, n, counts)
		}
	}
}
