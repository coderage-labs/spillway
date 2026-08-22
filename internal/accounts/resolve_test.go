package accounts

import (
	"strings"
	"testing"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/secrets"
)

func TestResolveYAMLRoundTrip(t *testing.T) {
	store := secrets.NewFake()
	if err := store.Set("work", secrets.Secrets{AccessToken: "acc", RefreshToken: "ref"}); err != nil {
		t.Fatal(err)
	}
	a := config.AccountConfig{
		Name: "work", Type: "claude-oauth",
		ExpiresAt: 4102444800000, AccountUUID: "11111111-2222-3333-4444-555555555555",
		Upstream: "https://custom.example.com",
	}
	acct, err := ResolveYAML(a, store)
	if err != nil {
		t.Fatalf("ResolveYAML: %v", err)
	}
	access, refresh, exp := acct.Credentials()
	if access != "acc" || refresh != "ref" || exp != 4102444800000 {
		t.Errorf("credentials = (%q, %q, %d)", access, refresh, exp)
	}
	if acct.AccountUUID != a.AccountUUID || acct.Upstream != a.Upstream {
		t.Errorf("metadata lost: %+v", acct)
	}
}

func TestResolveYAMLMissingSecrets(t *testing.T) {
	_, err := ResolveYAML(config.AccountConfig{Name: "ghost", Type: "claude-oauth"}, secrets.NewFake())
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("err = %v, want clear error naming the account", err)
	}
}
