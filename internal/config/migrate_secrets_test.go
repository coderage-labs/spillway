package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coderage-labs/spillway/internal/notify"
	"github.com/coderage-labs/spillway/internal/secrets"
	"github.com/coderage-labs/spillway/internal/testmode"
)

func TestMigrateInlineSecrets(t *testing.T) {
	dir := t.TempDir()
	spillwayPath := filepath.Join(dir, "spillway.yaml")
	yaml := `proxy:
  port: 7654
  host: 127.0.0.1
upstream: https://api.anthropic.com
accounts:
  - name: work
    type: claude-oauth
    accessToken: inline-tok
    refreshToken: inline-ref
    expiresAt: 4102444800000
  - name: already-migrated
    type: claude-oauth
    expiresAt: 4102444800000
log:
  level: info
`
	if err := os.WriteFile(spillwayPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	store := secrets.NewFake()

	migrated, migratedChannels, err := MigrateInlineSecrets(spillwayPath, store)
	if err != nil {
		t.Fatalf("MigrateInlineSecrets: %v", err)
	}
	if len(migrated) != 1 || migrated[0] != "work" {
		t.Errorf("migrated = %v", migrated)
	}
	if migratedChannels != nil {
		t.Errorf("migratedChannels = %v, want nil (no channels in this yaml)", migratedChannels)
	}

	raw, _ := os.ReadFile(spillwayPath)
	for _, tok := range []string{"inline-tok", "inline-ref", "accessToken", "refreshToken"} {
		if strings.Contains(string(raw), tok) {
			t.Errorf("yaml still contains %q after migration:\n%s", tok, raw)
		}
	}
	s, err := store.Get("work")
	if err != nil {
		t.Fatal(err)
	}
	if s.AccessToken != "inline-tok" || s.RefreshToken != "inline-ref" {
		t.Errorf("stored secrets = %+v", s)
	}
	testmode.AssertPrivateFile(t, spillwayPath)

	// Second run is a no-op.
	migrated, migratedChannels, err = MigrateInlineSecrets(spillwayPath, store)
	if err != nil {
		t.Fatal(err)
	}
	if migrated != nil {
		t.Errorf("second migration = %v, want nil", migrated)
	}
	if migratedChannels != nil {
		t.Errorf("second migration channels = %v, want nil", migratedChannels)
	}
}

// A channel someone hand-edits credential material into must be caught and
// scrubbed exactly like an inline account token (issue #101) — the whole
// point of storing it separately is that a config file is safe to paste
// into a bug report, and a hand-edit is exactly how that invariant breaks.
func TestMigrateInlineNotifyChannelSecrets(t *testing.T) {
	dir := t.TempDir()
	spillwayPath := filepath.Join(dir, "spillway.yaml")
	yaml := `proxy:
  port: 7654
  host: 127.0.0.1
upstream: https://api.anthropic.com
notify:
  channels:
    - name: phone
      provider: ntfy
      events: [exhausted]
      topic: my-guessable-topic
      token: tk_inlinesecret
log:
  level: info
`
	if err := os.WriteFile(spillwayPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	store := secrets.NewFake()

	_, migratedChannels, err := MigrateInlineSecrets(spillwayPath, store)
	if err != nil {
		t.Fatalf("MigrateInlineSecrets: %v", err)
	}
	if len(migratedChannels) != 1 || migratedChannels[0] != "phone" {
		t.Fatalf("migratedChannels = %v, want [phone]", migratedChannels)
	}

	raw, _ := os.ReadFile(spillwayPath)
	for _, tok := range []string{"my-guessable-topic", "tk_inlinesecret", "topic:", "token:"} {
		if strings.Contains(string(raw), tok) {
			t.Errorf("yaml still contains %q after migration:\n%s", tok, raw)
		}
	}

	blob, err := store.GetRaw(notify.ChannelKey("phone"))
	if err != nil {
		t.Fatal(err)
	}
	var dest notify.Destination
	if err := json.Unmarshal(blob, &dest); err != nil {
		t.Fatal(err)
	}
	if dest.Topic != "my-guessable-topic" || dest.Token != "tk_inlinesecret" {
		t.Errorf("stored destination = %+v", dest)
	}
}

func TestSecretsMissingEntryError(t *testing.T) {
	_, err := secrets.NewFake().Get("ghost@example.com")
	if !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "ghost@example.com") {
		t.Errorf("error does not name the account: %v", err)
	}
}
