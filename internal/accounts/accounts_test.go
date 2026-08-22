package accounts

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

const futureMs = 4_102_444_800_000 // 2100-01-01

func goodJSON() string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-abc","refreshToken":"sk-ant-ort-xyz","expiresAt":%d,"scopes":["user:inference","user:profile"],"subscriptionType":"max"}}`, futureMs)
}

func TestLoadClaudeOK(t *testing.T) {
	o, err := LoadClaude(SourceFunc(func() ([]byte, error) { return []byte(goodJSON()), nil }), time.Now())
	if err != nil {
		t.Fatalf("LoadClaude: %v", err)
	}
	if o.AccessToken != "sk-ant-oat-abc" {
		t.Errorf("accessToken = %q", o.AccessToken)
	}
	if o.SubscriptionType != "max" {
		t.Errorf("subscriptionType = %q", o.SubscriptionType)
	}
	if len(o.Scopes) != 2 || o.Scopes[0] != "user:inference" {
		t.Errorf("scopes = %v", o.Scopes)
	}
}

func TestLoadClaudeExpiredRefused(t *testing.T) {
	past := time.Now().Add(-time.Hour).UnixMilli()
	raw := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok","expiresAt":%d}}`, past)
	_, err := LoadClaude(SourceFunc(func() ([]byte, error) { return []byte(raw), nil }), time.Now())
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("err = %v, want expiry refusal", err)
	}
}

func TestLoadClaudeEmptyTokenRefused(t *testing.T) {
	raw := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"","expiresAt":%d}}`, futureMs)
	_, err := LoadClaude(SourceFunc(func() ([]byte, error) { return []byte(raw), nil }), time.Now())
	if err == nil || !strings.Contains(err.Error(), "accessToken is empty") {
		t.Errorf("err = %v, want empty-token refusal", err)
	}
}

func TestLoadClaudeMalformedJSON(t *testing.T) {
	_, err := LoadClaude(SourceFunc(func() ([]byte, error) { return []byte("{not json"), nil }), time.Now())
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Errorf("err = %v, want malformed error", err)
	}
}

func TestLoadClaudeMissingCreds(t *testing.T) {
	_, err := LoadClaude(SourceFunc(func() ([]byte, error) { return nil, errors.New("item not found") }), time.Now())
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("err = %v, want unavailable error", err)
	}
}

func TestBearerInjector(t *testing.T) {
	r, err := http.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Api-Key", "sk-ant-api-should-be-dropped")
	r.Header.Set("Anthropic-Beta", "fine-grained-tool-streaming-2025-05-14")
	r.Header.Set("User-Agent", "claude-cli/2.1.44")

	BearerInjector("sk-ant-oat-injected")(r)

	if got := r.Header.Get("Authorization"); got != "Bearer sk-ant-oat-injected" {
		t.Errorf("Authorization = %q", got)
	}
	if got := r.Header.Get("X-Api-Key"); got != "" {
		t.Errorf("x-api-key not dropped: %q", got)
	}
	if got := r.Header.Get("Anthropic-Beta"); got != "fine-grained-tool-streaming-2025-05-14" {
		t.Errorf("anthropic-beta mutated: %q", got)
	}
	if got := r.Header.Get("User-Agent"); got != "claude-cli/2.1.44" {
		t.Errorf("user-agent mutated: %q", got)
	}
}
