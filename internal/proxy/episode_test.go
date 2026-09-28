package proxy

// Tests for issue #230: one exhaustion/recovery notification per episode,
// scoped to the model family that is actually affected, with recovery
// driven by pool state rather than a request being served.
//
// Most of these drive Handler.reportExhaustion and Handler.checkRecovery
// directly rather than going through a real request/park cycle: the
// episode state machine (episode.go) is the thing #230 is actually about,
// and testing it directly is both faster (no waiting out real holds) and
// more precise (every case below is a pool-state transition, and the pool
// state is exactly what these two functions re-derive fresh on every
// call — see reportExhaustion's own comment on why it never trusts the
// caller's belief about capacity). TestHoldWiresIntoEpisodeNotification at
// the bottom is the one end-to-end check that hold.go's real call site
// plumbs the family and ETA through correctly.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/notify"
	"github.com/coderage-labs/spillway/internal/pool"
)

const fableFamily = "7d-fable"

// sentRecorder captures every local notification Notify/NotifyEpisode/
// NotifyLocal actually dispatched, in the order delivery reached it.
// Delivery runs on its own goroutine (both Notify's and NotifyEpisode's),
// so callers must poll (waitForSent) rather than assume it landed the
// instant the triggering call returns.
type sentRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *sentRecorder) add(title, body string) {
	r.mu.Lock()
	r.msgs = append(r.msgs, title+"|"+body)
	r.mu.Unlock()
}

func (r *sentRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.msgs...)
}

func (r *sentRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.msgs)
}

func waitForSent(t *testing.T, r *sentRecorder, want int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if r.count() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d notification(s), got %d: %v", want, r.count(), r.all())
}

// assertNoMoreSent gives any stray extra delivery a moment to land, then
// fails if one did — the negative half of waitForSent, for "and nothing
// further" assertions.
func assertNoMoreSent(t *testing.T, r *sentRecorder, want int) {
	t.Helper()
	time.Sleep(60 * time.Millisecond)
	if got := r.count(); got != want {
		t.Fatalf("got %d notification(s), want exactly %d: %v", got, want, r.all())
	}
}

// episodeRig builds a notify-mode Handler over accts, with its notifier's
// local send hook wired to a recorder instead of a real platform notifier —
// no real notification is ever sent by these tests.
func episodeRig(t *testing.T, holdMax string, accts ...*pool.Account) (*Handler, *pool.Pool, *sentRecorder) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Pool.ExhaustedMode = "notify"
	cfg.Pool.HoldMax = holdMax
	p := pool.New(accts, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	rec := &sentRecorder{}
	n := notify.New()
	n.SetSendFuncForTest(func(_ context.Context, title, body string) error {
		rec.add(title, body)
		return nil
	})
	h.SetNotifier(n)
	return h, p, rec
}

func claudeAcct(name string) *pool.Account {
	a := pool.NewAccount(name, pool.SourceYAML, "tok", "", 0, "")
	a.Type = "claude-oauth"
	return a
}

func freshFableWindow() pool.QuotaWindow {
	return pool.QuotaWindow{Name: fableFamily, Limit: 1, Used: 0, Source: "headers",
		ResetAt: time.Now().Add(48 * time.Hour), FetchedAt: time.Now()}
}

// Holding N requests, retrying them, and waking/re-holding repeatedly
// inside one episode must send exactly one start notification (issue
// #230's headline test).
func TestEpisodeOneStartNotificationForManyHolds(t *testing.T) {
	a := claudeAcct("a")
	h, p, rec := episodeRig(t, "4h", a)
	eta := time.Now().Add(90 * time.Minute)
	p.MarkWindowRejected(a, fableFamily, eta)

	for i := 0; i < 5; i++ {
		h.reportExhaustion(fableFamily, eta, true)
	}
	waitForSent(t, rec, 1)
	assertNoMoreSent(t, rec, 1)
	if !strings.Contains(rec.all()[0], "Fable held") {
		t.Errorf("message = %q, want a fable-held start", rec.all()[0])
	}
}

// The live bug (issue #230): a fable-only exhaustion must name fable and
// say other models are running — never claim the whole pool is exhausted —
// while the general windows genuinely still have capacity for a concurrent
// Sonnet-shaped request.
func TestEpisodeFamilyOnlyNeverClaimsAllModels(t *testing.T) {
	a := claudeAcct("a")
	h, p, rec := episodeRig(t, "4h", a)
	eta := time.Now().Add(20 * time.Minute)
	p.MarkWindowRejected(a, fableFamily, eta)

	h.reportExhaustion(fableFamily, eta, true)
	waitForSent(t, rec, 1)
	msg := rec.all()[0]
	if strings.Contains(msg, "All models") || strings.Contains(msg, "all accounts") {
		t.Errorf("fable-only message wrongly claims pool-wide exhaustion: %q", msg)
	}
	if !strings.Contains(msg, "other models running") {
		t.Errorf("message = %q, want it to say other models are still running", msg)
	}
	if !p.FamilyHasCapacity("") {
		t.Error("general capacity should still be available while only fable is rejected")
	}
}

// ETA slip rules: more than the threshold later re-notifies once; less
// than the threshold, or moving earlier, sends nothing.
func TestEpisodeReNotifiesOnlyOnMaterialSlip(t *testing.T) {
	a := claudeAcct("a")
	h, p, rec := episodeRig(t, "8h", a)
	base := time.Now().Add(30 * time.Minute)
	p.MarkWindowRejected(a, fableFamily, base)
	h.reportExhaustion(fableFamily, base, true)
	waitForSent(t, rec, 1)

	// Moves earlier: nothing (good news; the recovery message covers it).
	earlier := base.Add(-10 * time.Minute)
	p.MarkWindowRejected(a, fableFamily, earlier)
	h.reportExhaustion(fableFamily, earlier, true)
	assertNoMoreSent(t, rec, 1)

	// Slips later, but under the 15-minute threshold: nothing.
	smallSlip := base.Add(10 * time.Minute)
	p.MarkWindowRejected(a, fableFamily, smallSlip)
	h.reportExhaustion(fableFamily, smallSlip, true)
	assertNoMoreSent(t, rec, 1)

	// Slips later, past the threshold (measured from what was last
	// notified — base, not smallSlip): exactly one more.
	bigSlip := base.Add(20 * time.Minute)
	p.MarkWindowRejected(a, fableFamily, bigSlip)
	h.reportExhaustion(fableFamily, bigSlip, true)
	waitForSent(t, rec, 2)
	assertNoMoreSent(t, rec, 2)

	// Moves substantially earlier (more than the threshold, from what was
	// last notified — bigSlip): still nothing. This is the direction check
	// TestEpisodeReNotifiesOnlyOnMaterialSlip's small -10m case above can't
	// pin on its own, since a magnitude-only comparison would also stay
	// silent for a slip that small.
	muchEarlier := bigSlip.Add(-40 * time.Minute)
	p.MarkWindowRejected(a, fableFamily, muchEarlier)
	h.reportExhaustion(fableFamily, muchEarlier, true)
	assertNoMoreSent(t, rec, 2)
}

// Recovery sends exactly one "back", and a subsequent, brand-new
// exhaustion starts a fresh episode rather than staying suppressed.
func TestEpisodeRecoveryThenFreshEpisode(t *testing.T) {
	a := claudeAcct("a")
	h, p, rec := episodeRig(t, "4h", a)
	eta := time.Now().Add(20 * time.Minute)
	p.MarkWindowRejected(a, fableFamily, eta)
	h.reportExhaustion(fableFamily, eta, true)
	waitForSent(t, rec, 1)

	a.SetQuotaWindows([]pool.QuotaWindow{freshFableWindow()})
	p.ClearRecoveredWindowRejections(a)
	h.checkRecovery()
	waitForSent(t, rec, 2)
	assertNoMoreSent(t, rec, 2)
	if !strings.Contains(rec.all()[1], "Fable back") {
		t.Errorf("recovery message = %q, want %q", rec.all()[1], "Fable back")
	}

	newEta := time.Now().Add(5 * time.Minute)
	p.MarkWindowRejected(a, fableFamily, newEta)
	h.reportExhaustion(fableFamily, newEta, true)
	waitForSent(t, rec, 3)
	assertNoMoreSent(t, rec, 3)
	if !strings.Contains(rec.all()[2], "Fable held") {
		t.Errorf("fresh episode message = %q, want a new fable-held start", rec.all()[2])
	}
}

// Recovery must fire on capacity returning, even with zero held requests
// outstanding — the owner stopped retrying, so nothing on the request path
// runs at all; only checkRecovery (the background watcher's call) can
// possibly announce this.
func TestEpisodeRecoveryFiresWithoutAnyHeldRequest(t *testing.T) {
	a := claudeAcct("a")
	h, p, rec := episodeRig(t, "4h", a)
	eta := time.Now().Add(20 * time.Minute)
	p.MarkWindowRejected(a, fableFamily, eta)
	h.reportExhaustion(fableFamily, eta, true) // the one and only request that ever asked
	waitForSent(t, rec, 1)

	// Capacity returns on its own (a probe measuring it, in production).
	// No further reportExhaustion call follows — nobody is retrying.
	a.SetQuotaWindows([]pool.QuotaWindow{freshFableWindow()})
	p.ClearRecoveredWindowRejections(a)

	h.checkRecovery()
	waitForSent(t, rec, 2)
	if !strings.Contains(rec.all()[1], "Fable back") {
		t.Errorf("recovery message = %q, want %q", rec.all()[1], "Fable back")
	}
}

// A fable episode open, then every family running out, escalates once to
// "All models held" — not a second fable message.
func TestEpisodeEscalatesToAllModelsWithoutASecondFamilyMessage(t *testing.T) {
	a := claudeAcct("a")
	b := claudeAcct("b")
	h, p, rec := episodeRig(t, "4h", a, b)

	fableEta := time.Now().Add(10 * time.Minute)
	p.MarkWindowRejected(a, fableFamily, fableEta)
	p.MarkWindowRejected(b, fableFamily, fableEta)
	h.reportExhaustion(fableFamily, fableEta, true)
	waitForSent(t, rec, 1)
	if !strings.Contains(rec.all()[0], "Fable held") {
		t.Fatalf("setup: expected a fable-held start, got %q", rec.all()[0])
	}

	allEta := time.Now().Add(3 * time.Hour)
	p.MarkExhausted(a, allEta)
	p.MarkExhausted(b, allEta)
	h.reportExhaustion(fableFamily, allEta, true)

	waitForSent(t, rec, 2)
	assertNoMoreSent(t, rec, 2)
	if !strings.Contains(rec.all()[1], "All models held") {
		t.Errorf("escalation message = %q, want %q", rec.all()[1], "All models held")
	}
}

// Partial recovery out of an all-models episode: the general windows
// coming back while fable is still spent names fable and its own ETA, then
// a later, separate fable recovery sends exactly one "Fable back" — never
// a second combined message.
func TestEpisodePartialRecoveryThenFableRecovers(t *testing.T) {
	a := claudeAcct("a")
	h, p, rec := episodeRig(t, "4h", a)

	generalEta := time.Now().Add(2 * time.Hour)
	fableEta := generalEta.Add(30 * time.Minute) // fable's own reset is further out
	p.MarkWindowRejected(a, fableFamily, fableEta)
	p.MarkExhausted(a, generalEta)
	h.reportExhaustion(fableFamily, generalEta, true)
	waitForSent(t, rec, 1)
	if !strings.Contains(rec.all()[0], "All models held") {
		t.Fatalf("setup: want an all-models start, got %q", rec.all()[0])
	}

	// General windows recover; fable's own window is still rejected.
	p.ClearExhausted(a)
	h.checkRecovery()
	waitForSent(t, rec, 2)
	partial := rec.all()[1]
	if !strings.Contains(partial, "Other models back") || !strings.Contains(partial, "fable") {
		t.Errorf("partial recovery message = %q, want it to say other models are back and name fable", partial)
	}

	// Nothing further until fable itself actually recovers.
	h.checkRecovery()
	assertNoMoreSent(t, rec, 2)

	a.SetQuotaWindows([]pool.QuotaWindow{freshFableWindow()})
	p.ClearRecoveredWindowRejections(a)
	h.checkRecovery()
	waitForSent(t, rec, 3)
	assertNoMoreSent(t, rec, 3)
	if !strings.Contains(rec.all()[2], "Fable back") {
		t.Errorf("final recovery message = %q, want %q", rec.all()[2], "Fable back")
	}
}

// Both the general windows and fable recovering together send ONE "All
// models back" message, not "other models back" followed by "fable back".
func TestEpisodeBothRecoverTogetherSendsOneAllModelsBackMessage(t *testing.T) {
	a := claudeAcct("a")
	h, p, rec := episodeRig(t, "4h", a)

	allEta := time.Now().Add(2 * time.Hour)
	p.MarkWindowRejected(a, fableFamily, allEta)
	p.MarkExhausted(a, allEta)
	h.reportExhaustion(fableFamily, allEta, true)
	waitForSent(t, rec, 1)

	p.ClearExhausted(a)
	a.SetQuotaWindows([]pool.QuotaWindow{freshFableWindow()})
	p.ClearRecoveredWindowRejections(a)

	h.checkRecovery()
	waitForSent(t, rec, 2)
	assertNoMoreSent(t, rec, 2)
	if !strings.Contains(rec.all()[1], "All models back") {
		t.Errorf("combined recovery message = %q, want %q", rec.all()[1], "All models back")
	}
}

// Sanity check: a request with no extra family at all (Sonnet-shaped, or
// every account exhausted account-wide from the start) still gets the
// plain "all models" start/recovery pair — the family-scoping above must
// not have broken the ordinary, pre-#230 case.
func TestEpisodeGeneralOnlyExhaustionAndRecovery(t *testing.T) {
	a := claudeAcct("a")
	h, p, rec := episodeRig(t, "4h", a)

	eta := time.Now().Add(45 * time.Minute)
	p.MarkExhausted(a, eta)
	h.reportExhaustion("", eta, true)
	waitForSent(t, rec, 1)
	if !strings.Contains(rec.all()[0], "All models held") {
		t.Fatalf("got %q, want an all-models start", rec.all()[0])
	}

	p.ClearExhausted(a)
	h.checkRecovery()
	waitForSent(t, rec, 2)
	assertNoMoreSent(t, rec, 2)
	if !strings.Contains(rec.all()[1], "All models back") {
		t.Errorf("got %q, want %q", rec.all()[1], "All models back")
	}
}

// WatchRecovery must actually notice a capacity signal on its own — issue
// #230's requirement that recovery not depend on any request — without
// the test itself calling checkRecovery.
func TestWatchRecoveryFiresOnCapacitySignalWithoutDirectCall(t *testing.T) {
	a := claudeAcct("a")
	h, p, rec := episodeRig(t, "4h", a)
	eta := time.Now().Add(20 * time.Minute)
	p.MarkWindowRejected(a, fableFamily, eta)
	h.reportExhaustion(fableFamily, eta, true)
	waitForSent(t, rec, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.WatchRecovery(ctx)
	// Let the watcher goroutine actually reach its first CapacitySignal()
	// subscription before firing one — otherwise this test's own race
	// (subscribe-after-fire) rather than WatchRecovery's behaviour would
	// decide the outcome. Production doesn't need this: a signal the
	// watcher hasn't started yet to catch is followed, eventually, by
	// another one or by its own reset timer.
	time.Sleep(20 * time.Millisecond)

	a.SetQuotaWindows([]pool.QuotaWindow{freshFableWindow()})
	p.ClearRecoveredWindowRejections(a) // signals capacity (issue #105) — the watcher must notice, unaided

	waitForSent(t, rec, 2)
	if !strings.Contains(rec.all()[1], "Fable back") {
		t.Errorf("watcher-driven recovery message = %q, want %q", rec.all()[1], "Fable back")
	}
}

// WatchRecovery must also notice a reset simply arriving with no capacity
// signal at all and no request to nudge Account.State()'s own lazy
// StateExhausted->StateOK flip — its own reset timer (nextEpisodeDeadline)
// is what has to fire this.
func TestWatchRecoveryFiresOnResetTimerWithoutAnySignal(t *testing.T) {
	a := claudeAcct("a")
	h, p, rec := episodeRig(t, "4h", a)
	eta := time.Now().Add(80 * time.Millisecond)
	p.MarkExhausted(a, eta)
	h.reportExhaustion("", eta, true)
	waitForSent(t, rec, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.WatchRecovery(ctx)

	waitForSent(t, rec, 2)
	if !strings.Contains(rec.all()[1], "All models back") {
		t.Errorf("timer-driven recovery message = %q, want %q", rec.all()[1], "All models back")
	}
}

// End-to-end: hold.go's real waitForReset call site must plumb the
// family and ETA through to reportExhaustion correctly, not just the
// direct unit-level calls above.
func TestHoldWiresIntoEpisodeNotification(t *testing.T) {
	a := claudeAcct("a")
	h, p, rec := episodeRig(t, "4h", a)
	until := time.Now().Add(50 * time.Millisecond)
	p.MarkWindowRejected(a, fableFamily, until)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	body := []byte(fableReqBody)
	deadline := time.Now().Add(4 * time.Hour)

	h.waitForReset(req, body, deadline, p.CapacitySignal())
	waitForSent(t, rec, 1)
	if !strings.Contains(rec.all()[0], "Fable held") {
		t.Errorf("hold.go's own call site produced %q, want a fable-held start", rec.all()[0])
	}
}
