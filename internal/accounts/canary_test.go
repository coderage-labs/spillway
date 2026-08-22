package accounts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

func canaryFor(t *testing.T, h http.HandlerFunc, idle time.Duration) (*Canary, *pool.Account, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)

	a := pool.NewAccount("acct", pool.SourceYAML, "tok", "", 0, srv.URL)
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, time.Now())
	return &Canary{Pool: p, Client: srv.Client(), Upstream: srv.URL,
		Logger: quietLogger(), Idle: idle}, a, &hits
}

// A revoked credential surfaces without any request of the user's failing —
// that is the whole point.
func TestCanaryAlarmsOnAuthFailure(t *testing.T) {
	c, _, _ := canaryFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}, 0)
	res := c.Run(context.Background())
	if len(res) != 1 {
		t.Fatalf("want 1 result, got %d", len(res))
	}
	if !res[0].Alarm {
		t.Errorf("a 401 must alarm: %+v", res[0])
	}
}

// A transient failure says nothing about the account and must not cry wolf —
// an alarm that fires on a flaky network gets ignored.
func TestCanaryDoesNotAlarmOnTransientFailure(t *testing.T) {
	c, _, _ := canaryFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}, 0)
	res := c.Run(context.Background())
	if res[0].Alarm {
		t.Errorf("a 500 must not alarm: %+v", res[0])
	}
	if res[0].Err == nil {
		t.Error("a 500 should still be reported, just not as an alarm")
	}
}

func TestCanarySilentWhenHealthy(t *testing.T) {
	c, _, _ := canaryFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.1")
		w.WriteHeader(http.StatusOK)
	}, 0)
	if res := c.Run(context.Background()); res[0].Err != nil || res[0].Alarm {
		t.Errorf("healthy account should be silent: %+v", res[0])
	}
}

// An account in active use needs no canary: its real traffic is the check,
// and a synthetic request would be pure waste.
func TestCanarySkipsRecentlyUsedAccounts(t *testing.T) {
	c, a, hits := canaryFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, time.Hour)
	a.SetQuotaWindows([]pool.QuotaWindow{{Name: "5h", Limit: 1, Used: 0.2, FetchedAt: time.Now()}})

	c.Run(context.Background())
	if hits.Load() != 0 {
		t.Errorf("checked an account used moments ago (%d requests)", hits.Load())
	}
}

func TestCanaryChecksStaleAccounts(t *testing.T) {
	c, a, hits := canaryFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, time.Hour)
	a.SetQuotaWindows([]pool.QuotaWindow{{Name: "5h", Limit: 1, Used: 0.2,
		FetchedAt: time.Now().Add(-3 * time.Hour)}})

	c.Run(context.Background())
	if hits.Load() != 1 {
		t.Errorf("a stale account should be checked, got %d requests", hits.Load())
	}
}

// Parked and disabled accounts are not expected to work, so checking them
// would produce alarms the user deliberately caused.
func TestCanarySkipsParkedAndDisabled(t *testing.T) {
	c, a, hits := canaryFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}, 0)
	a.Park()
	if res := c.Run(context.Background()); len(res) != 0 {
		t.Errorf("parked account was checked: %+v", res)
	}
	a.Unpark()
	a.Disable()
	if res := c.Run(context.Background()); len(res) != 0 {
		t.Errorf("disabled account was checked: %+v", res)
	}
	if hits.Load() != 0 {
		t.Errorf("made %d requests for accounts that are not in service", hits.Load())
	}
}
