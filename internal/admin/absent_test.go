package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

// Issue #262: /api/accounts says which windows the plan does not have, and a
// never-measured account says nothing (omitted, not an empty claim).
func TestAccountsExposeAbsentWindows(t *testing.T) {
	ultra := pool.NewAccount("ultra", pool.SourceYAML, "tok", "", 0, "")
	ultra.Type = "claude-oauth"
	fresh := pool.NewAccount("fresh", pool.SourceYAML, "tok", "", 0, "")
	fresh.Type = "claude-oauth"
	now := time.Now()
	p := pool.New([]*pool.Account{ultra, fresh}, now)

	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "allowed")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.27")
	h.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(now.Add(96*time.Hour).Unix(), 10))
	p.RecordQuota(ultra, h, now)

	front := httptest.NewServer(New(p, nil, events.New(), testToken))
	defer front.Close()
	req, err := authed(front.URL + "/api/accounts")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw []map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	by := map[string]map[string]json.RawMessage{}
	for _, r := range raw {
		var n string
		_ = json.Unmarshal(r["name"], &n)
		by[n] = r
	}
	if got := string(by["ultra"]["absentWindows"]); got != `["5h"]` {
		t.Errorf("ultra absentWindows = %s, want [\"5h\"]", got)
	}
	if v, ok := by["fresh"]["absentWindows"]; ok {
		t.Errorf("never-measured account claims absentWindows = %s", v)
	}
}
