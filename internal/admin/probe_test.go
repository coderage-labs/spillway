package admin

// Issue #192: POST /api/accounts/probe.
//
// The probe itself is internal/accounts' job and is tested there against a
// real upstream. What is tested here is the endpoint: that it reaches the
// probe with the right arguments, that it maps a money refusal the way
// /api/pin already maps one (409, body rendered by both surfaces), and —
// the part that is not about this feature at all — that it is reachable
// exactly the way the other admin mutations are and no other way.
//
// Every test wires a recording stub, so "the probe did not run" is an
// assertion about a counter rather than about the absence of a side effect.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

type probeCall struct {
	name  string
	force bool
}

// probeServer builds an admin server over the named accounts with a
// recording probe stub. reply decides what the stub returns; nil means a
// plain success.
func probeServer(t *testing.T, accts []*pool.Account, reply func(name string, force bool) (bool, error)) (*Server, *[]probeCall, *atomic.Int32) {
	t.Helper()
	p := pool.New(accts, time.Now())
	s := New(p, nil, events.New(), testToken)
	var calls []probeCall
	var n atomic.Int32
	s.EnableProbe(func(_ context.Context, name string, force bool) (bool, error) {
		calls = append(calls, probeCall{name, force})
		n.Add(1)
		if reply == nil {
			return false, nil
		}
		return reply(name, force)
	})
	return s, &calls, &n
}

func okAccount(name string) *pool.Account {
	a := pool.NewAccount(name, pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.07, Source: "headers",
			ResetAt: time.Now().Add(time.Hour), FetchedAt: time.Now()},
	})
	return a
}

// postProbe sends a well-formed, authenticated, same-origin POST — the
// request the dashboard and the CLI both make.
func postProbe(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7657/api/accounts/probe",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestProbeEndpointForcesAProbeOfTheNamedAccount(t *testing.T) {
	s, calls, n := probeServer(t, []*pool.Account{okAccount("work"), okAccount("spare")}, nil)

	rec := postProbe(t, s, `{"name":"spare"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if n.Load() != 1 {
		t.Fatalf("probe ran %d times, want exactly 1", n.Load())
	}
	if got := (*calls)[0]; got.name != "spare" || got.force {
		t.Errorf("probe called with %+v, want the named account and force=false", got)
	}

	var out accountProbeResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Account != "spare" {
		t.Errorf("account = %q, want the account that was probed", out.Account)
	}
	if out.Billed {
		t.Error("billed = true for a probe the stub reported as free")
	}
	// The reading it just took, so a caller does not have to poll
	// /api/accounts to see the answer to the question it asked.
	if len(out.Windows) != 1 || out.Windows[0].Name != "5h" {
		t.Errorf("quotaWindows = %+v, want the account's windows", out.Windows)
	}
}

// force has to reach the probe, or the money escape does not exist.
func TestProbeEndpointPassesForceThrough(t *testing.T) {
	s, calls, _ := probeServer(t, []*pool.Account{okAccount("work")},
		func(string, bool) (bool, error) { return true, nil })

	rec := postProbe(t, s, `{"name":"work","force":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if got := (*calls)[0]; !got.force {
		t.Fatalf("probe called with %+v, want force=true", got)
	}
	var out accountProbeResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Billed {
		t.Error("billed = false on a probe the stub reported as charged: the caller was never told it paid")
	}
}

// The money refusal, mapped exactly as /api/pin maps ErrPinWouldBill: 409,
// with the reason in the body. Both surfaces key on that pair — the CLI
// offers --force on a 409 and prints the body, the dashboard renders the
// body with a "probe anyway" button — so either half being wrong makes the
// refusal unusable.
func TestProbeEndpointMapsAChargeRefusalToA409WithTheReason(t *testing.T) {
	s, _, _ := probeServer(t, []*pool.Account{okAccount("work")},
		func(name string, force bool) (bool, error) {
			if force {
				return true, nil
			}
			return false, fmt.Errorf("%w: %q is out of quota and has extra usage permitted, so this probe is a charged request",
				pool.ErrProbeWouldBill, name)
		})

	rec := postProbe(t, s, `{"name":"work"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — a 400 tells the caller to give up and a 500 makes a deliberate refusal look like a fault", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "spend money") || !strings.Contains(body, "work") {
		t.Errorf("409 body = %q, want the reason and the account named", body)
	}

	// And the same request with force goes through, so the 409 is a gate and
	// not a wall.
	rec = postProbe(t, s, `{"name":"work","force":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("forced status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
}

// A probe that was attempted and did not complete is not the caller's fault
// and is not fixed by forcing, so it must not share the 409 that means
// "say it again and I will".
func TestProbeEndpointMapsAFailedProbeToA502(t *testing.T) {
	s, _, _ := probeServer(t, []*pool.Account{okAccount("work")},
		func(string, bool) (bool, error) {
			return false, fmt.Errorf(`probe of "work" failed: context deadline exceeded`)
		})

	rec := postProbe(t, s, `{"name":"work"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "deadline exceeded") {
		t.Errorf("body = %q, want the underlying reason", rec.Body.String())
	}
}

// An unknown name must be a 404 and must never reach the probe — a typo is
// not a reason to send traffic to whatever account sorted first.
func TestProbeEndpointRejectsAnUnknownAccountWithoutProbing(t *testing.T) {
	s, _, n := probeServer(t, []*pool.Account{okAccount("work")}, nil)

	rec := postProbe(t, s, `{"name":"nope"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if n.Load() != 0 {
		t.Fatalf("probe ran %d times for an unknown account, want 0", n.Load())
	}
}

// Parked and disabled are 400, not 409, and the difference is behavioural:
// both surfaces render a 409 as a "do it anyway" offer, and forcing cannot
// un-park an account.
func TestProbeEndpointRefusesParkedAndDisabledAccountsWithoutProbing(t *testing.T) {
	parked := okAccount("parked")
	parked.Park()
	dead := okAccount("dead")
	dead.Disable()
	s, _, n := probeServer(t, []*pool.Account{parked, dead}, nil)

	for _, name := range []string{"parked", "dead"} {
		rec := postProbe(t, s, `{"name":"`+name+`","force":true}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (409 would offer a retry that cannot work)", name, rec.Code)
		}
	}
	if n.Load() != 0 {
		t.Fatalf("probe ran %d times for accounts that cannot be probed, want 0", n.Load())
	}
}

func TestProbeEndpointRejectsAMissingNameAndAMalformedBody(t *testing.T) {
	s, _, n := probeServer(t, []*pool.Account{okAccount("work")}, nil)

	for _, body := range []string{`{}`, `{"name":""}`, `{`, `not json`} {
		rec := postProbe(t, s, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
	if n.Load() != 0 {
		t.Fatalf("probe ran %d times on malformed input, want 0", n.Load())
	}
}

// With no probe wired — a build or a test with no client to send one — the
// endpoint says so rather than answering 200 for a probe that never happened.
func TestProbeEndpointIs404WhenNoProbeIsWired(t *testing.T) {
	s, _ := newTestServer(t)
	rec := postProbe(t, s, `{"name":"work"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when probing is not enabled", rec.Code)
	}
}

// The part that is not about this feature: it must be reachable exactly the
// way /api/pin and /api/accounts/remove are, and no other way. A new
// mutation is the easiest place to open an auth hole, so each guard is
// asserted to BOTH refuse the request and leave the probe un-run.
func TestProbeEndpointIsReachableOnlyLikeTheOtherMutations(t *testing.T) {
	accts := []*pool.Account{okAccount("work")}

	t.Run("no token is 401", func(t *testing.T) {
		s, _, n := probeServer(t, accts, nil)
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7657/api/accounts/probe",
			strings.NewReader(`{"name":"work"}`))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		if n.Load() != 0 {
			t.Errorf("probe ran %d times for an unauthenticated request", n.Load())
		}
	})

	// The SSE stream is the ONE endpoint that may carry its token in the
	// query string, because EventSource cannot set a header. A mutation that
	// accepted one would put a live credential in browser history and in
	// every intermediary's access log.
	t.Run("a query-string token is 401", func(t *testing.T) {
		s, _, n := probeServer(t, accts, nil)
		req := httptest.NewRequest(http.MethodPost,
			"http://127.0.0.1:7657/api/accounts/probe?token="+testToken,
			strings.NewReader(`{"name":"work"}`))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		if n.Load() != 0 {
			t.Errorf("probe ran %d times for a query-string token", n.Load())
		}
	})

	// CSRF: the dashboard is a browser page, so a cross-site page could
	// otherwise drive this at the user's loopback port — and on an opted-in
	// account with --force that is a charge.
	t.Run("a cross-site write is 403", func(t *testing.T) {
		for _, site := range []string{"cross-site", "same-site"} {
			s, _, n := probeServer(t, accts, nil)
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7657/api/accounts/probe",
				strings.NewReader(`{"name":"work","force":true}`))
			req.Header.Set("Authorization", "Bearer "+testToken)
			req.Header.Set("Sec-Fetch-Site", site)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("Sec-Fetch-Site %q: status = %d, want 403", site, rec.Code)
			}
			if n.Load() != 0 {
				t.Errorf("Sec-Fetch-Site %q: probe ran %d times", site, n.Load())
			}
		}
	})

	t.Run("a foreign Origin is 403", func(t *testing.T) {
		s, _, n := probeServer(t, accts, nil)
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7657/api/accounts/probe",
			strings.NewReader(`{"name":"work"}`))
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
		if n.Load() != 0 {
			t.Errorf("probe ran %d times for a foreign Origin", n.Load())
		}
	})

	// DNS rebinding: a name that resolves to 127.0.0.1 but is not a loopback
	// Host is rejected before anything else looks at the request.
	t.Run("a non-loopback Host is 403", func(t *testing.T) {
		s, _, n := probeServer(t, accts, nil)
		req := httptest.NewRequest(http.MethodPost, "http://attacker.example/api/accounts/probe",
			strings.NewReader(`{"name":"work"}`))
		req.Host = "attacker.example"
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
		if n.Load() != 0 {
			t.Errorf("probe ran %d times for a rebound Host", n.Load())
		}
	})

	// GET is not a probe. Anything a browser can be made to issue by
	// navigation must not spend money or originate traffic.
	t.Run("GET is 405 and does not probe", func(t *testing.T) {
		s, _, n := probeServer(t, accts, nil)
		rec := getAuth(t, s, "/api/accounts/probe", testToken)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rec.Code)
		}
		if n.Load() != 0 {
			t.Errorf("probe ran %d times for a GET", n.Load())
		}
	})
}

// Sanity: the loopback listener is unauthenticated by design (see New), and
// this endpoint must follow that rather than inventing its own rule.
func TestProbeEndpointFollowsTheLoopbackNoTokenRule(t *testing.T) {
	p := pool.New([]*pool.Account{okAccount("work")}, time.Now())
	s := New(p, nil, events.New(), "")
	var n atomic.Int32
	s.EnableProbe(func(context.Context, string, bool) (bool, error) {
		n.Add(1)
		return false, nil
	})

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7657/api/accounts/probe",
		strings.NewReader(`{"name":"work"}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 on an unauthenticated loopback listener",
			rec.Code, rec.Body.String())
	}
	if n.Load() != 1 {
		t.Fatalf("probe ran %d times, want 1", n.Load())
	}
}
