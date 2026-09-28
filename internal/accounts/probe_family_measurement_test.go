package accounts

// Issue #229's coordinator follow-up, third round: a live-daemon review
// found the family probe model (provider.Spec.FamilyProbeModel's constant)
// was INVENTED, and that a wrong-but-present id is worse than no id at
// all — it still returns ok=true, so the probe is sent, 400/404s on the
// unrecognised model, measures nothing, and (before this round) the
// account stayed excluded regardless, because the exclusion fallback only
// ever asked "would this bill", never "did anything actually measure it".
//
// Two fixes, tested here:
//
//  1. familyProbeModel now prefers a model THIS ACCOUNT (or another in the
//     pool) has actually been served on and is confirmed fable-governed,
//     over the fallback constant — TestFamilyProbeModelPrefersAPreviouslyServedModel
//     and its neighbours.
//  2. Any outcome that isn't a successful measurement — 400, 404, a
//     network error, or a 200 with no header for the window — must not
//     extend the exclusion. pool.windowRejectionExcludes' fallback (pinned
//     directly in internal/pool/window_billing_fallback_test.go) now judges
//     this purely by whether ANYTHING has refreshed the window's row within
//     windowRejectionTTL, which these end-to-end tests exercise through the
//     real probe pipeline: model resolution, the HTTP round trip, and
//     RecordQuota's write (or non-write).

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
)

// fableBody mirrors internal/pool's own test fixture of the same name
// (accounts must not import pool's test-only identifiers, so this is a
// small, deliberate duplicate).
func fableBody(model string) []byte {
	return []byte(`{"model":"` + model + `","max_tokens":16,"messages":[]}`)
}

// servedFableModel is a model containing "fable" that no production
// account has actually served — distinct from provider's own fallback
// constant, so a test asserting "the OBSERVED model, not the constant"
// cannot pass by coincidence.
const servedFableModel = "claude-fable-9-observed-test"

// Required test (a): a previously served fable model wins over the
// fallback constant.
func TestFamilyProbeModelPrefersAPreviouslyServedModel(t *testing.T) {
	a := claudeAccount("work")
	a.SetLastModel(servedFableModel)
	p := pool.New([]*pool.Account{a}, time.Now())

	model, ok := familyProbeModel(p, a, "7d-fable")
	if !ok {
		t.Fatal("familyProbeModel(\"7d-fable\") = ok=false")
	}
	if model != servedFableModel {
		t.Errorf("familyProbeModel = %q, want the observed model %q — the fallback constant must "+
			"never be preferred over a model this account has actually been served on", model, servedFableModel)
	}
}

// Failing that, another account in the SAME pool that has served the
// family wins too — still a real, confirmed id, just not this account's
// own.
func TestFamilyProbeModelFallsBackToAnotherAccountInThePool(t *testing.T) {
	a := claudeAccount("work") // never served fable itself
	other := claudeAccount("other")
	other.SetLastModel(servedFableModel)
	p := pool.New([]*pool.Account{a, other}, time.Now())

	model, ok := familyProbeModel(p, a, "7d-fable")
	if !ok {
		t.Fatal("familyProbeModel(\"7d-fable\") = ok=false")
	}
	if model != servedFableModel {
		t.Errorf("familyProbeModel = %q, want %q from the other account in the pool", model, servedFableModel)
	}
}

// A different provider's account serving a "fable"-named model must not
// leak across: an id valid for one provider says nothing about another's
// vocabulary.
func TestFamilyProbeModelIgnoresAnotherProvidersServedModel(t *testing.T) {
	a := claudeAccount("work")
	kimi := pool.NewAccount("kimi", pool.SourceYAML, "tok", "", 0, "")
	kimi.Type = "kimi-oauth"
	kimi.SetLastModel(servedFableModel)
	p := pool.New([]*pool.Account{a, kimi}, time.Now())

	fallback, ok := provider.For(a.Type).FamilyProbeModel("7d-fable", nil)
	if !ok {
		t.Fatal("precondition: provider must have a fallback constant for 7d-fable")
	}

	model, ok := familyProbeModel(p, a, "7d-fable")
	if !ok {
		t.Fatal("familyProbeModel(\"7d-fable\") = ok=false")
	}
	if model != fallback {
		t.Errorf("familyProbeModel = %q, want the provider's own fallback %q — a Kimi account's served "+
			"model must never be borrowed for a Claude account's probe", model, fallback)
	}
}

// Nothing in the pool has ever served the family: the provider's own
// fallback constant is used, exactly as documented.
func TestFamilyProbeModelFallsBackToConstantWhenNothingEverServedIt(t *testing.T) {
	a := claudeAccount("work")
	p := pool.New([]*pool.Account{a}, time.Now())

	fallback, ok := provider.For(a.Type).FamilyProbeModel("7d-fable", nil)
	if !ok {
		t.Fatal("precondition: provider must have a fallback constant for 7d-fable")
	}

	model, ok := familyProbeModel(p, a, "7d-fable")
	if !ok {
		t.Fatal("familyProbeModel(\"7d-fable\") = ok=false")
	}
	if model != fallback {
		t.Errorf("familyProbeModel = %q, want the provider's fallback constant %q", model, fallback)
	}
}

// unknownModelUpstream answers EVERY probe — ordinary and family alike —
// with a 400 "unknown model", the shape a wrong-but-present model id
// produces in production: never a charge, never network failure, just no
// usable reading.
func unknownModelUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"model: unknown model"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Required test (b): a family probe answered with 400 "unknown model"
// does not strand the account. Plant: remove the could-not-measure
// fallback (pool.windowRejectionExcludes' age check) and this fails.
func TestFamilyProbe400UnknownModelDoesNotStrandAccount(t *testing.T) {
	now := time.Now()
	a := claudeAccount("work")
	p := pool.New([]*pool.Account{a}, now)
	// Rejected well past windowRejectionTTL ago, with nothing having
	// measured it since — the shape a permanently-400ing family probe
	// would leave behind.
	rejectedAt := now.Add(-45 * time.Minute)
	p.MarkWindowRejectedAtForTest(a, "7d-fable", now.Add(9*24*time.Hour), rejectedAt)

	srv := unknownModelUpstream(t)
	a.Upstream = srv.URL

	ProbeIdle(t.Context(), p, srv.Client(), srv.URL, 30*time.Minute, quietLogger())

	if got := p.SelectFor("s", fableBody(servedFableModel)); got == nil {
		t.Fatal("SelectFor(fable) = nil — a family probe that 400's on every attempt must not strand " +
			"the account: nothing has EVER successfully measured this window")
	}
}

// Required test (c), end to end: a family probe that DOES measure the
// window, and finds it still spent, keeps the account excluded — this is
// the one outcome where exclusion is actually justified, and it must
// survive going through the real probe pipeline (model resolution, HTTP,
// RecordQuota), not just the pool-level unit form in
// internal/pool/window_billing_fallback_test.go.
func TestFamilyProbeMeasuringStillSpentKeepsAccountExcluded(t *testing.T) {
	now := time.Now()
	a := claudeAccount("work")
	p := pool.New([]*pool.Account{a}, now)
	rejectedAt := now.Add(-45 * time.Minute)
	p.MarkWindowRejectedAtForTest(a, "7d-fable", now.Add(9*24*time.Hour), rejectedAt)

	fableModel := fableFamilyModel(t, p, a)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.1")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", fmt.Sprint(now.Add(2*time.Hour).Unix()))
		w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Utilization", "1.0") // still fully spent
		w.Header().Set("Anthropic-Ratelimit-Unified-7d_oi-Reset", fmt.Sprint(now.Add(9*24*time.Hour).Unix()))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_ok"}`))
	}))
	t.Cleanup(srv.Close)
	a.Upstream = srv.URL

	ProbeIdle(t.Context(), p, srv.Client(), srv.URL, 30*time.Minute, quietLogger())

	if got := p.SelectFor("s", fableBody(fableModel)); got != nil {
		t.Fatalf("SelectFor(fable) = %q, want nil — a probe that just re-confirmed the window is still "+
			"spent must keep the account excluded", got.Name)
	}
}
