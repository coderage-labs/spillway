package admin

// Issue #209: /api/accounts fed the dashboard tanks (and the figures table
// built from the same payload) in Pool.Accounts() order, which is just
// config order. The dashboard reads the list top-to-bottom as the pool's
// preference, so it has to match better() (pool.go) — lower Priority wins,
// ties broken on Name rather than load, since load churns every poll and
// would reshuffle the tanks continuously instead of only when priority
// actually changes.

import (
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

func TestAccountsOrdersByPriorityThenName(t *testing.T) {
	// Built in an order that matches neither priority nor name, so a test
	// that merely preserved input order would still pass.
	zebra := pool.NewAccount("zebra", pool.SourceYAML, "tok", "", 0, "")
	zebra.SetPriority(1)
	alpha := pool.NewAccount("alpha", pool.SourceYAML, "tok", "", 0, "")
	alpha.SetPriority(2)
	middle := pool.NewAccount("middle", pool.SourceYAML, "tok", "", 0, "")
	middle.SetPriority(0)
	// Two accounts sharing a priority: tie breaks on name, not on the order
	// they were built in.
	bravo := pool.NewAccount("bravo", pool.SourceYAML, "tok", "", 0, "")
	bravo.SetPriority(1)

	p := pool.New([]*pool.Account{zebra, alpha, middle, bravo}, time.Now())
	s := New(p, nil, events.New(), testToken)

	got := fetchAccountsJSON(t, s)
	var names []string
	for _, a := range got {
		names = append(names, a.Name)
	}
	want := []string{"middle", "bravo", "zebra", "alpha"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}
}

// Issue #254: /api/accounts carries the server-computed bucket.
func TestAccountsReportBucket(t *testing.T) {
	ok := pool.NewAccount("ok", pool.SourceYAML, "tok", "", 0, "")
	ok.Type = "claude-oauth"
	spent := pool.NewAccount("spent", pool.SourceYAML, "tok", "", 0, "")
	spent.Type = "claude-oauth"
	dead := pool.NewAccount("dead", pool.SourceYAML, "tok", "", 0, "")
	dead.Type = "claude-oauth"
	dead.Disable()
	parked := pool.NewAccount("parked", pool.SourceYAML, "tok", "", 0, "")
	parked.Type = "claude-oauth"
	parked.Park()
	fableOnly := pool.NewAccount("fableonly", pool.SourceYAML, "tok", "", 0, "")
	fableOnly.Type = "claude-oauth"

	p := pool.New([]*pool.Account{ok, spent, dead, parked, fableOnly}, time.Now())
	until := time.Now().Add(time.Hour)
	p.MarkWindowRejected(spent, "5h", until)
	p.MarkWindowRejected(spent, "7d-fable", until)
	p.MarkWindowRejected(fableOnly, "7d-fable", until)
	s := New(p, nil, events.New(), testToken)

	want := map[string]string{"ok": "available", "spent": "spent", "dead": "attention",
		"parked": "attention", "fableonly": "available"}
	for _, a := range fetchAccountsJSON(t, s) {
		if a.Bucket != want[a.Name] {
			t.Errorf("%s bucket = %q, want %q", a.Name, a.Bucket, want[a.Name])
		}
		delete(want, a.Name)
	}
	if len(want) != 0 {
		t.Fatalf("accounts missing from response: %v", want)
	}
}
