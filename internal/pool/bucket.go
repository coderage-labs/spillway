package pool

import "time"

// Bucket is the dashboard's three-way classification of an account (issue
// #254). Every account is in exactly one.
type Bucket string

const (
	// BucketAvailable: the general windows (5h/7d) are above the spill point
	// and not refused -- what the tank's own label calls standby/serving.
	BucketAvailable Bucket = "available"
	// BucketSpent: healthy, but its general windows are at/below the spill
	// point or refused -- exactly when the tank's label says "spent" or "dry".
	BucketSpent Bucket = "spent"
	// BucketAttention: needs the user, not time -- operator-disabled, or the
	// credential died (rejected refresh token, auth failure).
	BucketAttention Bucket = "attention"
)

// BucketOf classifies a. Attention wins over everything else. Otherwise the
// filter must agree with the tank's label, which says "spent" from the
// spill point on (overThreshold), not only once the provider refuses: an
// account is Available iff the general family still has capacity
// (accountHasFamilyCapacityLocked, routing's own predicate) AND its general
// windows are above the spill point (OverThresholdFor, the same call
// /api/accounts' overThreshold makes). Fable is deliberately ignored: a
// fable-only spent account is still Available, as its label says.
func (p *Pool) BucketOf(a *Account) Bucket {
	if a.Parked() || a.State() == StateDisabled {
		return BucketAttention
	}
	p.mu.Lock()
	ok := p.accountHasFamilyCapacityLocked(a, "", time.Now())
	thr := p.threshold()
	p.mu.Unlock()
	if ok && !a.OverThresholdFor("", thr) {
		return BucketAvailable
	}
	return BucketSpent
}
