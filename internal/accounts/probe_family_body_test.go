package accounts

// Constraint from the repo owner (issue #229): the family probe must reuse
// probeBody exactly — max_tokens: 1, one-character message, no thinking
// parameter, no system prompt, no tools — with only the model name
// substituted. This is already true by construction: probeRejectedFamilies
// calls sendProbeRequest, the exact same helper probeOne itself uses, which
// builds its request body as fmt.Sprintf(probeBody, model) and nothing
// else. This test pins that down as a byte-for-byte assertion, so nobody
// can later widen the family probe's body (a thinking block, a bigger
// max_tokens) without this failing.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

func TestFamilyProbeBodyIsExactlyProbeBodyWithModelSubstituted(t *testing.T) {
	now := time.Now()
	a := claudeAccount("work")
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.1, Source: "headers", ResetAt: now.Add(2 * time.Hour), FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 0.2, Source: "headers", ResetAt: now.Add(48 * time.Hour), FetchedAt: now},
	})
	p0 := pool.New([]*pool.Account{a}, now)
	fableModel := fableFamilyModel(t, p0, a)
	want := fmt.Sprintf(probeBody, fableModel)

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading probe request body: %v", err)
		}
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(b, &payload); err != nil {
			t.Fatalf("unmarshalling probe request body %q: %v", b, err)
		}
		if payload.Model == fableModel {
			gotBody = b
		}
		healthyWindowsHandler(w, r)
	}))
	t.Cleanup(srv.Close)

	a.Upstream = srv.URL
	p := pool.New([]*pool.Account{a}, now)
	p.MarkWindowRejected(a, "7d-fable", now.Add(time.Hour))

	if _, err := ProbeNow(t.Context(), p, srv.Client(), srv.URL, 30*time.Minute, "work", false, quietLogger()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}

	if gotBody == nil {
		t.Fatal("the family probe never ran — nothing to assert the body of")
	}
	if string(gotBody) != want {
		t.Errorf("family probe body = %q, want %q — it must be exactly probeBody with the family "+
			"model substituted: no thinking block, no larger max_tokens, no system prompt, no tools",
			gotBody, want)
	}
}
