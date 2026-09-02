package admin

// The dashboard's pin button posts to the same handler as `spillway switch`,
// so the provider-side money refusal (issue #139) has to arrive there as a
// 409 carrying a readable reason — that is what showPinConflict renders next
// to its "pin anyway" button. A 400 would tell the dashboard to give up, and
// a 500 would give it nothing to say.

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
)

// callPinBody is callPin with the response body, which callPin discards.
func callPinBody(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/api/pin", strings.NewReader(body))
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
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestPinEndpointSurfacesTheProviderSideRefusal(t *testing.T) {
	front, p := pinServer(t)
	spent := p.Accounts()[1]
	spent.SetQuotaWindows([]pool.QuotaWindow{{
		Name: "5h", Limit: 1, Used: 1, Source: "headers",
		FetchedAt: time.Now(), ResetAt: time.Now().Add(time.Hour),
	}})
	spent.SetOverageForTest(provider.Overage{Known: true, Available: true})

	// Spillway's own overage stays off: the whole point of #139 is that this
	// is not enough to stop the provider billing a pinned request.
	if p.AllowOverage() {
		t.Fatal("test setup: pool overage is on")
	}

	code, body := callPinBody(t, front.URL, `{"account":"side"}`)
	if code != http.StatusConflict {
		t.Errorf("POST = %d, want 409 so the dashboard can offer \"pin anyway\"", code)
	}
	if p.Pinned() != "" {
		t.Error("refused and pinned anyway")
	}
	for _, want := range []string{"extra usage", "claude", "overage"} {
		if !strings.Contains(body, want) {
			t.Errorf("409 body does not mention %q: %s", want, body)
		}
	}

	code, body = callPinBody(t, front.URL, `{"account":"side","force":true}`)
	if code != http.StatusOK {
		t.Errorf("forced pin = %d (%s), want 200", code, body)
	}
	if p.Pinned() != "side" {
		t.Errorf("Pinned() = %q after a forced pin", p.Pinned())
	}
}
