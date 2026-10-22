package pool

import "time"

// Bucket is the dashboard's three-way classification of an account (issue
// #254). Every account is in exactly one.
type Bucket string

const (
	// BucketAvailable: routing would pick the account for at least one model
	// family right now.
	BucketAvailable Bucket = "available"
	// BucketSpent: healthy, but cannot serve any family until a refill.
	BucketSpent Bucket = "spent"
	// BucketAttention: needs the user, not time -- operator-disabled, or the
	// credential died (rejected refresh token, auth failure).
	BucketAttention Bucket = "attention"
)

// bucketFamilies are the families the dashboard asks about: the general
// windows and the fable window. A fable-only rejection leaves "" open, so
// the account stays Available (its fable gauge already shows fable is out).
var bucketFamilies = []string{"", "7d-fable"}

// BucketOf classifies a. Attention wins over everything else; otherwise
// Available iff accountHasFamilyCapacityLocked -- the same per-account
// predicate FamilyHasCapacity (and so selection's tiers 1/2) uses -- holds
// for at least one family. Never re-derived from utilisation percentages.
func (p *Pool) BucketOf(a *Account) Bucket {
	if a.Parked() || a.State() == StateDisabled {
		return BucketAttention
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, f := range bucketFamilies {
		if p.accountHasFamilyCapacityLocked(a, f, now) {
			return BucketAvailable
		}
	}
	return BucketSpent
}
