package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	migrated, err := MigrateInlineSecrets(spillwayPath, store)
	if err != nil {
		t.Fatalf("MigrateInlineSecrets: %v", err)
	}
	if len(migrated) != 1 || migrated[0] != "work" {
		t.Errorf("migrated = %v", migrated)
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
	migrated, err = MigrateInlineSecrets(spillwayPath, store)
	if err != nil {
		t.Fatal(err)
	}
	if migrated != nil {
		t.Errorf("second migration = %v, want nil", migrated)
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
