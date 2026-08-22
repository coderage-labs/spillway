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
	"github.com/coderage-labs/spillway/internal/provider"
)

func pinServer(t *testing.T) (*httptest.Server, *pool.Pool) {
	t.Helper()
	a := pool.NewAccount("work", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	b := pool.NewAccount("side", pool.SourceYAML, "tok", "", 0, "")
	b.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a, b}, time.Now())
	front := httptest.NewServer(New(p, nil, events.New(), testToken))
	t.Cleanup(front.Close)
	return front, p
}

func callPin(t *testing.T, front *httptest.Server, method, body string) (int, string) {
	t.Helper()
	var r *strings.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	var req *http.Request
	var err error
	if r != nil {
		req, err = http.NewRequest(method, front.URL+"/api/pin", r)
	} else {
		req, err = http.NewRequest(method, front.URL+"/api/pin", nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	json.NewDecoder(resp.Body).Decode(new(any))
	return resp.StatusCode, sb.String()
}

func TestPinEndpointSetsAndClears(t *testing.T) {
	front, p := pinServer(t)

	if code, _ := callPin(t, front, http.MethodPost, `{"account":"side"}`); code != http.StatusOK {
		t.Fatalf("POST = %d", code)
	}
	if p.Pinned() != "side" {
		t.Errorf("pool not pinned: %q", p.Pinned())
	}
	if code, _ := callPin(t, front, http.MethodDelete, ""); code != http.StatusOK {
		t.Fatalf("DELETE = %d", code)
	}
	if p.Pinned() != "" {
		t.Errorf("still pinned after DELETE: %q", p.Pinned())
	}
}

// 409, not 400, for the two refusals --force overrides: the request is well
// formed and the answer may be different if the caller means it. A 400 tells
// a dashboard to give up rather than offer the choice.
func TestPinEndpointConflictsAreDistinguishable(t *testing.T) {
	front, p := pinServer(t)
	p.AllowOverage = true
	spent := p.Accounts()[1]
	spent.SetOverageForTest(provider.Overage{Known: true, Available: true})
	p.MarkExhausted(spent, time.Now().Add(time.Hour))

	if code, _ := callPin(t, front, http.MethodPost, `{"account":"side"}`); code != http.StatusConflict {
		t.Errorf("pinning at a billable account = %d, want 409", code)
	}
	if p.Pinned() != "" {
		t.Error("refused and pinned anyway")
	}
	if code, _ := callPin(t, front, http.MethodPost, `{"account":"side","force":true}`); code != http.StatusOK {
		t.Errorf("forced pin = %d, want 200", code)
	}
}

func TestPinEndpointRejectsNonsense(t *testing.T) {
	front, _ := pinServer(t)
	if code, _ := callPin(t, front, http.MethodPost, `{"account":"nobody"}`); code != http.StatusBadRequest {
		t.Errorf("unknown account = %d, want 400", code)
	}
	if code, _ := callPin(t, front, http.MethodPost, `not json`); code != http.StatusBadRequest {
		t.Errorf("malformed body = %d, want 400", code)
	}
	if code, _ := callPin(t, front, http.MethodPut, `{}`); code != http.StatusMethodNotAllowed {
		t.Errorf("PUT = %d, want 405", code)
	}
}

// A reader that does not know about the pin would otherwise report a pool
// "choosing" an account it was told to use.
func TestStateReportsThePin(t *testing.T) {
	front, p := pinServer(t)
	if err := p.Pin("side", false); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/api/state", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st stateJSON
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Pinned != "side" {
		t.Errorf("state.pinned = %q, want side", st.Pinned)
	}
}
