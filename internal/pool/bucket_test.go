package pool

import (
	"testing"
	"time"
)

// Issue #254: every account is in exactly one bucket, decided by routing's
// own predicates.
func TestBucketOf(t *testing.T) {
	now := time.Now()
	until := now.Add(time.Hour)

	for _, tc := range []struct {
		name  string
		setup func(p *Pool, a *Account)
		want  Bucket
	}{
		{"healthy", func(*Pool, *Account) {}, BucketAvailable},
		{"fable-only rejected stays available", func(p *Pool, a *Account) {
			p.MarkWindowRejected(a, "7d-fable", until)
		}, BucketAvailable},
		{"every family rejected is spent", func(p *Pool, a *Account) {
			p.MarkWindowRejected(a, "7d-fable", until)
			p.MarkWindowRejected(a, "5h", until)
		}, BucketSpent},
		{"general window below the spill point is spent, though not refused", func(p *Pool, a *Account) {
			a.SetQuotaWindows([]QuotaWindow{{Name: "7d", Limit: 1, Used: 0.99, Source: "headers", FetchedAt: now}})
		}, BucketSpent},
		{"fable-only below the spill point stays available", func(p *Pool, a *Account) {
			a.SetQuotaWindows([]QuotaWindow{{Name: "7d-fable", Limit: 1, Used: 0.99, Source: "headers", FetchedAt: now}})
		}, BucketAvailable},
		{"exhausted is spent", func(p *Pool, a *Account) {
			p.MarkExhausted(a, until)
		}, BucketSpent},
		{"auth failure is attention", func(_ *Pool, a *Account) { a.Disable() }, BucketAttention},
		{"parked is attention", func(_ *Pool, a *Account) { a.Park() }, BucketAttention},
		{"disabled AND spent is attention only", func(p *Pool, a *Account) {
			p.MarkWindowRejected(a, "5h", until)
			a.Disable()
		}, BucketAttention},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := claudeAccountPrio("a", 0)
			p := New([]*Account{a}, now)
			tc.setup(p, a)
			if got := p.BucketOf(a); got != tc.want {
				t.Fatalf("BucketOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// The bucket must agree with FamilyHasCapacity, the predicate it was built
// from: an account is Available iff, alone in the pool, one family has
// capacity.
func TestBucketAgreesWithFamilyHasCapacity(t *testing.T) {
	now := time.Now()
	a := claudeAccountPrio("a", 0)
	p := New([]*Account{a}, now)
	p.MarkWindowRejected(a, "7d-fable", now.Add(time.Hour))
	if p.FamilyHasCapacity("7d-fable") || !p.FamilyHasCapacity("") {
		t.Fatal("precondition: fable out, general in")
	}
	if p.BucketOf(a) != BucketAvailable {
		t.Fatal("general capacity must make the account Available")
	}
}
