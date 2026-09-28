package accounts

// Issue #229's second fix, twice refined.
//
// First cut: moving a window rejection's re-test off live traffic and onto
// the prober only works if the prober actually runs. A busy account —
// still serving Sonnet/Opus fine — keeps its 5h/7d windows fresh through
// ordinary traffic alone, which is exactly what needsProbe's plain
// staleness check (now.Sub(newest) > staleAfter) looks at. Without a check
// that looks at the FAMILY window's own state, such an account's fable
// rejection could go un-probed indefinitely: the account never becomes
// selectable for fable again (issue #229's first fix makes the exclusion
// last the real reset), and nothing ever re-measures it either —
// permanently excluded, the exact failure #194 was written to prevent,
// wearing a new hat.
//
// Second cut, from a production-live review of the first: that forcing
// check was keyed on "a windowRejected entry exists", which misses a
// family that has gone stale (its recorded reset passed, nothing
// re-measured since) WITHOUT ever having been formally 429-rejected —
// e.g. nobody has sent a fable request in a week, so no rejection was ever
// recorded, but the last reading on file is long expired. The trigger is
// now Account.HasSpentOrExpiredFamilyAt, applied in ProbeIdle's own loop
// (see its call site) rather than inside needsProbeAt: SPENT (an
// outstanding rejection, or over the switch threshold) OR EXPIRED (reset
// passed, unmeasured since). A HEALTHY family window must never trigger
// the family probe at all — that would spend real fable quota to
// re-confirm a fact already on file.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/pool"
)

// busyAccount is healthy and freshly measured on both governing windows —
// what a Sonnet/Opus workhorse looks like from needsProbe's point of view.
func busyAccount(name string) *pool.Account {
	a := claudeAccount(name)
	now := time.Now()
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.3, Source: "headers", ResetAt: now.Add(2 * time.Hour), FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 0.4, Source: "headers", ResetAt: now.Add(48 * time.Hour), FetchedAt: now},
	})
	return a
}

// newestFetchedAt mirrors needsProbeAt's own scan, for precondition checks.
func newestFetchedAt(a *pool.Account) time.Time {
	var newest time.Time
	for _, w := range a.QuotaWindows() {
		if w.FetchedAt.After(newest) {
			newest = w.FetchedAt
		}
	}
	return newest
}

// probeHits records which model every request to a fake upstream named,
// safe for concurrent use (ProbeIdle walks accounts sequentially today,
// but nothing here should depend on that).
type probeHits struct {
	mu     sync.Mutex
	models []string
}

func (h *probeHits) add(model string) {
	h.mu.Lock()
	h.models = append(h.models, model)
	h.mu.Unlock()
}

func (h *probeHits) contains(model string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.models {
		if m == model {
			return true
		}
	}
	return false
}

func (h *probeHits) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.models)
}

// requestModel extracts the "model" field spillway's probeBody always
// sends, without consuming r.Body for anyone else.
func requestModel(t *testing.T, r *http.Request) string {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading probe request body: %v", err)
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshalling probe request body %q: %v", body, err)
	}
	return payload.Model
}

// fableFamilyModel resolves the model familyProbeModel picks for "7d-fable"
// — the same call probeRejectedFamilies itself makes — so these tests
// assert against the real value rather than a second, hand-typed copy of
// provider's own (unexported) model id that could silently drift from it.
func fableFamilyModel(t *testing.T, p *pool.Pool, a *pool.Account) string {
	t.Helper()
	model, ok := familyProbeModel(p, a, "7d-fable")
	if !ok {
		t.Fatal("familyProbeModel(\"7d-fable\") = ok=false — precondition broken for every test in this file")
	}
	return model
}

// perModelUpstream answers each request according to its own model: the
// ordinary probe model and the fable family model are given independent
// response functions, so a test can assert exactly which one(s) were
// actually asked for.
func perModelUpstream(t *testing.T, hits *probeHits, respond func(model string) http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := requestModel(t, r)
		hits.add(model)
		respond(model)(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// healthyWindowsHandler answers with a plain, healthy 5h/7d reading —
// enough for probeOne to succeed and move on, uninteresting to every test
// here.
func healthyWindowsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.1")
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", fmt.Sprint(time.Now().Add(2*time.Hour).Unix()))
	w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.2")
	w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", fmt.Sprint(time.Now().Add(48*time.Hour).Unix()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"msg_ok"}`))
}

// The headline. Everything about this account looks freshly probed except
// the one thing that matters: a live fable rejection old enough to have
// crossed FamilyProbeDue's half-effective-TTL mark (issue #229's
// coordinator follow-up on timing — see TestFamilyProbeNeverForcedBeforeHalfEffectiveTTL
// for the fresh-rejection contrast). ProbeIdle — the real scheduled path,
// not a unit check on needsProbeAt alone — must still visit the account.
func TestProbeIdleVisitsBusyAccountForStaleWindowRejection(t *testing.T) {
	staleAfter := 30 * time.Minute
	now := time.Now()

	var hits probeHits
	srv := perModelUpstream(t, &hits, func(model string) http.HandlerFunc { return healthyWindowsHandler })

	a := busyAccount("work")
	a.Upstream = srv.URL
	p := pool.New([]*pool.Account{a}, now)
	// probeEvery is left at its zero value (no Apply call), so the
	// effective TTL is the raw windowRejectionTTL (30m, unexported) and
	// FamilyProbeDue's half-mark is 15m; 20 minutes ago is past that.
	rejectedAt := now.Add(-20 * time.Minute)
	p.MarkWindowRejectedAtForTest(a, "7d-fable", now.Add(9*24*time.Hour), rejectedAt)

	// Precondition: the plain staleness check alone would NOT probe this
	// account — its general windows are as fresh as `now` itself.
	if now.Sub(newestFetchedAt(a)) > staleAfter {
		t.Fatal("precondition broken: the account's general windows are not actually fresh")
	}
	if needsProbe(a, p.AllowOverage(), staleAfter) {
		t.Fatal("precondition broken: needsProbe alone already says yes, so this doesn't test the OR")
	}
	if !p.FamilyProbeDue(a, now) {
		t.Fatal("precondition broken: the rejection must already be past FamilyProbeDue's half-mark, " +
			"or forcing a visit proves nothing")
	}

	ProbeIdle(t.Context(), p, srv.Client(), srv.URL, staleAfter, quietLogger())

	if hits.count() == 0 {
		t.Error("ProbeIdle never visited a busy account carrying a live window rejection past the " +
			"half-effective-TTL mark — nothing will ever re-measure this family: it stays excluded " +
			"from selection AND invisible to the prober (issue #229)")
	}
}

// Contrast: a rejection recorded only moments ago must NOT force a visit
// yet — FamilyProbeDue's half-mark exists precisely so a fresh rejection
// isn't probed on literally the next tick regardless of phase, only once
// there is a real reason to hurry. A busy account with nothing else due
// stays unvisited.
func TestFamilyProbeNotYetDueForFreshRejection(t *testing.T) {
	staleAfter := 30 * time.Minute
	now := time.Now()

	var hits probeHits
	srv := perModelUpstream(t, &hits, func(model string) http.HandlerFunc { return healthyWindowsHandler })

	a := busyAccount("work")
	a.Upstream = srv.URL
	p := pool.New([]*pool.Account{a}, now)
	p.MarkWindowRejected(a, "7d-fable", now.Add(9*24*time.Hour)) // recorded just now

	if p.FamilyProbeDue(a, now) {
		t.Fatal("precondition broken: a rejection recorded just now must not already be due")
	}
	if needsProbe(a, p.AllowOverage(), staleAfter) {
		t.Fatal("precondition broken: needsProbe alone must not already say yes")
	}

	ProbeIdle(t.Context(), p, srv.Client(), srv.URL, staleAfter, quietLogger())

	if hits.count() != 0 {
		t.Error("ProbeIdle visited a busy account over a rejection recorded moments ago — " +
			"FamilyProbeDue's half-mark was not honoured")
	}
}

// Required plant 1 (coordinator follow-up): a HEALTHY family window must
// never trigger the family probe. The account here IS visited (its
// general windows are stale, so the ordinary probe runs regardless) —
// isolating the one question this test is about: does a healthy fable
// reading additionally draw a SECOND, family-scoped request. It must not.
func TestProbeIdleNeverProbesAHealthyFamilyWindow(t *testing.T) {
	now := time.Now()
	a := claudeAccount("work")
	// Stale general windows (forces the ordinary probe) AND a healthy,
	// fresh, well-under-threshold fable reading — nothing due about it.
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.1, Source: "headers", ResetAt: now.Add(2 * time.Hour), FetchedAt: now.Add(-time.Hour)},
		{Name: "7d", Limit: 1, Used: 0.2, Source: "headers", ResetAt: now.Add(48 * time.Hour), FetchedAt: now.Add(-time.Hour)},
		{Name: "7d-fable", Limit: 1, Used: 0.1, Source: "headers", ResetAt: now.Add(48 * time.Hour), FetchedAt: now.Add(-time.Hour)},
	})
	ordinaryModel := probeModel(a)
	p := pool.New([]*pool.Account{a}, now)
	fableModel := fableFamilyModel(t, p, a)

	var hits probeHits
	srv := perModelUpstream(t, &hits, func(model string) http.HandlerFunc {
		if model == fableModel {
			return func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("the fable family model %q was probed for a HEALTHY fable window", model)
				healthyWindowsHandler(w, r)
			}
		}
		return healthyWindowsHandler
	})
	a.Upstream = srv.URL

	if a.HasSpentOrExpiredFamilyAt(p.Threshold(), now) {
		t.Fatal("precondition broken: the fable window must read healthy, or this doesn't test the skip")
	}

	ProbeIdle(t.Context(), p, srv.Client(), srv.URL, 30*time.Minute, quietLogger())

	if hits.contains(fableModel) {
		t.Error("the family probe fired for a healthy fable window — see the handler's own failure above")
	}
	if !hits.contains(ordinaryModel) {
		t.Fatal("the ordinary probe never ran at all — precondition broken, not what this test is about")
	}
}

// Required plant 2 (coordinator follow-up): an EXPIRED family window with
// NO outstanding rejection — nobody has sent a fable request in a while,
// so upstream never 429'd it, but the last reading's reset has passed with
// nothing re-measuring it since. The scheduled path (ProbeIdle) must still
// run the family probe for it; keying the trigger on "a windowRejected
// entry exists" alone (the first version of this fix) would miss this
// case entirely.
func TestProbeIdleRunsFamilyProbeForExpiredUnrejectedWindow(t *testing.T) {
	now := time.Now()
	a := claudeAccount("work")
	a.SetQuotaWindows([]pool.QuotaWindow{
		{Name: "5h", Limit: 1, Used: 0.1, Source: "headers", ResetAt: now.Add(2 * time.Hour), FetchedAt: now},
		{Name: "7d", Limit: 1, Used: 0.2, Source: "headers", ResetAt: now.Add(48 * time.Hour), FetchedAt: now},
		// Well under threshold — never rejected, never spent by
		// OverThresholdForWindow's own measure — but its reset passed two
		// hours ago and nothing has re-measured it since: EXPIRED.
		{Name: "7d-fable", Limit: 1, Used: 0.3, Source: "headers", ResetAt: now.Add(-2 * time.Hour), FetchedAt: now.Add(-3 * time.Hour)},
	})
	p := pool.New([]*pool.Account{a}, now)
	fableModel := fableFamilyModel(t, p, a)

	var hits probeHits
	srv := perModelUpstream(t, &hits, func(model string) http.HandlerFunc { return healthyWindowsHandler })
	a.Upstream = srv.URL

	if _, ok := a.WindowRejectedUntil("7d-fable"); ok {
		t.Fatal("precondition broken: there must be no outstanding rejection for this test to isolate 'expired'")
	}
	if !a.HasSpentOrExpiredFamilyAt(p.Threshold(), now) {
		t.Fatal("precondition broken: the fable window must read EXPIRED, or forcing a visit proves nothing")
	}

	ProbeIdle(t.Context(), p, srv.Client(), srv.URL, 30*time.Minute, quietLogger())

	if !hits.contains(fableModel) {
		t.Error("the scheduled path never ran the family probe for an expired-but-unrejected window — " +
			"keying the trigger on 'a rejection exists' misses exactly this case")
	}
}
