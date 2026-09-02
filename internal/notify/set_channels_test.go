package notify

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSetChannelsOSProviderNeedsNoCredential(t *testing.T) {
	n, rec := testNotifier()
	warnings := n.SetChannels(
		[]ChannelSpec{{Name: "desktop", Provider: "os", Events: []string{EventExhausted}}},
		func(string) (Destination, error) {
			t.Fatal("os provider must never consult the credential source")
			return Destination{}, nil
		},
	)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	n.Notify(EventExhausted, "k", "t", "b")
	waitFor(t, &rec.count, 1)
}

// A channel with no credential yet must be disabled with a warning naming
// the channel and the fix — never a silent no-op, and never fatal to the
// daemon (issues #65, #81: a broken keychain read must not take it down).
func TestSetChannelsMissingCredentialDisablesWithWarning(t *testing.T) {
	n := &Notifier{last: map[string]time.Time{}}
	warnings := n.SetChannels(
		[]ChannelSpec{{Name: "phone", Provider: "ntfy", Events: []string{EventExhausted}}},
		func(string) (Destination, error) { return Destination{}, errors.New("no credential in keychain") },
	)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
	if !strings.Contains(warnings[0], "phone") || !strings.Contains(warnings[0], "spillway notify set phone") {
		t.Errorf("warning does not name the channel and the fix: %q", warnings[0])
	}

	failing := &recordingProvider{}
	n.channels[0].provider = failing // in case SetChannels wired a real one
	n.Notify(EventExhausted, "k", "t", "b")
	time.Sleep(30 * time.Millisecond)
	if got := failing.count(); got != 0 {
		t.Errorf("a disabled channel must never be sent to, got %d sends", got)
	}
}

func TestSetChannelsUnknownProviderDisablesWithWarning(t *testing.T) {
	n := &Notifier{last: map[string]time.Time{}}
	warnings := n.SetChannels(
		[]ChannelSpec{{Name: "carrier", Provider: "pigeon", Events: []string{EventExhausted}}},
		func(string) (Destination, error) { return Destination{}, nil },
	)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "carrier") || !strings.Contains(warnings[0], "pigeon") {
		t.Errorf("warnings = %v, want one naming carrier and pigeon", warnings)
	}
}

func TestSetChannelsReplacesPreviousList(t *testing.T) {
	n := &Notifier{last: map[string]time.Time{}}
	n.SetChannels([]ChannelSpec{{Name: "a", Provider: "os", Events: []string{EventExhausted}}}, nil)
	if len(n.channels) != 1 {
		t.Fatalf("first SetChannels: got %d channels", len(n.channels))
	}
	n.SetChannels([]ChannelSpec{{Name: "b", Provider: "os", Events: []string{EventHeld}}}, nil)
	if len(n.channels) != 1 || n.channels[0].name != "b" {
		t.Errorf("SetChannels did not replace the list: %+v", n.channels)
	}
}
