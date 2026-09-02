package main

// These tests exercise promptNotifyChannel and buildNotifier directly,
// never runNotifySet/runNotifyTest/runNotifyList/runNotifyRemove — those
// call openSecrets(), which on a machine with a real OS keychain reaches it
// for real (see resolve_test.go's comment on the same split for
// login/accounts). secrets.Fake stands in for the store everywhere here.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/notify"
	"github.com/coderage-labs/spillway/internal/secrets"
)

func promptWith(input string) *bufio.Reader { return bufio.NewReader(strings.NewReader(input)) }

func TestPromptNotifyChannelWebhook(t *testing.T) {
	var out bytes.Buffer
	spec, dest, err := promptNotifyChannel(
		promptWith("webhook\nexhausted,held\nhttps://hooks.example.com/x\ntk_abc\n"),
		&out, "team", config.NotifyChannelConfig{})
	if err != nil {
		t.Fatalf("promptNotifyChannel: %v", err)
	}
	if spec.Name != "team" || spec.Provider != "webhook" {
		t.Errorf("spec = %+v", spec)
	}
	if len(spec.Events) != 2 || spec.Events[0] != "exhausted" || spec.Events[1] != "held" {
		t.Errorf("events = %v", spec.Events)
	}
	if dest.URL != "https://hooks.example.com/x" || dest.Token != "tk_abc" {
		t.Errorf("dest = %+v", dest)
	}
}

func TestPromptNotifyChannelNtfyBlankEventsMeansAll(t *testing.T) {
	var out bytes.Buffer
	spec, dest, err := promptNotifyChannel(
		promptWith("ntfy\n\nmy-random-topic\n\n\n"),
		&out, "phone", config.NotifyChannelConfig{})
	if err != nil {
		t.Fatalf("promptNotifyChannel: %v", err)
	}
	if len(spec.Events) != len(notify.ValidEvents()) {
		t.Errorf("blank events should default to every valid event, got %v", spec.Events)
	}
	if dest.Topic != "my-random-topic" {
		t.Errorf("dest = %+v", dest)
	}
	if dest.URL != "" || dest.Token != "" {
		t.Errorf("blank prompts must not fabricate a URL/token: %+v", dest)
	}
}

func TestPromptNotifyChannelOSNeedsNoCredential(t *testing.T) {
	var out bytes.Buffer
	spec, dest, err := promptNotifyChannel(promptWith("os\novercage-cap\n"), &out, "desktop", config.NotifyChannelConfig{})
	// The events line here is deliberately a typo ("overage-cap" misspelled)
	// to prove it's rejected rather than silently accepted.
	if err == nil {
		t.Fatalf("expected an error for the misspelled event, got spec=%+v dest=%+v", spec, dest)
	}
}

func TestPromptNotifyChannelRejectsUnknownProvider(t *testing.T) {
	var out bytes.Buffer
	_, _, err := promptNotifyChannel(promptWith("carrier-pigeon\n"), &out, "x", config.NotifyChannelConfig{})
	if err == nil {
		t.Fatal("expected an error for an unknown provider")
	}
}

func TestPromptNotifyChannelPushoverRequiresBothFields(t *testing.T) {
	var out bytes.Buffer
	_, _, err := promptNotifyChannel(promptWith("pushover\n\napptoken\n"), &out, "phone", config.NotifyChannelConfig{})
	if err == nil {
		t.Fatal("expected an error when the user key prompt is left blank")
	}
}

func TestPromptNotifyChannelKeepsExistingProviderAsDefault(t *testing.T) {
	var out bytes.Buffer
	// Blank line at the provider prompt should keep the existing value.
	spec, _, err := promptNotifyChannel(promptWith("\n\nsome-topic\n\n\n"), &out, "phone",
		config.NotifyChannelConfig{Name: "phone", Provider: "ntfy", Events: []string{"exhausted"}})
	if err != nil {
		t.Fatalf("promptNotifyChannel: %v", err)
	}
	if spec.Provider != "ntfy" {
		t.Errorf("provider = %q, want the existing ntfy to be kept as default", spec.Provider)
	}
}

func TestBuildNotifierNoChannelsIsLocalOnly(t *testing.T) {
	cfg := &config.Config{}
	store := secrets.NewFake()
	n, warnings := buildNotifier(cfg, store)
	if n == nil {
		t.Fatal("buildNotifier returned nil")
	}
	if warnings != nil {
		t.Errorf("warnings = %v, want none with no channels configured", warnings)
	}
}

func TestBuildNotifierWarnsOnMissingCredential(t *testing.T) {
	cfg := &config.Config{}
	cfg.Notify.Channels = []config.NotifyChannelConfig{
		{Name: "phone", Provider: "ntfy", Events: []string{"exhausted"}},
	}
	store := secrets.NewFake()
	_, warnings := buildNotifier(cfg, store)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "phone") {
		t.Errorf("warnings = %v, want one naming phone", warnings)
	}
}

func TestBuildNotifierResolvesStoredCredential(t *testing.T) {
	cfg := &config.Config{}
	cfg.Notify.Channels = []config.NotifyChannelConfig{
		{Name: "phone", Provider: "ntfy", Events: []string{"exhausted"}},
	}
	store := secrets.NewFake()
	blob, _ := json.Marshal(notify.Destination{Topic: "abc123"})
	if err := store.SetRaw(notify.ChannelKey("phone"), blob); err != nil {
		t.Fatal(err)
	}
	_, warnings := buildNotifier(cfg, store)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none once a credential is stored", warnings)
	}
}
