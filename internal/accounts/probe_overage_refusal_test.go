package accounts

// Issue #151 seen from the probe side. Two things need pinning down, because
// #152 and #151 both changed what CanOverage answers and the probe's money
// guard is built out of CanOverage.
//
//  1. While a refusal is FRESH, CanOverage is false, so the probe is free and
//     runs on the ordinary cadence. That is #152's fix and #151 must not cost
//     it: it is the path that lifts a refusal on an idle pool.
//  2. Once a refusal EXPIRES on an opted-in account, CanOverage is true and
//     the probe falls back under #152's billed rationing. That is deliberate,
//     not a regression: an expired refusal means spillway is now willing to
//     spend there, and a probe it might be charged for is exactly what
//     billedProbeAge exists to ration. The reading still moves — ordinary
//     traffic reaches the account the moment there is any, because selection
//     no longer suppresses it, which is the whole point of #151.

import (
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/provider"
)

// A fresh refusal keeps the probe free, so the probe keeps running.
func TestFreshOverageRefusalKeepsTheProbeFree(t *testing.T) {
	a := spentAccount(t, "refused-fresh", "")
	a.SetAllowOverage(boolPtr(true)) // opted in, and it still cannot bill
	a.SetOverageForTest(provider.Overage{
		Known: true, Available: false, Reason: "member_zero_credit_limit",
	})

	if wouldBill(a, true, time.Now()) {
		t.Fatal("a probe on an account the provider is refusing was treated as " +
			"billable — the provider answers it with a free 429 (issue #152)")
	}
	if !needsProbe(a, true, 30*time.Minute) {
		t.Error("the free probe that can lift a refusal was skipped — this is the " +
			"path that unsticks an idle pool")
	}
}

// An expired refusal on an opted-in account means spillway is willing to
// spend there, so the probe is rationed rather than free. Documented, not
// incidental.
func TestExpiredOverageRefusalPutsTheProbeBackUnderRationing(t *testing.T) {
	a := spentAccount(t, "refused-stale", "")
	a.SetAllowOverage(boolPtr(true))
	a.SetOverageAtForTest(provider.Overage{
		Known: true, Available: false, Reason: "member_zero_credit_limit",
	}, time.Now().Add(-25*time.Hour))

	if !wouldBill(a, true, time.Now()) {
		t.Fatal("an expired refusal plus an explicit opt-in should be treated as " +
			"potentially billable: spillway has decided it may spend there")
	}
	// The quota reading is 6h old (spentAccount), inside billedProbeAge's
	// 24h ceiling, so no probe is bought for it.
	if needsProbe(a, true, 30*time.Minute) {
		t.Error("a probe that might be charged ran outside the age cap (issue #152)")
	}
}

// Without an opt-in there is nothing to expire into, so the probe stays free
// whatever the refusal's age — the never-spend-uninvited rule, from the
// probe's side.
func TestExpiredRefusalWithoutAnOptInKeepsTheProbeFree(t *testing.T) {
	for _, poolAllows := range []bool{false, true} {
		a := spentAccount(t, "no-opt-in", "")
		a.SetOverageAtForTest(provider.Overage{
			Known: true, Available: false, Reason: "member_zero_credit_limit",
		}, time.Now().Add(-25*time.Hour))

		if wouldBill(a, poolAllows, time.Now()) {
			t.Errorf("poolAllows=%v: an account nobody singled out was treated as "+
				"billable after its refusal expired", poolAllows)
		}
		if !needsProbe(a, poolAllows, 30*time.Minute) {
			t.Errorf("poolAllows=%v: the free probe was skipped", poolAllows)
		}
	}
}
