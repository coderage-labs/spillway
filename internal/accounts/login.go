package accounts

// PKCE login flow for `spillway login claude`.
//
// Two redirect modes: a loopback callback on localhost:54545 (see
// loopback.go), and the manual code#state paste page as a fallback for
// headless use. Both were verified against the live endpoint.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// ClaudeAuthorizeURL is the Claude Code OAuth authorize endpoint.
	ClaudeAuthorizeURL = "https://claude.ai/oauth/authorize"
	// ClaudeScopes mirrors the official CLI's requested scopes.
	ClaudeScopes = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	// ClaudeProfileURL yields account/org identity for an access token.
	ClaudeProfileURL = "https://api.anthropic.com/api/oauth/profile"
	// ManualRedirectURI is the redirect the official CLI's manual flow
	// uses: the page shows the code as `code#state` for paste-back.
	ManualRedirectURI = "https://platform.claude.com/oauth/code/callback"
)

// PKCE holds one login attempt's verifier/challenge/state.
type PKCE struct {
	Verifier  string
	Challenge string
	State     string
}

// GeneratePKCE creates a fresh verifier (32 random bytes, base64url),
// S256 challenge and state.
func GeneratePKCE() (*PKCE, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	s := make([]byte, 32)
	if _, err := rand.Read(s); err != nil {
		return nil, err
	}
	return &PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
		State:     base64.RawURLEncoding.EncodeToString(s),
	}, nil
}

// AuthorizeURL builds the browser URL for a login attempt.
func AuthorizeURL(redirectURI string, pkce *PKCE) string {
	q := url.Values{}
	q.Set("code", "true")
	q.Set("client_id", claudeClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", ClaudeScopes)
	q.Set("code_challenge", pkce.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", pkce.State)
	return ClaudeAuthorizeURL + "?" + q.Encode()
}

// ParseCode accepts the three paste forms: `code#state` (official CLI
// manual page), a full callback URL (?code=&state=), or a bare code. When a
// state is present it must match.
func ParseCode(input, expectedState string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", errors.New("empty authorization code")
	}
	if i := strings.Index(trimmed, "#"); i >= 0 {
		code, state := trimmed[:i], trimmed[i+1:]
		if state != expectedState {
			return "", errors.New("OAuth state mismatch")
		}
		return code, nil
	}
	if u, err := url.Parse(trimmed); err == nil && u.Query().Get("code") != "" {
		if st := u.Query().Get("state"); st != "" && st != expectedState {
			return "", errors.New("OAuth state mismatch")
		}
		return u.Query().Get("code"), nil
	}
	return trimmed, nil
}

// LoginTokens is the exchange result plus the profile identity.
type LoginTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // epoch milliseconds
}

// ExchangeCode swaps an authorization code for tokens (grant_type
// authorization_code with the PKCE verifier).
func ExchangeCode(ctx context.Context, client *http.Client, tokenURL, code string, pkce *PKCE, redirectURI string) (*LoginTokens, error) {
	if tokenURL == "" {
		tokenURL = ClaudeTokenURL
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	payload, err := json.Marshal(map[string]string{
		"code":          code,
		"state":         pkce.State,
		"grant_type":    "authorization_code",
		"client_id":     claudeClientID,
		"redirect_uri":  redirectURI,
		"code_verifier": pkce.Verifier,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	res, err := parseTokenResponse(body, time.Now())
	if err != nil {
		return nil, err
	}
	if res.RefreshToken == "" {
		return nil, errors.New("token exchange response missing refresh_token")
	}
	return &LoginTokens{AccessToken: res.AccessToken, RefreshToken: res.RefreshToken, ExpiresAt: res.ExpiresAt}, nil
}

// Profile is the account identity behind an access token.
type Profile struct {
	AccountUUID string
	Email       string
	OrgUUID     string
	OrgName     string
}

// FetchProfile reads the OAuth profile to capture accountUuid/orgUuid —
// the accountUuid is what the proxy rewrites into metadata.user_id.
func FetchProfile(ctx context.Context, client *http.Client, profileURL, accessToken string) (*Profile, error) {
	if profileURL == "" {
		profileURL = ClaudeProfileURL
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, profileURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("profile fetch failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var data struct {
		Account struct {
			UUID  string `json:"uuid"`
			Email string `json:"email"`
		} `json:"account"`
		Organization struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("profile malformed: %w", err)
	}
	return &Profile{
		AccountUUID: data.Account.UUID,
		Email:       data.Account.Email,
		OrgUUID:     data.Organization.UUID,
		OrgName:     data.Organization.Name,
	}, nil
}
