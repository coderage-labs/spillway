package pool

// Issue #151: a cached provider overage refusal blocks the request that
// would lift it.
//
// The loop these tests close: CanOverage answers the provider's question
// before the operator's, so a stored "extra usage is not available" outranks
// allowOverage — correctly, the provider does have the last word. But
// selection needs CanOverage true to send anything, and only a sent request
// brings fresh headers, so the reading that says "do not bother asking" is
// the one reading only asking can clear. Observed live on 2026-09-04: hours
// of 429s after the user re-enabled a credit limit upstream, cleared only by
// restarting the daemon.
//
// Every test here is also a money test. Expiring a refusal is the direction
// that can SPEND, so the rules that must survive it are: an account nobody
// singled out never becomes billable, a fresh refusal is still obeyed, and a
// reading of unknown age is never treated as an old one.

import (
	"net/http"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/provider"
)

// refusedAccount builds an opted-in account whose quota is gone and whose
// provider refused extra usage `age` ago.
func refusedAccount(t *testing.T, age time.Duration) (*Pool, *Account) {
	t.Helper()
	now := time.Now()
	a := NewAccount("refused", SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.allowOverage = boolp(true) // the operator's explicit "spend on this one"
	a.setOverage(provider.Overage{
		Known: true, Available: false, Reason: "member_zero_credit_limit",
	}, now.Add(-age))
	p := New([]*Account{a}, now)
	p.MarkExhausted(a, now.Add(4*time.Hour))
	return p, a
}

// The bug. A refusal older than the TTL stops suppressing selection, so the
// request that could bring a fresh answer can actually go out.
func TestStaleOverageRefusalStopsSuppressingSelection(t *testing.T) {
	p, a := refusedAccount(t, overageRefusalTTL+time.Minute)

	if !a.CanOverage(p.AllowOverage()) {
		t.Fatal("a stale refusal still refuses: the reading that suppresses the only " +
			"request able to replace it is believed forever (issue #151)")
	}
	got := p.SelectFor("s", nil)
	if got != a {
		t.Fatalf("SelectFor = %v, want the opted-in account — nothing is ever sent, "+
			"so no fresh overage header can ever arrive", got)
	}
	p.Done(got)
}

// The other half. A refusal spillway heard a minute ago is still obeyed —
// expiring immediately would hammer a provider that genuinely refuses.
func TestFreshOverageRefusalIsStillHonoured(t *testing.T) {
	p, a := refusedAccount(t, time.Minute)

	if a.CanOverage(p.AllowOverage()) {
		t.Error("a refusal one minute old was ignored — the provider's answer is " +
			"still current and re-asking is pure waste")
	}
	if got := p.SelectFor("s", nil); got != nil {
		t.Errorf("selected %s despite a fresh provider refusal", got.Name)
	}
}

// The exact boundary, both sides, with the clock injected so the test is
// about the rule rather than about scheduling.
func TestOverageRefusalExpiresOnlyPastTheTTL(t *testing.T) {
	base := time.Now()
	a := NewAccount("refused", SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.allowOverage = boolp(true)
	a.setOverage(provider.Overage{Known: true, Available: false}, base)

	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"the moment it was read", base, false},
		{"one tick before the TTL", base.Add(overageRefusalTTL - time.Nanosecond), false},
		{"exactly at the TTL", base.Add(overageRefusalTTL), false},
		{"one tick past the TTL", base.Add(overageRefusalTTL + time.Nanosecond), true},
	} {
		if got := a.canOverageAt(true, tc.at); got != tc.want {
			t.Errorf("%s: CanOverage = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Never spend uninvited. Expiry hands the decision back to the operator's
// own setting, and a pool-wide default is not the operator singling this
// account out — CanOverage requires a confirmed header for that, and an
// expired refusal is not one.
func TestExpiredRefusalNeverMakesAnUninvitedAccountBillable(t *testing.T) {
	stale := time.Now().Add(-(overageRefusalTTL + time.Hour))

	for _, tc := range []struct {
		name       string
		allow      *bool // the per-account override
		poolAllows bool
		want       bool
	}{
		{"no opt-in at all, pool default off", nil, false, false},
		{"no opt-in at all, pool default ON", nil, true, false},
		{"the account explicitly opted OUT of a pool-wide yes", boolp(false), true, false},
		{"the account explicitly opted in", boolp(true), false, true},
	} {
		a := NewAccount("a", SourceYAML, "tok", "", 0, "")
		a.Type = "claude-oauth"
		a.allowOverage = tc.allow
		a.setOverage(provider.Overage{
			Known: true, Available: false, Reason: "member_zero_credit_limit",
		}, stale)
		if got := a.CanOverage(tc.poolAllows); got != tc.want {
			t.Errorf("%s: CanOverage = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A zero FetchedAt is an unknown age, not an infinite one. Treating unknown
// as expired is precisely how issue #34 came to buy a request on every
// daemon restart, and this is the direction that spends.
func TestOverageRefusalOfUnknownAgeNeverExpires(t *testing.T) {
	a := NewAccount("seeded", SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.allowOverage = boolp(true)
	// Deliberately bypassing setOverage, which always stamps: this is the
	// shape a future persisted or hand-built reading could arrive in.
	a.overage = provider.Overage{Known: true, Available: false, Reason: "member_zero_credit_limit"}

	if a.canOverageAt(true, time.Now().Add(100*24*time.Hour)) {
		t.Error("a refusal with no recorded measurement time was treated as old enough " +
			"to expire — an unknown age is not grounds to spend")
	}
}

// Only the refusal expires. A stale "extra usage IS available" is left
// alone: acting on it costs at most a free rejection, while expiring it
// would drop real paid capacity out of the last-resort tier for no observed
// problem.
func TestStaleAvailabilityIsNotExpired(t *testing.T) {
	a := NewAccount("available", SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	a.setOverage(provider.Overage{Known: true, Available: true},
		time.Now().Add(-(overageRefusalTTL + 24*time.Hour)))

	if !a.CanOverage(true) {
		t.Error("a stale AVAILABLE reading was expired too — that removes paid " +
			"capacity the pool was told it may use")
	}
}

// The reason is diagnostic gold: "member_zero_credit_limit" was the entire
// answer to "why is my overage not working". Expiry must not eat it.
func TestExpiredRefusalKeepsTheProvidersReason(t *testing.T) {
	p, a := refusedAccount(t, overageRefusalTTL+time.Hour)

	if got := a.Overage().Reason; got != "member_zero_credit_limit" {
		t.Errorf("Overage().Reason = %q after expiry, want it preserved", got)
	}
	if !a.Overage().Known {
		t.Error("the stored reading was overwritten by the expiry — Overage() must " +
			"keep reporting what the provider actually said")
	}
	// ...but the refusal itself is no longer one spillway is acting on, so a
	// surface must not present it as live.
	if _, refused := a.OverageRefusal(); refused {
		t.Error("OverageRefusal still reports a live refusal that CanOverage has " +
			"already stopped believing — two surfaces, two answers")
	}
	if !a.CanOverage(p.AllowOverage()) {
		t.Error("preserving the reason resurrected the refusal")
	}
}

// The surface contract, both states.
func TestOverageRefusalReportsTheLiveState(t *testing.T) {
	fresh := NewAccount("fresh", SourceYAML, "tok", "", 0, "")
	fresh.setOverage(provider.Overage{
		Known: true, Available: false, Reason: "member_zero_credit_limit",
	}, time.Now())
	reason, refused := fresh.OverageRefusal()
	if !refused || reason != "member_zero_credit_limit" {
		t.Errorf("fresh refusal: got (%q, %v), want (member_zero_credit_limit, true)",
			reason, refused)
	}

	never := NewAccount("never-used", SourceYAML, "tok", "", 0, "")
	if _, refused := never.OverageRefusal(); refused {
		t.Error("an account that has never responded reported a refusal — unknown " +
			"is not no")
	}

	ok := NewAccount("ok", SourceYAML, "tok", "", 0, "")
	ok.setOverage(provider.Overage{Known: true, Available: true}, time.Now())
	if _, refused := ok.OverageRefusal(); refused {
		t.Error("an account with extra usage available reported a refusal")
	}
}

// The pool-wide summary the exhausted-pool message is built from: distinct,
// sorted, reasons only, and silent about refusals that have expired.
func TestOverageRefusalsSummarisesTheLiveReasons(t *testing.T) {
	now := time.Now()
	mk := func(name, reason string, age time.Duration, available bool) *Account {
		a := NewAccount(name, SourceYAML, "tok", "", 0, "")
		a.Type = "claude-oauth"
		a.setOverage(provider.Overage{
			Known: true, Available: available, Reason: reason,
		}, now.Add(-age))
		return a
	}
	twins1 := mk("twin-1", "member_zero_credit_limit", time.Minute, false)
	twins2 := mk("twin-2", "member_zero_credit_limit", time.Minute, false)
	other := mk("other", "admin_disabled", time.Minute, false)
	silent := mk("silent", "", time.Minute, false)
	expired := mk("expired", "gone_stale", overageRefusalTTL+time.Hour, false)
	fine := mk("fine", "", time.Minute, true)
	unknown := NewAccount("unknown", SourceYAML, "tok", "", 0, "")

	p := New([]*Account{twins1, twins2, other, silent, expired, fine, unknown}, now)
	got := p.OverageRefusals()
	want := []string{"admin_disabled", "member_zero_credit_limit", "no reason given"}
	if len(got) != len(want) {
		t.Fatalf("OverageRefusals() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("OverageRefusals() = %v, want %v", got, want)
		}
	}
}

// The end of the live scenario: once a request finally goes out and the
// provider says extra usage is permitted again, the account is back to
// ordinary billable service — no restart needed.
func TestAFreshHeaderAfterExpiryRestoresNormalBehaviour(t *testing.T) {
	p, a := refusedAccount(t, overageRefusalTTL+time.Minute)
	if !a.CanOverage(p.AllowOverage()) {
		t.Fatal("precondition: the stale refusal should already have expired")
	}

	// What the response to that now-permitted request carries.
	p.RecordQuota(a, http.Header{
		"Anthropic-Ratelimit-Unified-Overage-Status": []string{"allowed"},
	}, time.Now())

	ov := a.Overage()
	if !ov.Known || !ov.Available {
		t.Fatalf("overage after a permissive header = %+v, want known and available", ov)
	}
	if ov.Reason != "" {
		t.Errorf("Reason = %q, want it cleared by the new reading", ov.Reason)
	}
	if _, refused := a.OverageRefusal(); refused {
		t.Error("still reporting a refusal after the provider permitted extra usage")
	}
	if got := p.SelectFor("s", nil); got != a {
		t.Fatalf("SelectFor = %v, want the recovered account back in the paid tier", got)
	}
	p.Done(a)
}
