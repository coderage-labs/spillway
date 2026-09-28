package notify

// Tests for NotifyEpisode (issue #230): the exhaustion/hold/recovery path
// deliberately bypasses shouldSend's coalesce map, because
// internal/proxy's episode tracker is already the dedup — see
// NotifyEpisode's own comment for why layering the flat 10-minute window
// on top of that would either resend on its own clock (the original bug)
// or drop a second, genuinely distinct transition that lands inside the
// same 10 minutes.

import (
	"testing"
	"time"
)

// The headline difference from Notify: two calls on the very same key,
// back to back, both get delivered — nothing here suppresses a repeat,
// because the caller decided each call was worth sending.
func TestNotifyEpisodeDoesNotCoalesce(t *testing.T) {
	n, rec := testNotifier()
	n.NotifyEpisode(EventHeld, "t1", "b1")
	n.NotifyEpisode(EventHeld, "t2", "b2")
	waitFor(t, &rec.count, 2)
	if got := rec.count.Load(); got != 2 {
		t.Errorf("sent %d, want 2 — NotifyEpisode must not share Notify's coalesce", got)
	}
}

// Plant: routing NotifyEpisode through shouldSend (Notify's own dedup)
// would coalesce these exactly like TestNotifyCoalescesRepeats does for
// Notify — which is precisely the bug #230 reports (a message suppressed
// for 10 minutes regardless of it being a distinct transition). Restoring
// NotifyEpisode's real bypass makes this pass; routing it through
// shouldSend makes it fail with "sent 1, want 2".
func TestNotifyEpisodeFiresRepeatsEvenWithinCoalesceWindow(t *testing.T) {
	n, rec := testNotifier()
	for i := 0; i < 3; i++ {
		n.NotifyEpisode(EventExhausted, "spillway: all models held", "All models held until 08:14 (1h36m)")
	}
	waitFor(t, &rec.count, 3)
	if got := rec.count.Load(); got != 3 {
		t.Errorf("sent %d, want 3 — the coordinator's per-episode dedup already decided each call "+
			"was distinct; a second dedup layer here must not second-guess it", got)
	}
}

// No channels configured (today's local-only default) must still reach
// the platform notifier via NotifyEpisode, exactly as Notify does.
func TestNotifyEpisodeNoChannelsUsesLocal(t *testing.T) {
	n, rec := testNotifier()
	n.NotifyEpisode(EventRecovered, "spillway: fable back", "Fable back")
	waitFor(t, &rec.count, 1)
	if got := rec.sent(); len(got) != 1 || got[0] != "spillway: fable back|Fable back" {
		t.Errorf("sent = %v, want one local notification with the given title/body", got)
	}
}

// Channel fan-out and per-channel event subscription work the same way
// for NotifyEpisode as for Notify.
func TestNotifyEpisodeFansOutOnlyToSubscribedChannels(t *testing.T) {
	recovered := &recordingProvider{}
	overage := &recordingProvider{}

	n := &Notifier{last: map[string]time.Time{}}
	n.channels = []channel{
		testChannel("phone", []string{EventRecovered}, recovered),
		testChannel("desktop", []string{EventOverageCap}, overage),
	}

	n.NotifyEpisode(EventRecovered, "spillway: all models back", "All models back")
	waitForCount(t, recovered.count, 1)
	time.Sleep(30 * time.Millisecond)
	if got := overage.count(); got != 0 {
		t.Errorf("desktop (not subscribed to recovered) got %d sends, want 0", got)
	}
	if got := recovered.count(); got != 1 {
		t.Errorf("phone got %d sends, want 1", got)
	}
}

// EventRecovered must be accepted by the same validation every other event
// name goes through (config.Validate's error message names ValidEvents).
func TestEventRecoveredIsValid(t *testing.T) {
	if !IsValidEvent(EventRecovered) {
		t.Error("IsValidEvent(EventRecovered) = false, want true")
	}
	found := false
	for _, e := range ValidEvents() {
		if e == EventRecovered {
			found = true
		}
	}
	if !found {
		t.Error("ValidEvents() does not list EventRecovered")
	}
}
