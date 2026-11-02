package accounts

import (
	"context"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// Issue #262: an Ultra account's last 5h sample sits in quota_samples with a
// reset in the past. A restart must not resurrect it; the live 7d still seeds.
func TestSeedQuotaDoesNotResurrectAStaleFiveHourWindow(t *testing.T) {
	l := openTestLog(t)
	now := time.Now()
	a := pool.NewAccount("ultra", pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	p := pool.New([]*pool.Account{a}, now)

	for _, s := range []reqlog.Sample{
		{Ts: now.Add(-40 * time.Hour), Account: "ultra", Window: "5h", Limit: 1, Used: 0.25, ResetAt: now.Add(-38 * time.Hour)},
		{Ts: now.Add(-time.Minute), Account: "ultra", Window: "7d", Limit: 1, Used: 0.27, ResetAt: now.Add(4 * 24 * time.Hour)},
	} {
		if err := l.RecordQuota(s); err != nil {
			t.Fatal(err)
		}
	}

	SeedQuota(context.Background(), p, l, now, quietLogger())

	var names []string
	for _, w := range a.QuotaWindows() {
		names = append(names, w.Name)
	}
	if len(names) != 1 || names[0] != "7d" {
		t.Errorf("seeded windows = %v, want only [7d]", names)
	}
}
