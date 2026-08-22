package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/accounts"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/secrets"
)

func TestRewriteModel(t *testing.T) {
	mm := map[string]string{"claude-sonnet-4-6": "k3[1m]", "claude-haiku-4-5": "kimi-for-coding"}
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"model: claude-sonnet-4-6 in text"}]}`)
	out, err := rewriteModel(body, mm)
	if err != nil {
		t.Fatalf("rewriteModel: %v", err)
	}
	if !strings.Contains(string(out), `"model":"k3[1m]"`) {
		t.Errorf("model not rewritten:\n%s", out)
	}
	// Everything else byte-identical, including the model id in user text.
	if !strings.Contains(string(out), `"content":"model: claude-sonnet-4-6 in text"`) {
		t.Errorf("user content mutated:\n%s", out)
	}

	// Same model → same bytes.
	out2, err := rewriteModel([]byte(`{"model":"k3[1m]"}`), map[string]string{"k3[1m]": "k3[1m]"})
	if err != nil || string(out2) != `{"model":"k3[1m]"}` {
		t.Errorf("identity rewrite = %s, %v", out2, err)
	}
}

func TestRewriteModelUnmappedHardError(t *testing.T) {
	_, err := rewriteModel([]byte(`{"model":"claude-opus-9","messages":[]}`), map[string]string{"claude-sonnet-4-6": "k3[1m]"})
	var unmapped *errUnmappedModel
	if err == nil || !asUnmapped(err, &unmapped) {
		t.Errorf("err = %v, want errUnmappedModel", err)
	}
}

func asUnmapped(err error, target **errUnmappedModel) bool {
	for err != nil {
		if u, ok := err.(*errUnmappedModel); ok {
			*target = u
			return true
		}
		type unwrapper interface{ Unwrap() error }
		uw, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = uw.Unwrap()
	}
	return false
}

func TestClassifyErrorKimi(t *testing.T) {
	kimi := &pool.Account{Type: "kimi-oauth"}
	claude := &pool.Account{Type: "claude-oauth"}

	mkResp := func(status int) *http.Response {
		return &http.Response{StatusCode: status, Header: http.Header{}}
	}
	cases := []struct {
		name   string
		acct   *pool.Account
		status int
		body   string
		want   errKind
	}{
		{"kimi 429 usage limit", kimi, 429, `{"error":"usage limit reached"}`, errQuota},
		{"kimi 429 concurrency", kimi, 429, `{"error":"too many requests"}`, errRate},
		{"kimi 403 quota", kimi, 403, `{"error":"quota exceeded, upgrade billing"}`, errQuota},
		{"kimi 403 other", kimi, 403, `{"error":"forbidden region"}`, errNone},
		{"claude 429 no rejection headers", claude, 429, `{}`, errRate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyError(tc.acct, mkResp(tc.status), []byte(tc.body)); got != tc.want {
				t.Errorf("classify = %v, want %v", got, tc.want)
			}
		})
	}

	// Anthropic header logic must NOT run on kimi responses: a kimi 429 with
	// "rejected"-looking body text but no usage-limit phrase is a throttle.
	hdr := http.Header{}
	hdr.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
	if got := classifyError(claude, &http.Response{StatusCode: 429, Header: hdr}, nil); got != errQuota {
		t.Errorf("claude rejected headers = %v, want errQuota", got)
	}
	if got := classifyError(kimi, &http.Response{StatusCode: 429, Header: hdr}, []byte(`{"error":"rate limited"}`)); got != errRate {
		t.Errorf("kimi with anthropic-looking headers = %v, want errRate", got)
	}
}

// kimi 429 "usage limit" rotates; plain 429 backoffs the same account.
func TestKimi429UsageLimitRotates(t *testing.T) {
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"usage limit reached for this window"}}`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		},
	})
	// BOTH accounts are kimi: this test is about classifying a usage-limit
	// 429 and rotating, not about crossing providers — which is off by
	// default (§6.18) and would otherwise silently decide the outcome.
	rig.pool.Accounts()[0].Type = "kimi-oauth"
	rig.pool.Accounts()[1].Type = "kimi-oauth"

	resp := postMessages(t, rig.front.URL, testBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via rotation", resp.StatusCode)
	}
	if rig.hits[0].Load() != 1 || rig.hits[1].Load() != 1 {
		t.Errorf("hits = (%d, %d), want (1, 1)", rig.hits[0].Load(), rig.hits[1].Load())
	}
	if rig.pool.Accounts()[0].State() != pool.StateExhausted {
		t.Errorf("kimi account state = %v, want exhausted", rig.pool.Accounts()[0].State())
	}
}

func TestKimi429ConcurrencyStaysSameAccount(t *testing.T) {
	var calls atomic.Int32
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, `{"error":"concurrency limit"}`)
				return
			}
			fmt.Fprint(w, `{"ok":true}`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("rotated on a kimi concurrency 429")
		},
	})
	rig.pool.Accounts()[0].Type = "kimi-oauth"

	resp := postMessages(t, rig.front.URL, testBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if calls.Load() != 2 || rig.hits[1].Load() != 0 {
		t.Errorf("calls = %d, B hits = %d; want same-account retry", calls.Load(), rig.hits[1].Load())
	}
}

// modelMap rewrite end-to-end through the proxy; unmapped → hard 502.
func TestModelMapThroughProxy(t *testing.T) {
	got := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	acct := pool.NewAccount("kimi", pool.SourceYAML, "kimi-tok", "", 0, upstream.URL)
	acct.Type = "kimi-oauth"
	acct.ModelMap = map[string]string{"claude-sonnet-4-6": "k3[1m]"}
	p := pool.New([]*pool.Account{acct}, time.Now())
	cfg := config.Defaults()
	cfg.Pool.ExhaustedMode = "fail"
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	body := `{"model":"claude-sonnet-4-6","metadata":{"user_id":"s"},"messages":[]}`
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	sent := <-got
	if !strings.Contains(sent, `"model":"k3[1m]"`) {
		t.Errorf("upstream body = %s", sent)
	}

	// Unmapped model: hard error, upstream never hit.
	//
	// Not a claude-* id any more: the provider now ships defaults covering
	// those families, so they are mapped rather than rejected. An id from
	// another vendor is what "unmapped" means now, and it must still stop
	// here rather than reach Kimi as-is.
	resp, err = http.Post(front.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	errBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for unmapped model; body %s", resp.StatusCode, errBody)
	}
	if !strings.Contains(string(errBody), "modelMap") {
		t.Errorf("error body should name the cause: %s", errBody)
	}
	select {
	case s := <-got:
		t.Errorf("unmapped request reached upstream: %s", s)
	default:
	}
}

// A static-key account serves requests end-to-end with zero refresh calls.
func TestStaticKeyServesWithoutRefresh(t *testing.T) {
	var refreshCalls atomic.Int32
	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer refreshSrv.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "Bearer sk-static" {
			t.Errorf("Authorization = %q", a)
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	acct := pool.NewAccount("static-kimi", pool.SourceYAML, "sk-static", "", 0, upstream.URL)
	acct.Type = "kimi-oauth"
	p := pool.New([]*pool.Account{acct}, time.Now())

	// Real Manager with a tripwire refresh endpoint.
	m := accounts.NewManager("", nil, secrets.NewFake(), testLogger())
	m.Refresher.TokenURL = refreshSrv.URL
	m.KimiAuthBase = refreshSrv.URL
	p.SetTokenManager(m)

	cfg := config.Defaults()
	cfg.Pool.ExhaustedMode = "fail"
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if refreshCalls.Load() != 0 {
		t.Errorf("refresh endpoint called %d times for a static key", refreshCalls.Load())
	}
	if acct.State() != pool.StateOK {
		t.Errorf("state = %v, want OK", acct.State())
	}
}

// Kimi's overloaded 401 for unknown model ids must pass through untouched —
// never Recover, never disable a healthy account. Exact body captured live
// 2026-08-21.
func TestKimi401ModelErrorPassesThrough(t *testing.T) {
	const modelErrBody = `{"error":{"type":"authentication_error","message":"Your model id does not exist, recognized as other:k3[1m]. Please set model id as ` + "`k3`." + `"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, modelErrBody)
	}))
	defer upstream.Close()

	acct := pool.NewAccount("kimi", pool.SourceYAML, "healthy-key", "", 0, upstream.URL)
	acct.Type = "kimi-oauth"
	p := pool.New([]*pool.Account{acct}, time.Now())
	fm := &fakeManager{recover: func(a *pool.Account) error {
		t.Error("Recover called for a model-id 401 — healthy account would be disabled")
		return nil
	}}
	p.SetTokenManager(fm)

	cfg := config.Defaults()
	cfg.Pool.ExhaustedMode = "fail"
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 passed through", resp.StatusCode)
	}
	if string(body) != modelErrBody {
		t.Errorf("body mutated:\n%s", body)
	}
	if fm.recoverCalls.Load() != 0 {
		t.Errorf("recover calls = %d, want 0", fm.recoverCalls.Load())
	}
	if acct.State() != pool.StateOK {
		t.Errorf("account state = %v, want OK (healthy, not disabled)", acct.State())
	}
}

// A genuine kimi auth 401 (not a model error) still goes down the Recover
// path.
func TestKimi401AuthErrorRecovers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"type":"authentication_error","message":"Invalid bearer token"}}`)
	}))
	defer upstream.Close()

	acct := pool.NewAccount("kimi", pool.SourceYAML, "dead-key", "", 0, upstream.URL)
	acct.Type = "kimi-oauth"
	p := pool.New([]*pool.Account{acct}, time.Now())
	fm := &fakeManager{recover: func(a *pool.Account) error {
		a.SetCredentials("recovered-key", "", 0)
		return nil
	}}
	p.SetTokenManager(fm)

	cfg := config.Defaults()
	cfg.Pool.ExhaustedMode = "fail"
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessages(t, front.URL, testBody)
	defer resp.Body.Close()
	if fm.recoverCalls.Load() != 1 {
		t.Errorf("recover calls = %d, want 1 for a genuine auth 401", fm.recoverCalls.Load())
	}
}

// Rotation must not cross providers by default: the client fixed its
// capabilities from the first model it saw (§6.18).
func TestQuotaRotationStaysWithinProviderByDefault(t *testing.T) {
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"usage limit reached for this window"}}`)
		},
		func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true}`) },
	})
	rig.pool.Accounts()[0].Type = "kimi-oauth"   // session starts here
	rig.pool.Accounts()[1].Type = "claude-oauth" // different family

	started := time.Now()
	resp := postMessages(t, rig.front.URL, testBody)
	defer resp.Body.Close()

	if rig.hits[1].Load() != 0 {
		t.Errorf("rotated across providers with CrossProvider off (%d hits)", rig.hits[1].Load())
	}
	if resp.StatusCode == http.StatusOK {
		t.Error("expected the request to fail rather than silently change provider")
	}
	// Fast: the pin is what stopped it, not a rate-limit backoff. An
	// anthropic-shaped fixture on a kimi account is misclassified and burns
	// 3x5s here, which would make this pass for the wrong reason.
	if el := time.Since(started); el > 5*time.Second {
		t.Errorf("took %v — that is backoff, not the provider pin", el)
	}
}

// With the opt-in set, the same rotation is allowed.
func TestQuotaRotationCrossesProviderWhenEnabled(t *testing.T) {
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"usage limit reached for this window"}}`)
		},
		func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true}`) },
	})
	rig.pool.Accounts()[0].Type = "kimi-oauth"
	rig.pool.Accounts()[1].Type = "claude-oauth"
	rig.pool.CrossProvider = true

	resp := postMessages(t, rig.front.URL, testBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via cross-provider rotation", resp.StatusCode)
	}
	if rig.hits[1].Load() != 1 {
		t.Errorf("second provider hits = %d, want 1", rig.hits[1].Load())
	}
}
