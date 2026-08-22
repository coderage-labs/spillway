package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Claude OAuth refresh contract, verified against the live endpoint.
const (
	ClaudeTokenURL = "https://platform.claude.com/v1/oauth/token"
	claudeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
)

// ErrRefreshDead marks a permanently rejected refresh token (400/401) — the
// account needs re-login, not a retry.
var ErrRefreshDead = errors.New("refresh token rejected")

// TokenResult is a normalized refresh response.
type TokenResult struct {
	AccessToken  string
	RefreshToken string // empty → keep the old one (providers may omit it)
	ExpiresAt    int64  // epoch milliseconds, normalized
}

// Refresher exchanges refresh tokens for access tokens.
type Refresher struct {
	// TokenURL is overridable for tests; empty uses ClaudeTokenURL.
	TokenURL string
	// HTTPClient defaults to a 15s-timeout client.
	HTTPClient *http.Client
	// Now defaults to time.Now (testability).
	Now func() time.Time
}

// NewRefresher returns a Refresher against the real token endpoint.
func NewRefresher() *Refresher { return &Refresher{} }

func (r *Refresher) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Refresh exchanges refreshToken. 5xx and network errors are retried once;
// 400/401 return ErrRefreshDead.
func (r *Refresher) Refresh(ctx context.Context, refreshToken string) (*TokenResult, error) {
	url := r.TokenURL
	if url == "" {
		url = ClaudeTokenURL
	}
	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	payload, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     claudeClientID,
	})
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err // network: retry once
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("%w (upstream %d)", ErrRefreshDead, resp.StatusCode)
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("token endpoint %d", resp.StatusCode) // retry once
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("token endpoint unexpected status %d", resp.StatusCode)
		}
		return parseTokenResponse(body, r.now())
	}
	return nil, lastErr
}

func parseTokenResponse(body []byte, now time.Time) (*TokenResult, error) {
	var tr struct {
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"`
		ExpiresAt    json.Number `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("token endpoint malformed response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("token endpoint response missing access_token")
	}

	res := &TokenResult{AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken}
	switch {
	case tr.ExpiresAt != "":
		v, err := tr.ExpiresAt.Float64()
		if err != nil {
			return nil, fmt.Errorf("expires_at: %v", err)
		}
		res.ExpiresAt = int64(normalizeMs(v))
	case tr.ExpiresIn != "":
		v, err := tr.ExpiresIn.Float64()
		if err != nil {
			return nil, fmt.Errorf("expires_in: %v", err)
		}
		res.ExpiresAt = now.UnixMilli() + int64(normalizeMs(v))
	default:
		return nil, errors.New("token endpoint response missing expiry")
	}
	return res, nil
}

// normalizeMs converts second-based values to milliseconds; values already
// past 10^12 are taken as milliseconds (epoch-ms is ~1.7e12 today).
func normalizeMs(v float64) float64 {
	if v < 1e12 {
		return v * 1000
	}
	return v
}
