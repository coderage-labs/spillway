package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coderage-labs/spillway/internal/notify"
)

func baseValidYAML() string {
	return "proxy:\n  port: 7654\n  host: 127.0.0.1\nupstream: https://api.anthropic.com\nlog:\n  level: info\n"
}

// An unknown event name is a typo that would otherwise silently disable an
// alert — the worst outcome for a feature whose whole job is telling
// someone something is wrong. Config load must refuse it and name the valid
// set.
func TestValidateRejectsUnknownNotifyEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	yaml := baseValidYAML() + "notify:\n  channels:\n    - name: phone\n      provider: ntfy\n      events: [exhaustd]\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("expected an error for the unknown event name \"exhaustd\"")
	}
	for _, want := range append([]string{"exhaustd"}, notify.ValidEvents()...) {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestValidateRejectsUnknownNotifyProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	yaml := baseValidYAML() + "notify:\n  channels:\n    - name: phone\n      provider: carrier-pigeon\n      events: [exhausted]\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("expected an error for the unknown provider")
	}
	if !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Errorf("error does not name the bad provider: %v", err)
	}
}

func TestValidateRejectsDuplicateChannelName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	yaml := baseValidYAML() + "notify:\n  channels:\n    - name: phone\n      provider: os\n" +
		"    - name: phone\n      provider: webhook\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(path); err == nil {
		t.Fatal("expected an error for a duplicate channel name")
	}
}

func TestValidateAcceptsWellFormedChannel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	yaml := baseValidYAML() + "notify:\n  channels:\n    - name: phone\n      provider: ntfy\n      events: [exhausted, held]\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if len(cfg.Notify.Channels) != 1 || cfg.Notify.Channels[0].Name != "phone" {
		t.Errorf("channels = %+v", cfg.Notify.Channels)
	}
}

// No notify block at all must load and validate exactly as before this
// feature existed (issue #101: off by default).
func TestValidateAcceptsNoNotifyBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	if err := os.WriteFile(path, []byte(baseValidYAML()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if len(cfg.Notify.Channels) != 0 {
		t.Errorf("channels = %+v, want none", cfg.Notify.Channels)
	}
}

func TestUpsertListFindRemoveNotifyChannel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")

	if err := UpsertNotifyChannel(path, NotifyChannelConfig{
		Name: "phone", Provider: "ntfy", Events: []string{"exhausted"},
	}); err != nil {
		t.Fatalf("UpsertNotifyChannel: %v", err)
	}

	list, err := ListNotifyChannels(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "phone" {
		t.Fatalf("list = %+v", list)
	}

	found, ok, err := FindNotifyChannel(path, "phone")
	if err != nil || !ok || found.Provider != "ntfy" {
		t.Fatalf("FindNotifyChannel = %+v, %v, %v", found, ok, err)
	}

	// Upsert again with the same name replaces rather than duplicates.
	if err := UpsertNotifyChannel(path, NotifyChannelConfig{
		Name: "phone", Provider: "webhook", Events: []string{"held"},
	}); err != nil {
		t.Fatalf("UpsertNotifyChannel (replace): %v", err)
	}
	list, _ = ListNotifyChannels(path)
	if len(list) != 1 || list[0].Provider != "webhook" {
		t.Fatalf("replace did not take effect: %+v", list)
	}

	if err := RemoveNotifyChannel(path, "phone"); err != nil {
		t.Fatalf("RemoveNotifyChannel: %v", err)
	}
	list, _ = ListNotifyChannels(path)
	if len(list) != 0 {
		t.Errorf("channel still present after remove: %+v", list)
	}

	if err := RemoveNotifyChannel(path, "phone"); err == nil {
		t.Error("removing a missing channel a second time should be an error")
	}
}
