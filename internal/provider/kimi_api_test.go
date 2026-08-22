package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// deviceFlowServer mocks the kimi auth host: one device_authorization, then
// token polls that stay pending until `approveAfter` polls have happened.
type deviceFlowServer struct {
	*httptest.Server
	polls      atomic.Int32
	deviceCode string
}

func newDeviceFlowServer(t *testing.T, approveAfter int32) *deviceFlowServer {
	t.Helper()
	d := &deviceFlowServer{deviceCode: "dev-code-123"}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/oauth/device_authorization":
			if err := r.ParseForm(); err != nil {
				t.Errorf("device_authorization not form: %v", err)
			}
			if got := r.Form.Get("client_id"); got != KimiClientID {
				t.Errorf("client_id = %q", got)
			}
			if r.Form.Get("scope") != "" {
				t.Error("scope must not be sent (server returns it)")
			}
			fmt.Fprint(w, `{"user_code":"ABCD-EFGH","device_code":"dev-code-123","verification_uri_complete":"https://auth.kimi.com/device?code=ABCD-EFGH","expires_in":600,"interval":1}`)
		case "/api/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("token not form: %v", err)
			}
			if r.Form.Get("grant_type") != deviceCodeGrant {
				t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
			}
			if r.Form.Get("device_code") != d.deviceCode {
				t.Errorf("device_code = %q", r.Form.Get("device_code"))
			}
			if r.Form.Get("client_id") != KimiClientID {
				t.Errorf("client_id = %q", r.Form.Get("client_id"))
			}
			if d.polls.Add(1) <= approveAfter {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"authorization_pending"}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"kimi-access","refresh_token":"kimi-refresh","expires_in":3600,"scope":"openid","token_type":"Bearer"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(d.Server.Close)
	return d
}

func TestDeviceFlowPendingThenApprove(t *testing.T) {
	d := newDeviceFlowServer(t, 2)
	da, err := KimiDeviceAuthorize(context.Background(), nil, d.URL)
	if err != nil {
		t.Fatalf("KimiDeviceAuthorize: %v", err)
	}
	if da.UserCode != "ABCD-EFGH" || da.DeviceCode != "dev-code-123" || da.VerificationURIComplete == "" {
		t.Errorf("device auth = %+v", da)
	}

	toks, err := KimiPollDevice(context.Background(), nil, d.URL, da)
	if err != nil {
		t.Fatalf("KimiPollDevice: %v", err)
	}
	if toks.AccessToken != "kimi-access" || toks.RefreshToken != "kimi-refresh" {
		t.Errorf("tokens = %+v", toks)
	}
	if d.polls.Load() != 3 {
		t.Errorf("polls = %d, want 3 (2 pending + success)", d.polls.Load())
	}
	if exp := toks.ExpiresAtMs(time.Now()); exp < time.Now().UnixMilli() {
		t.Errorf("expiresAtMs in the past: %d", exp)
	}
}

func TestDevicePollExpiredToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"expired_token"}`)
	}))
	defer srv.Close()
	da := &KimiDeviceAuth{DeviceCode: "x", Interval: 1}
	_, err := KimiPollDevice(context.Background(), nil, srv.URL, da)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("err = %v, want expired", err)
	}
}

func TestKimiRefreshRotation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" {
			t.Errorf("form = %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":7200}`)
	}))
	defer srv.Close()

	toks, err := KimiRefresh(context.Background(), nil, srv.URL, "old-refresh")
	if err != nil {
		t.Fatalf("KimiRefresh: %v", err)
	}
	if toks.AccessToken != "rotated-access" || toks.RefreshToken != "rotated-refresh" {
		t.Errorf("tokens = %+v — rotated refresh token must be kept", toks)
	}
}

func TestKimiRefreshDeadGrant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	}))
	defer srv.Close()
	_, err := KimiRefresh(context.Background(), nil, srv.URL, "dead")
	if !errors.Is(err, ErrRefreshDead) {
		t.Errorf("err = %v, want ErrRefreshDead", err)
	}
}

// Kimi refresh through the Manager: rotation persisted, dead grant disables.
func TestParseUsagesShapes(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	cases := []struct {
		name string
		body string
	}{
		{"single object", fmt.Sprintf(`{"window":"5h","limit":100,"used":42,"reset_at":"%s"}`, reset.Format(time.RFC3339))},
		{"array", fmt.Sprintf(`[{"name":"5h","limit":100,"used":42,"reset_at":%d}]`, reset.Unix())},
		{"wrapped", fmt.Sprintf(`{"windows":[{"period":"weekly","total":1000,"spent":250,"resets_at":%d000}]}`, reset.Unix())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usages, err := parseUsages([]byte(tc.body))
			if err != nil {
				t.Fatalf("parseUsages: %v", err)
			}
			if len(usages) != 1 {
				t.Fatalf("usages = %v", usages)
			}
			u := usages[0]
			if u.Limit == 0 || u.Used == 0 {
				t.Errorf("limit/used not parsed: %+v", u)
			}
			if u.ResetAt.IsZero() {
				t.Errorf("reset not parsed: %+v", u)
			} else if u.ResetAt.Sub(reset) > time.Second || reset.Sub(u.ResetAt) > time.Second {
				t.Errorf("reset = %v, want ~%v", u.ResetAt, reset)
			}
		})
	}
}

func TestFormEncoding(t *testing.T) {
	v := url.Values{"client_id": {KimiClientID}}
	if !strings.Contains(v.Encode(), "client_id="+KimiClientID) {
		t.Errorf("encode = %s", v.Encode())
	}
}

// Real /usages shape captured live 2026-08-21: string values, resetTime
// RFC3339Nano, limits[] with {window:{duration,timeUnit}, detail}.
func TestParseUsagesRealShape(t *testing.T) {
	body := `{
	  "usage": {"limit":"100","used":"42","remaining":"58","resetTime":"2026-08-23T04:45:32.121871Z"},
	  "limits": [{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},
	              "detail":{"limit":"100","used":"65","remaining":"35","resetTime":"2026-08-21T23:00:00Z"}}],
	  "parallel": {"limit":"30"}
	}`
	usages, err := parseUsages([]byte(body))
	if err != nil {
		t.Fatalf("parseUsages: %v", err)
	}
	byName := map[string]KimiUsage{}
	for _, u := range usages {
		byName[u.Name] = u
	}
	weekly, ok := byName["7d"]
	if !ok || weekly.Limit != 100 || weekly.Used != 42 {
		t.Errorf("7d = %+v (parsed %v)", weekly, usages)
	}
	if weekly.ResetAt.IsZero() {
		t.Error("7d resetTime not parsed")
	} else if weekly.ResetAt.UTC().Format("2006-01-02 15:04") != "2026-08-23 04:45" {
		t.Errorf("7d reset = %v", weekly.ResetAt)
	}
	fiveH, ok := byName["5h"]
	if !ok || fiveH.Limit != 100 || fiveH.Used != 65 || fiveH.ResetAt.IsZero() {
		t.Errorf("5h = %+v", fiveH)
	}
	par, ok := byName["parallel"]
	if !ok || par.Limit != 30 {
		t.Errorf("parallel = %+v", par)
	}
}

func TestKimiWindowName(t *testing.T) {
	cases := []struct {
		dur  float64
		unit string
		want string
	}{
		{300, "TIME_UNIT_MINUTE", "5h"},
		{30, "TIME_UNIT_MINUTE", "30m"},
		{2, "TIME_UNIT_HOUR", "2h"},
		{7, "TIME_UNIT_DAY", "7d"},
	}
	for _, tc := range cases {
		w := map[string]any{"duration": tc.dur, "timeUnit": tc.unit}
		if got := kimiWindowName(w); got != tc.want {
			t.Errorf("(%v, %s) = %q, want %q", tc.dur, tc.unit, got, tc.want)
		}
	}
}

// One vocabulary for window names across providers.
//
// Claude's are 5h / 7d / 7d-fable, from its response headers. Kimi's come
// from a different shape entirely — an unnamed top-level object for the week,
// and a {duration, timeUnit} pair for the rest — so its names are constructed
// here, and were constructed differently: "weekly" for the week and "1w" had
// a week ever arrived through the other path. Nothing downstream knew they
// meant the same thing as 7d, so a pool holding both providers listed two
// separate week columns and could not compare the two accounts' headroom.
func TestKimiWindowNamesUseTheSharedVocabulary(t *testing.T) {
	for _, tc := range []struct {
		dur   float64
		unit  string
		want  string
		about string
	}{
		{300, "TIME_UNIT_MINUTE", "5h", "Kimi's own 5h window, as minutes"},
		{90, "TIME_UNIT_MINUTE", "90m", "minutes that are not whole hours stay minutes"},
		{5, "TIME_UNIT_HOUR", "5h", "hours"},
		{7, "TIME_UNIT_DAY", "7d", "days"},
		{1, "TIME_UNIT_WEEK", "7d", "a week is 7d, matching Claude"},
		{2, "TIME_UNIT_WEEK", "14d", "two weeks is 14d, not 2w"},
	} {
		got := kimiWindowName(map[string]any{"duration": tc.dur, "timeUnit": tc.unit})
		if got != tc.want {
			t.Errorf("%s: %g %s -> %q, want %q", tc.about, tc.dur, tc.unit, got, tc.want)
		}
	}

	// And the unnamed top-level usage object, which is the one that was
	// called "weekly".
	got, err := parseUsages([]byte(`{"usage":{"limit":"100","used":"52","resetTime":"2026-08-23T04:45:32Z"},"limits":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "7d" {
		t.Errorf("top-level usage parsed as %+v, want one window named 7d", got)
	}
}
