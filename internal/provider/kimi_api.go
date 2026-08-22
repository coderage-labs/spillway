package provider

// Kimi provider: device-flow login, token refresh, and /usages quota reads.
// Endpoint shapes spiked from kimi-cli (design doc §12a, 2026-08-21).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// KimiAuthBase hosts the OAuth endpoints (device authorization + token).
	KimiAuthBase = "https://auth.kimi.com"
	// KimiClientID is the public OAuth client id (kimi-code; no scope sent).
	KimiClientID = "17e5f671-d194-4dfb-9706-5516cb48c098"
	// KimiUpstream serves the Anthropic shape (/v1/messages) Claude Code
	// uses per Kimi's own docs.
	KimiUpstream = "https://api.kimi.com/coding"

	deviceCodeGrant = "urn:ietf:params:oauth:grant-type:device_code"
)

// KimiDeviceAuth is the device_authorization response.
type KimiDeviceAuth struct {
	UserCode                string `json:"user_code"`
	DeviceCode              string `json:"device_code"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// KimiDeviceAuthorize starts the device flow (§12a: form body with just the
// client_id; the server returns the scope).
func KimiDeviceAuthorize(ctx context.Context, client *http.Client, authBase string) (*KimiDeviceAuth, error) {
	if authBase == "" {
		authBase = KimiAuthBase
	}
	form := url.Values{"client_id": {KimiClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		authBase+"/api/oauth/device_authorization", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient(client).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device authorization failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var da KimiDeviceAuth
	if err := json.Unmarshal(body, &da); err != nil {
		return nil, fmt.Errorf("device authorization malformed: %w", err)
	}
	if da.DeviceCode == "" || da.UserCode == "" {
		return nil, errors.New("device authorization response missing codes")
	}
	if da.Interval <= 0 {
		da.Interval = 5 // RFC 8628 default
	}
	return &da, nil
}

// KimiTokens is a token endpoint result.
type KimiTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

// ExpiresAtMs normalises the relative expiry to epoch milliseconds.
func (t *KimiTokens) ExpiresAtMs(now time.Time) int64 {
	if t.ExpiresIn <= 0 {
		return 0
	}
	return now.Add(time.Duration(t.ExpiresIn) * time.Second).UnixMilli()
}

// kimiToken posts a grant to the token endpoint and decodes either a token
// response or an RFC 8628 error.
func kimiToken(ctx context.Context, client *http.Client, authBase string, form url.Values) (*KimiTokens, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		authBase+"/api/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient(client).Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return nil, e.Error, nil
		}
		return nil, "", fmt.Errorf("token endpoint %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var t KimiTokens
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, "", fmt.Errorf("token response malformed: %w", err)
	}
	if t.AccessToken == "" {
		return nil, "", errors.New("token response missing access_token")
	}
	return &t, "", nil
}

// KimiPollDevice polls the token endpoint until the user approves, the
// device code expires, or ctx ends. Honours the server interval and
// slow_down; authorization_pending keeps polling (RFC 8628).
func KimiPollDevice(ctx context.Context, client *http.Client, authBase string, da *KimiDeviceAuth) (*KimiTokens, error) {
	if authBase == "" {
		authBase = KimiAuthBase
	}
	interval := time.Duration(da.Interval) * time.Second
	deadline := time.Now().Add(15 * time.Minute)
	if da.ExpiresIn > 0 {
		deadline = time.Now().Add(time.Duration(da.ExpiresIn) * time.Second)
	}
	for {
		toks, errCode, err := kimiToken(ctx, client, authBase, url.Values{
			"grant_type":  {deviceCodeGrant},
			"device_code": {da.DeviceCode},
			"client_id":   {KimiClientID},
		})
		if err != nil {
			return nil, err
		}
		switch errCode {
		case "":
			return toks, nil
		case "authorization_pending":
			// keep polling
		case "slow_down":
			interval += 5 * time.Second
		case "expired_token":
			return nil, errors.New("device code expired — run login again")
		case "access_denied":
			return nil, errors.New("login denied")
		default:
			return nil, fmt.Errorf("device poll error: %s", errCode)
		}
		if time.Now().Add(interval).After(deadline) {
			return nil, errors.New("device code expired — run login again")
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// KimiRefresh exchanges a refresh token at the same endpoint
// (grant_type=refresh_token; the response ROTATES the refresh token — the
// caller must persist the new one before serving waiters, §6.4).
func KimiRefresh(ctx context.Context, client *http.Client, authBase, refreshToken string) (*KimiTokens, error) {
	if authBase == "" {
		authBase = KimiAuthBase
	}
	toks, errCode, err := kimiToken(ctx, client, authBase, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {KimiClientID},
	})
	if err != nil {
		return nil, err
	}
	switch errCode {
	case "":
		if toks.RefreshToken == "" {
			return nil, errors.New("refresh response missing rotated refresh_token")
		}
		return toks, nil
	case "invalid_grant", "expired_token":
		return nil, fmt.Errorf("%w (%s)", ErrRefreshDead, errCode)
	default:
		return nil, fmt.Errorf("kimi refresh error: %s", errCode)
	}
}

// KimiUsage is one /usages window parsed into a tolerant shape. §12a does
// not pin the exact response fields, so the parser accepts several common
// layouts (see parseUsages).
type KimiUsage struct {
	Name    string
	Limit   float64
	Used    float64
	ResetAt time.Time
}

// FetchKimiUsages GETs {upstream}/v1/usages with the account's bearer token.
func FetchKimiUsages(ctx context.Context, client *http.Client, upstream, accessToken string) ([]KimiUsage, error) {
	if upstream == "" {
		upstream = KimiUpstream
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream+"/v1/usages", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient(client).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/usages %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return parseUsages(body)
}

// parseUsages parses the /usages response. The real shape (verified live
// 2026-08-21) uses STRING values and resetTime (RFC3339Nano):
//
//	{"usage":     {"limit":"100","used":"42","remaining":"58","resetTime":"..."},   // the 7-day window
//	 "limits":    [{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},
//	               "detail":{"limit":"100","used":"65","remaining":"35","resetTime":"..."}}],
//	 "parallel":  {"limit":"30"}}                                                    // concurrency cap
//
// A tolerant legacy layout (array / windows|usages|data wrapper) is still
// accepted as a fallback.
func parseUsages(body []byte) ([]KimiUsage, error) {
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("/usages malformed: %w", err)
	}
	if m, ok := raw.(map[string]any); ok {
		if _, hasUsage := m["usage"]; hasUsage || m["limits"] != nil {
			return parseKimiUsages(m)
		}
	}
	return parseUsagesLegacy(raw)
}

// parseKimiUsages parses the verified live shape.
func parseKimiUsages(m map[string]any) ([]KimiUsage, error) {
	var out []KimiUsage
	if u, ok := m["usage"].(map[string]any); ok {
		out = append(out, KimiUsage{
			// "7d", not "weekly": Claude's windows are named 5h / 7d / 7d-fable,
			// and a pool holding both providers has to be sortable and
			// comparable across them. Two names for one week put Kimi in its
			// own column in every listing that unions window names.
			Name:    "7d", // top-level usage is the 7-day window
			Limit:   numField(u, "limit"),
			Used:    numField(u, "used"),
			ResetAt: parseReset(strField(u, "resetTime")),
		})
	}
	if arr, ok := m["limits"].([]any); ok {
		for _, item := range arr {
			e, ok := item.(map[string]any)
			if !ok {
				continue
			}
			w, _ := e["window"].(map[string]any)
			d, _ := e["detail"].(map[string]any)
			out = append(out, KimiUsage{
				Name:    kimiWindowName(w),
				Limit:   numField(d, "limit"),
				Used:    numField(d, "used"),
				ResetAt: parseReset(strField(d, "resetTime")),
			})
		}
	}
	if p, ok := m["parallel"].(map[string]any); ok {
		out = append(out, KimiUsage{Name: "parallel", Limit: numField(p, "limit")})
	}
	if len(out) == 0 {
		return nil, errors.New("/usages: no windows parsed")
	}
	return out, nil
}

// kimiWindowName derives a window name from {duration, timeUnit}:
// 300 × TIME_UNIT_MINUTE → "5h".
func kimiWindowName(w map[string]any) string {
	dur := numField(w, "duration")
	unit := strField(w, "timeUnit")
	switch unit {
	case "TIME_UNIT_MINUTE":
		mins := int(dur)
		if mins%60 == 0 {
			return fmt.Sprintf("%dh", mins/60)
		}
		return fmt.Sprintf("%dm", mins)
	case "TIME_UNIT_HOUR":
		return fmt.Sprintf("%dh", int(dur))
	case "TIME_UNIT_DAY":
		return fmt.Sprintf("%dd", int(dur))
	case "TIME_UNIT_WEEK":
		// Days, not weeks: one vocabulary across providers, so a week is 7d
		// wherever it comes from.
		return fmt.Sprintf("%dd", int(dur)*7)
	}
	if unit != "" {
		return fmt.Sprintf("%s-%d", strings.ToLower(unit), int(dur))
	}
	return "window"
}

// parseUsagesLegacy tolerates the older guessed layouts: a single object, an
// array of windows, or a {"windows"|"usages"|"data": [...|{...}]} wrapper.
// Window fields: name/window/period, limit/total, used/usage/consumed,
// reset_at/resets_at/resetAt/reset (epoch s/ms or RFC3339).
func parseUsagesLegacy(raw any) ([]KimiUsage, error) {
	var windows []any
	if v, ok := raw.(map[string]any); ok {
		for _, key := range []string{"windows", "usages", "data"} {
			if inner, ok := v[key]; ok {
				raw = inner
				break
			}
		}
	}
	if raw == nil {
		return nil, errors.New("/usages: empty")
	}
	switch v := raw.(type) {
	case []any:
		windows = v
	case map[string]any:
		windows = []any{v}
	default:
		return nil, errors.New("/usages: unrecognised shape")
	}

	var out []KimiUsage
	for i, w := range windows {
		m, ok := w.(map[string]any)
		if !ok {
			continue
		}
		u := KimiUsage{
			Name:  strField(m, "name", "window", "period", "type"),
			Limit: numField(m, "limit", "total", "quota"),
			Used:  numField(m, "used", "usage", "consumed", "spent"),
		}
		if u.Name == "" {
			u.Name = fmt.Sprintf("window-%d", i)
		}
		if s := strField(m, "reset_at", "resets_at", "resetAt", "reset"); s != "" {
			u.ResetAt = parseReset(s)
		} else if n := numField(m, "reset_at", "resets_at", "resetAt", "reset"); n != 0 {
			u.ResetAt = parseResetNum(n)
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		return nil, errors.New("/usages: no windows parsed")
	}
	return out, nil
}

func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch s := v.(type) {
			case string:
				if s != "" {
					return s
				}
			case float64:
				return strconv.FormatFloat(s, 'f', -1, 64)
			}
		}
	}
	return ""
}

func numField(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				return n
			case string:
				if f, err := strconv.ParseFloat(n, 64); err == nil {
					return f
				}
			}
		}
	}
	return 0
}

func parseReset(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return parseResetNum(f)
	}
	return time.Time{}
}

func parseResetNum(f float64) time.Time {
	if f < 1e12 {
		return time.Unix(int64(f), 0)
	}
	return time.UnixMilli(int64(f))
}

func httpClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 15 * time.Second}
}
