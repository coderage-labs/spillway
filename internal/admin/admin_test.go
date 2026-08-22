package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

const testToken = "test-admin-token"

func newTestServer(t *testing.T) (*Server, *pool.Pool) {
	t.Helper()
	a := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.SetQuotaWindows([]pool.QuotaWindow{{Name: "5h", Limit: 1, Used: 0.42, Source: "headers", ResetAt: time.Now().Add(time.Hour), FetchedAt: time.Now()}})
	p := pool.New([]*pool.Account{a}, time.Now())
	return New(p, nil, events.New(), testToken), p
}

func authed(url string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	return req, nil
}

func TestTokenRequired(t *testing.T) {
	s, _ := newTestServer(t)
	front := httptest.NewServer(s)
	defer front.Close()

	// No token → 401.
	resp, err := http.Get(front.URL + "/api/accounts")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token status = %d, want 401", resp.StatusCode)
	}

	// Bearer token → 200.
	req, _ := authed(front.URL + "/api/accounts")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("bearer status = %d, want 200", resp.StatusCode)
	}

	// ?token= → 401 anywhere but the SSE stream, which is the only endpoint
	// with no way to send a header. See
	// TestQueryStringTokenOnlyWorksOnTheEventStream.
	resp, err = http.Get(front.URL + "/api/accounts?token=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("query-token status = %d, want 401", resp.StatusCode)
	}
}

func TestHostHeaderGuard(t *testing.T) {
	s, _ := newTestServer(t)
	front := httptest.NewServer(s)
	defer front.Close()

	req, _ := authed(front.URL + "/api/accounts")
	req.Host = "evil.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("evil host status = %d, want 403 (DNS-rebinding guard)", resp.StatusCode)
	}

	req, _ = authed(front.URL + "/api/accounts")
	req.Host = "localhost:7657"
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("localhost status = %d, want 200", resp.StatusCode)
	}
}

func TestMutatingOriginRejected(t *testing.T) {
	s, _ := newTestServer(t)
	front := httptest.NewServer(s)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/api/accounts", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST with Origin status = %d, want 403", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, front.URL+"/api/accounts", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST with Sec-Fetch-Site status = %d, want 403", resp.StatusCode)
	}
}

func TestAccountsShape(t *testing.T) {
	s, _ := newTestServer(t)
	front := httptest.NewServer(s)
	defer front.Close()

	req, _ := authed(front.URL + "/api/accounts")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got []accountJSON
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("accounts = %+v", got)
	}
	a := got[0]
	if a.Name != "work" || a.Type != "claude-oauth" || a.State != "ok" {
		t.Errorf("account = %+v", a)
	}
	if len(a.Windows) != 1 || a.Windows[0].Name != "5h" || a.Windows[0].Used != 0.42 || a.Windows[0].Source != "headers" {
		t.Errorf("windows = %+v", a.Windows)
	}
}

func TestRequestsEndpoint(t *testing.T) {
	s, _ := newTestServer(t)
	l, err := reqlog.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_ = l.Record(reqlog.Entry{Account: "work", Path: "/v1/messages", Status: 200, DurationMs: 5, Bytes: 10, Event: reqlog.EventServed})
	s.log = l

	front := httptest.NewServer(s)
	defer front.Close()
	req, _ := authed(front.URL + "/api/requests?limit=10")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got []reqlog.Entry
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Account != "work" || got[0].Event != reqlog.EventServed {
		t.Errorf("entries = %+v", got)
	}
}

func TestEventsSSE(t *testing.T) {
	s, _ := newTestServer(t)
	front := httptest.NewServer(s)
	defer front.Close()

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(front.URL + "/api/events?token=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	s.broker.Publish(events.Event{Type: "rotated-quota", Account: "work", Detail: "test"})
	buf := make([]byte, 512)
	n, err := resp.Body.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("read SSE: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "rotated-quota") {
		t.Errorf("SSE frame = %q", buf[:n])
	}
}
