package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeneratePKCE(t *testing.T) {
	p, err := GeneratePKCE()
	if err != nil {
		t.Fatal(err)
	}
	// 32 bytes base64url → 43 chars, no padding.
	if len(p.Verifier) != 43 {
		t.Errorf("verifier length = %d, want 43", len(p.Verifier))
	}
	sum := sha256.Sum256([]byte(p.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if p.Challenge != want {
		t.Errorf("challenge = %q, want %q", p.Challenge, want)
	}
	if len(p.State) != 43 {
		t.Errorf("state length = %d", len(p.State))
	}
	p2, _ := GeneratePKCE()
	if p2.Verifier == p.Verifier || p2.State == p.State {
		t.Error("PKCE values not unique")
	}
}

func TestAuthorizeURL(t *testing.T) {
	p := &PKCE{Verifier: "v", Challenge: "ch", State: "st"}
	u := AuthorizeURL("https://platform.claude.com/oauth/code/callback", p)
	for _, want := range []string{
		"https://claude.ai/oauth/authorize?",
		"client_id=" + claudeClientID,
		"response_type=code",
		"code_challenge=ch",
		"code_challenge_method=S256",
		"state=st",
		"redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("authorize URL missing %q:\n%s", want, u)
		}
	}
	if !strings.Contains(u, "scope=") || !strings.Contains(u, "user%3Ainference") {
		t.Errorf("scopes missing:\n%s", u)
	}
}

func TestParseCode(t *testing.T) {
	cases := []struct {
		name, input, state, want string
		wantErr                  bool
	}{
		{"code#state", "abc123#state-xyz", "state-xyz", "abc123", false},
		{"code#state mismatch", "abc123#wrong", "state-xyz", "", true},
		{"callback URL", "http://localhost:1234/callback?code=abc123&state=state-xyz", "state-xyz", "abc123", false},
		{"callback URL state mismatch", "http://localhost/callback?code=abc&state=nope", "state-xyz", "", true},
		{"bare code", "abc123", "state-xyz", "abc123", false},
		{"whitespace", "  abc123#state-xyz  \n", "state-xyz", "abc123", false},
		{"empty", "", "state-xyz", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, err := ParseCode(tc.input, tc.state)
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error, got code %q", code)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCode: %v", err)
			}
			if code != tc.want {
				t.Errorf("code = %q, want %q", code, tc.want)
			}
		})
	}
}

func TestExchangeCode(t *testing.T) {
	pkce := &PKCE{Verifier: "verifier-abc", Challenge: "c", State: "state-xyz"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			t.Errorf("exchange body not JSON: %v", err)
		}
		checks := map[string]string{
			"grant_type":    "authorization_code",
			"client_id":     claudeClientID,
			"code":          "the-code",
			"state":         "state-xyz",
			"code_verifier": "verifier-abc",
			"redirect_uri":  ManualRedirectURI,
		}
		for k, want := range checks {
			if req[k] != want {
				t.Errorf("exchange %s = %q, want %q", k, req[k], want)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
	}))
	defer srv.Close()

	tokens, err := ExchangeCode(context.Background(), nil, srv.URL, "the-code", pkce, ManualRedirectURI)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tokens.AccessToken != "new-access" || tokens.RefreshToken != "new-refresh" {
		t.Errorf("tokens = %+v", tokens)
	}
	if tokens.ExpiresAt < 4102444800000/10 { // sanity: ~1h from now, normalized to ms
		t.Errorf("expiresAt = %d, not normalized to ms", tokens.ExpiresAt)
	}
}

func TestExchangeCodeError(t *testing.T) {
	pkce := &PKCE{Verifier: "v", State: "s"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	}))
	defer srv.Close()
	if _, err := ExchangeCode(context.Background(), nil, srv.URL, "bad", pkce, ManualRedirectURI); err == nil {
		t.Error("want exchange error")
	}
}

func TestFetchProfile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "Bearer the-token" {
			t.Errorf("Authorization = %q", a)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"account":{"uuid":"acc-uuid-1","email":"u@example.com"},"organization":{"uuid":"org-uuid-1","name":"Acme"}}`)
	}))
	defer srv.Close()

	p, err := FetchProfile(context.Background(), nil, srv.URL, "the-token")
	if err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	if p.AccountUUID != "acc-uuid-1" || p.OrgUUID != "org-uuid-1" || p.OrgName != "Acme" || p.Email != "u@example.com" {
		t.Errorf("profile = %+v", p)
	}
}
