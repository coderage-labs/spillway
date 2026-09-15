package proxy

// Issue #176: the routing default is inverted — pool only what is
// recognised as inference, pass everything else through on the client's own
// credential.
//
// Four things have to be true and each is proved here rather than argued:
//
//   - POST /v1/messages still pools, rotates and holds EXACTLY as before.
//     This is the one thing that must never break, so it is tested first
//     and on all three axes.
//   - Every path family measured in issue #176's table no longer receives a
//     pooled credential. Table-driven over all eleven.
//   - The warning fires once for an unseen inference-shaped path and never
//     for a known one — the mitigation that makes the inversion safe.
//   - An identity path still works.

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/reqlog"
)

// poolToken is what a pooled account injects. A test asserts on this exact
// string, so an injected credential is unmistakable in a failure message.
const poolToken = "pooled-account-token"

// clientToken is what the CLIENT sends. An unpooled or identity-bound path
// must reach the upstream carrying this and nothing else.
const clientToken = "the-clients-own-token"

// seenAuth records the Authorization header each upstream request arrived
// with, keyed by path. Concurrent-safe: httptest serves each request on its
// own goroutine.
type seenAuth struct {
	mu sync.Mutex
	by map[string]string
	n  int
}

func (s *seenAuth) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.by == nil {
		s.by = map[string]string{}
	}
	s.by[r.URL.Path] = r.Header.Get("Authorization")
	s.n++
}

func (s *seenAuth) get(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.by[path]
}

// inversionFixture stands up an upstream that records Authorization, and a
// proxy in front of it whose single pooled account holds poolToken.
func inversionFixture(t *testing.T) (front *httptest.Server, h *Handler, seen *seenAuth) {
	t.Helper()
	seen = &seenAuth{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(up.Close)

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "fail"
	p := pool.New([]*pool.Account{
		pool.NewAccount("pooled", pool.SourceYAML, poolToken, "", 0, ""),
	}, time.Now())
	var err error
	h, err = NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front = httptest.NewServer(h)
	t.Cleanup(front.Close)
	return front, h, seen
}

// doPost sends a POST carrying the client's own credential and a body of
// the given size.
func doPost(t *testing.T, front *httptest.Server, path string, bodySize int) *http.Response {
	t.Helper()
	body := bytes.Repeat([]byte("x"), bodySize)
	req, err := http.NewRequest(http.MethodPost, front.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+clientToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func doGet(t *testing.T, front *httptest.Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, front.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+clientToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// ---------------------------------------------------------------------
// 1. The thing that must never break.
// ---------------------------------------------------------------------

// TestInferenceStillPooled is the load-bearing assertion of this whole
// change: POST /v1/messages still reaches the upstream carrying the POOLED
// account's credential, not the client's. If the inversion is written even
// slightly too wide, this is what goes red.
func TestInferenceStillPooled(t *testing.T) {
	front, _, seen := inversionFixture(t)

	resp := doPost(t, front, "/v1/messages", 2048)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got, want := seen.get("/v1/messages"), "Bearer "+poolToken; got != want {
		t.Errorf("Authorization at the upstream = %q, want %q — POST /v1/messages must still be POOLED", got, want)
	}
}

// TestInferenceStillLogsAgainstThePooledAccount: the request log must still
// attribute inference to the real account that served it. An inversion that
// passed /v1/messages through would leave every completion labelled
// "(unpooled)" and every per-account total at zero.
func TestInferenceStillLogsAgainstThePooledAccount(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()
	rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "fail"
	p := pool.New([]*pool.Account{pool.NewAccount("pooled", pool.SourceYAML, poolToken, "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	h.SetHooks(Hooks{Log: rl})
	front := httptest.NewServer(h)
	defer front.Close()

	doPost(t, front, "/v1/messages", 2048)

	e := waitForEntry(t, rl)
	if e.Account != "pooled" {
		t.Errorf("account = %q, want %q — inference must still be attributed to the account that served it", e.Account, "pooled")
	}
	if e.Event != reqlog.EventServed {
		t.Errorf("event = %q, want %q", e.Event, reqlog.EventServed)
	}
}

// TestInferenceStillRotatesOnQuota429: the first account 429s with a quota
// rejection, the request must rotate to the second and be served on ITS
// credential. Rotation only exists on the pooled path, so this proves the
// inversion did not quietly divert inference off it.
func TestInferenceStillRotatesOnQuota429(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tokens = append(tokens, tok)
		mu.Unlock()
		if tok == "tok-a" {
			// A genuine quota rejection, not a transient throttle: the
			// unified-window headers are what make the provider classify
			// this as errQuota and rotate rather than retry in place.
			quota429(w)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "fail"
	a := pool.NewAccount("a", pool.SourceYAML, "tok-a", "", 0, "")
	b := pool.NewAccount("b", pool.SourceYAML, "tok-b", "", 0, "")
	p := pool.New([]*pool.Account{a, b}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	resp := doPost(t, front, "/v1/messages", 1024)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after rotating off the quota-rejected account", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) < 2 {
		t.Fatalf("upstream saw %v — want two attempts, i.e. a rotation", tokens)
	}
	if tokens[len(tokens)-1] != "tok-b" {
		t.Errorf("served on %q, want %q — inference must still rotate across the pool", tokens[len(tokens)-1], "tok-b")
	}
}

// TestInferenceStillHoldsOnExhaustion: with every account exhausted and
// exhaustedMode=hold, POST /v1/messages must PARK rather than fail fast.
// Issue #91 deliberately restricted holding to this one path; the inversion
// removed that guard because only inference can reach it any more, so this
// checks the behaviour survived the guard's removal.
//
// Proved by the wait AND the outcome: the only account is exhausted with a
// reset 400ms out and a generous hold budget, so a request that holds sleeps
// until the reset and is then served 200 by that account, while one that
// fell through to the fail-fast branch returns a 429 immediately. Both
// halves are asserted, because either alone is weak — a 200 could come from
// a pass-through and a delay could come from anything.
func TestInferenceStillHoldsOnExhaustion(t *testing.T) {
	seen := &seenAuth{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "hold"
	cfg.Pool.HoldMax = "10s"
	acct := pool.NewAccount("a", pool.SourceYAML, poolToken, "", 0, "")
	p := pool.New([]*pool.Account{acct}, time.Now())
	p.MarkExhausted(acct, time.Now().Add(400*time.Millisecond))
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	start := time.Now()
	resp := doPost(t, front, "/v1/messages", 1024)
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the request should have parked until the reset and then been served", resp.StatusCode)
	}
	if elapsed < 300*time.Millisecond {
		t.Errorf("returned after %v, want a park of about the 400ms until reset — POST /v1/messages must still hold on exhaustion", elapsed)
	}
	if got, want := seen.get("/v1/messages"), "Bearer "+poolToken; got != want {
		t.Errorf("Authorization = %q, want %q — served on the pooled account after the hold", got, want)
	}
}

// ---------------------------------------------------------------------
// 2. Every measured path family stops getting a pooled credential.
// ---------------------------------------------------------------------

// TestMeasuredNonInferencePathsAreNoLongerPooled walks issue #176's table.
// Each of these was observed in live traffic reaching the upstream with a
// pooled account's credential attached; after the inversion each must reach
// it with the CLIENT's own credential and nothing spillway issued.
//
// /api/oauth/validate is the sharpest and is why this is a table rather
// than a spot check: a token-validation call answered with a pooled
// account's token is a wrong answer, not just a misattribution.
func TestMeasuredNonInferencePathsAreNoLongerPooled(t *testing.T) {
	for _, path := range []string{
		"/latest/api/token",
		"/latest/meta-data/placement/region",
		"/api/claude_code/metrics",
		"/api/claude_code/notification/preferences",
		"/api/eval/something",
		"/api/frame/deploy/direct",
		"/api/web/domain_info",
		"/v1/sessions",
		"/mcp-registry/v0/servers",
		"/api/oauth/validate",
		"/api/claude_cli/bootstrap",
		"/",
	} {
		t.Run(path, func(t *testing.T) {
			front, _, seen := inversionFixture(t)
			doPost(t, front, path, 1024)

			got := seen.get(path)
			if got == "Bearer "+poolToken {
				t.Fatalf("%s reached the upstream with the POOLED account's credential — this is exactly issue #176", path)
			}
			if got != "Bearer "+clientToken {
				t.Errorf("Authorization = %q, want the client's own %q", got, "Bearer "+clientToken)
			}
		})
	}
}

// TestCountTokensIsNotPooled pins judgement call one. Anthropic documents
// token counting as free and rate-limited independently of message
// creation, so it spends none of the quota spillway pools and pooling it
// would buy nothing. It passes through on the client's own credential.
func TestCountTokensIsNotPooled(t *testing.T) {
	front, _, seen := inversionFixture(t)
	doPost(t, front, "/v1/messages/count_tokens", 2048)
	if got := seen.get("/v1/messages/count_tokens"); got != "Bearer "+clientToken {
		t.Errorf("Authorization = %q, want the client's own %q — count_tokens consumes no pooled quota", got, "Bearer "+clientToken)
	}
}

// TestGetOnInferencePathIsNotPooled: the pooled set is POST /v1/messages,
// method included. A GET to the same path is not a completion and must not
// consume a pooled credential.
func TestGetOnInferencePathIsNotPooled(t *testing.T) {
	front, _, seen := inversionFixture(t)
	doGet(t, front, "/v1/messages")
	if got := seen.get("/v1/messages"); got != "Bearer "+clientToken {
		t.Errorf("Authorization = %q, want the client's own %q", got, "Bearer "+clientToken)
	}
}

// ---------------------------------------------------------------------
// 3. Identity paths still work.
// ---------------------------------------------------------------------

// TestIdentityPathStillPassesThrough: /v1/sessions and the trees around it
// must still relay the client's own credential and still be labelled
// "(passthrough)" rather than swept into the new "(unpooled)" default.
// Their label is the claim that they belong to the client's login, which is
// a stronger statement than "not recognised".
func TestIdentityPathStillPassesThrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+clientToken {
			t.Errorf("identity path reached the upstream with %q, want the client's own credential", got)
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()
	rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "fail"
	p := pool.New([]*pool.Account{pool.NewAccount("pooled", pool.SourceYAML, poolToken, "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	h.SetHooks(Hooks{Log: rl})
	front := httptest.NewServer(h)
	defer front.Close()

	resp := doPost(t, front, "/v1/sessions", 64)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	e := waitForEntry(t, rl)
	if e.Account != "(passthrough)" {
		t.Errorf("account = %q, want %q — an identity path must stay tellable apart from an unrecognised one", e.Account, "(passthrough)")
	}
	if u := h.UnpooledStats(); u.Requests != 0 {
		t.Errorf("unpooled requests = %d, want 0 — identity paths are RECOGNISED, just not pooled", u.Requests)
	}
}

// TestNonQuotaLabelSurvivesTheInversion is judgement call three, pinned.
// isNonQuotaPath no longer decides routing — the default already passes
// these through — but it still decides what the request log claims, and
// these three must stay "(non-quota)" rather than collapsing into
// "(unpooled)" and being counted as paths spillway does not recognise.
func TestNonQuotaLabelSurvivesTheInversion(t *testing.T) {
	for _, path := range []string{
		"/api/event_logging/v2/batch",
		"/api/claude_code/settings",
		"/api/claude_code/policy_limits",
	} {
		t.Run(path, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"ok":true}`)
			}))
			defer up.Close()
			rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer rl.Close()

			cfg := config.Defaults()
			cfg.Upstream = up.URL
			cfg.Pool.ExhaustedMode = "fail"
			p := pool.New([]*pool.Account{pool.NewAccount("pooled", pool.SourceYAML, poolToken, "", 0, "")}, time.Now())
			h, err := NewHandler(&cfg, testLogger(), p)
			if err != nil {
				t.Fatal(err)
			}
			h.SetHooks(Hooks{Log: rl})
			front := httptest.NewServer(h)
			defer front.Close()

			doPost(t, front, path, 4096)

			e := waitForEntry(t, rl)
			if e.Account != "(non-quota)" {
				t.Errorf("account = %q, want %q", e.Account, "(non-quota)")
			}
			if u := h.UnpooledStats(); u.Requests != 0 {
				t.Errorf("unpooled requests = %d, want 0 — a confirmed non-quota path is recognised, not unrecognised", u.Requests)
			}
		})
	}
}

// ---------------------------------------------------------------------
// 4. The warning: the mitigation that makes the inversion safe.
// ---------------------------------------------------------------------

// warnRecorder captures WARN-level log records so a test can count them.
type warnRecorder struct {
	mu   sync.Mutex
	msgs []string
	buf  bytes.Buffer
}

func (w *warnRecorder) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func (w *warnRecorder) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(b)
	w.msgs = append(w.msgs, string(b))
	return len(b), nil
}

// unpooledWarnings returns the lines this recorder captured that are the
// unseen-path warning.
func (w *warnRecorder) unpooledWarnings() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, line := range strings.Split(w.buf.String(), "\n") {
		if strings.Contains(line, "unrecognised path passed through unpooled") {
			out = append(out, line)
		}
	}
	return out
}

// warnFixture is inversionFixture with a logger a test can read back.
func warnFixture(t *testing.T) (*httptest.Server, *Handler, *warnRecorder) {
	t.Helper()
	rec := &warnRecorder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(up.Close)
	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "fail"
	p := pool.New([]*pool.Account{pool.NewAccount("pooled", pool.SourceYAML, poolToken, "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, rec.logger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	return front, h, rec
}

// TestUnseenInferenceShapedPathWarnsExactlyOnce is the counter-risk
// mitigation working: a path spillway has never been told about, taking a
// POST with a substantial body, is the shape of a new inference endpoint,
// and it must announce itself on the FIRST request — not after 7,500.
//
// "Exactly once" is half the requirement. A warning that fires per request
// is a firehose that gets muted, which is the same as no warning at all.
func TestUnseenInferenceShapedPathWarnsExactlyOnce(t *testing.T) {
	front, _, rec := warnFixture(t)
	for i := 0; i < 5; i++ {
		doPost(t, front, "/v1/responses", 4096)
	}
	got := rec.unpooledWarnings()
	if len(got) != 1 {
		t.Fatalf("five requests to /v1/responses produced %d warnings, want exactly 1:\n%s",
			len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[0], "/v1/responses") {
		t.Errorf("warning does not name the path: %s", got[0])
	}
	if !strings.Contains(got[0], "inferencePaths") {
		t.Errorf("warning does not say what to do about it: %s", got[0])
	}
}

// TestKnownNonInferencePathsNeverWarn: the measured families from issue
// #176's table are not news. If they warned, the first day of the inverted
// default would produce a wall of warnings about traffic already known to
// be harmless, and the one line that matters would be lost in it.
//
// /api/claude_code/metrics and /api/event_logging/v2/batch are the pointed
// cases: both are POSTs with real bodies, so only the known-path
// suppression keeps them quiet.
func TestKnownNonInferencePathsNeverWarn(t *testing.T) {
	front, _, rec := warnFixture(t)
	for _, path := range []string{
		"/latest/api/token",
		"/api/claude_code/metrics",
		"/api/claude_code/notification/preferences",
		"/api/event_logging/v2/batch",
		"/api/eval/run",
		"/api/frame/deploy/direct",
		"/api/web/domain_info",
		"/api/oauth/validate",
		"/api/claude_cli/bootstrap",
		"/mcp-registry/v0/servers",
		"/v1/sessions",
		"/",
	} {
		doPost(t, front, path, 8192)
	}
	if got := rec.unpooledWarnings(); len(got) != 0 {
		t.Errorf("known non-inference paths produced %d warnings, want 0:\n%s",
			len(got), strings.Join(got, "\n"))
	}
}

// TestUnpooledWarningKeysOnTheRedactedTemplate: without this, "warn once
// per path" IS "warn once per request" for any /collection/<id> endpoint,
// because every request carries a different id. Ten distinct ids on the
// same unrecognised endpoint must produce one warning naming {id}, and the
// raw ids must appear nowhere in it.
func TestUnpooledWarningKeysOnTheRedactedTemplate(t *testing.T) {
	front, h, rec := warnFixture(t)
	ids := []string{
		"58883633-52d9-4a53-a4b6-36e5aaaaaaaa",
		"11111111-2222-3333-4444-555555555555",
		"deadbeefdeadbeefdeadbeef01234567",
	}
	for _, id := range ids {
		for i := 0; i < 3; i++ {
			doPost(t, front, "/v1/newthing/"+id, 4096)
		}
	}
	got := rec.unpooledWarnings()
	if len(got) != 1 {
		t.Fatalf("nine requests over three ids produced %d warnings, want exactly 1:\n%s",
			len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[0], "/v1/newthing/{id}") {
		t.Errorf("warning should name the redacted template /v1/newthing/{id}: %s", got[0])
	}
	for _, id := range ids {
		if strings.Contains(got[0], id) {
			t.Errorf("warning leaked the identifier %q: %s", id, got[0])
		}
	}
	// And the same redaction governs what is stored and reported.
	u := h.UnpooledStats()
	if u.Paths != 1 {
		t.Errorf("distinct paths = %d, want 1 — three ids on one endpoint are one template", u.Paths)
	}
	if u.Requests != 9 {
		t.Errorf("unpooled requests = %d, want 9", u.Requests)
	}
	if len(u.InferenceShaped) != 1 || u.InferenceShaped[0] != "/v1/newthing/{id}" {
		t.Errorf("inferenceShaped = %v, want [/v1/newthing/{id}]", u.InferenceShaped)
	}
}

// TestUnseenPathWithoutAnInferenceShapedRequestDoesNotWarn: a GET, or a
// POST with a trivial body, is not the shape of inference. It is still
// counted — the user can see it on /api/state — but it must not raise the
// warning, which is reserved for the one thing that would mean the pool has
// stopped doing its job.
func TestUnseenPathWithoutAnInferenceShapedRequestDoesNotWarn(t *testing.T) {
	front, h, rec := warnFixture(t)
	doGet(t, front, "/v1/unknown-get")
	doPost(t, front, "/v1/unknown-tiny", 8)

	if got := rec.unpooledWarnings(); len(got) != 0 {
		t.Errorf("a GET and a tiny POST produced %d warnings, want 0:\n%s", len(got), strings.Join(got, "\n"))
	}
	u := h.UnpooledStats()
	if u.Requests != 2 || u.Paths != 2 {
		t.Errorf("stats = %d requests on %d paths, want 2 on 2 — not warning is not the same as not counting", u.Requests, u.Paths)
	}
	if len(u.InferenceShaped) != 0 {
		t.Errorf("inferenceShaped = %v, want empty", u.InferenceShaped)
	}
}

// TestUnpooledRequestIsLabelledUnpooled: a path spillway does not recognise
// is recorded under its own account label, so the request log tells "chose
// not to claim this" apart from "this is the client's own login" and from
// "confirmed to cost nothing". "(forward)" set the precedent in issue #177.
func TestUnpooledRequestIsLabelledUnpooled(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()
	rl, err := reqlog.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "fail"
	p := pool.New([]*pool.Account{pool.NewAccount("pooled", pool.SourceYAML, poolToken, "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	h.SetHooks(Hooks{Log: rl})
	front := httptest.NewServer(h)
	defer front.Close()

	doPost(t, front, "/v1/responses", 4096)

	e := waitForEntry(t, rl)
	if e.Account != "(unpooled)" {
		t.Errorf("account = %q, want %q", e.Account, "(unpooled)")
	}
	if e.Event != reqlog.EventPassthrough {
		t.Errorf("event = %q, want %q", e.Event, reqlog.EventPassthrough)
	}
}

// ---------------------------------------------------------------------
// 5. The escape hatch: pooling a new endpoint without a release.
// ---------------------------------------------------------------------

// TestConfiguredInferencePathIsPooled: the answer to "Anthropic shipped
// /v1/responses". One config line and it is pooled exactly like
// /v1/messages — credential injected, no warning, not counted as
// unrecognised.
func TestConfiguredInferencePathIsPooled(t *testing.T) {
	seen := &seenAuth{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer up.Close()

	rec := &warnRecorder{}
	cfg := config.Defaults()
	cfg.Upstream = up.URL
	cfg.Pool.ExhaustedMode = "fail"
	cfg.Proxy.InferencePaths = []string{"/v1/responses"}
	p := pool.New([]*pool.Account{pool.NewAccount("pooled", pool.SourceYAML, poolToken, "", 0, "")}, time.Now())
	h, err := NewHandler(&cfg, rec.logger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	doPost(t, front, "/v1/responses", 4096)

	if got, want := seen.get("/v1/responses"), "Bearer "+poolToken; got != want {
		t.Errorf("Authorization = %q, want %q — a configured inference path must be POOLED", got, want)
	}
	if got := rec.unpooledWarnings(); len(got) != 0 {
		t.Errorf("a configured inference path warned %d times, want 0:\n%s", len(got), strings.Join(got, "\n"))
	}
	if u := h.UnpooledStats(); u.Requests != 0 {
		t.Errorf("unpooled requests = %d, want 0", u.Requests)
	}
}

// TestSetInferencePathsAppliesLive: the config watcher replaces the pooled
// set on a running daemon. If this needed a restart, "add a line to pool the
// new endpoint" would mean restarting every proxied CLI's connection.
func TestSetInferencePathsAppliesLive(t *testing.T) {
	front, h, seen := inversionFixture(t)

	doPost(t, front, "/v1/responses", 4096)
	if got := seen.get("/v1/responses"); got != "Bearer "+clientToken {
		t.Fatalf("before the config change, Authorization = %q, want the client's own", got)
	}

	h.SetInferencePaths([]string{"/v1/responses"})

	doPost(t, front, "/v1/responses", 4096)
	if got, want := seen.get("/v1/responses"), "Bearer "+poolToken; got != want {
		t.Errorf("after SetInferencePaths, Authorization = %q, want %q", got, want)
	}
	if got := h.InferencePaths(); len(got) != 1 || got[0] != "/v1/responses" {
		t.Errorf("InferencePaths() = %v, want [/v1/responses]", got)
	}
}

// TestSetInferencePathsRejectsNonPaths: an entry that is not a path is
// dropped rather than half-matched. A typo that silently pools nothing and
// a typo that silently pools everything are both worse than one that does
// nothing.
func TestSetInferencePathsRejectsNonPaths(t *testing.T) {
	_, h, _ := inversionFixture(t)
	h.SetInferencePaths([]string{"", "  ", "v1/responses", "https://api.anthropic.com/v1/x", "/v1/ok"})
	got := h.InferencePaths()
	if len(got) != 1 || got[0] != "/v1/ok" {
		t.Errorf("InferencePaths() = %v, want only [/v1/ok]", got)
	}
}

// ---------------------------------------------------------------------
// 6. Redaction, as a unit.
// ---------------------------------------------------------------------

func TestRedactPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// Real endpoint names must survive: redacting them would make every
		// warning say the same thing.
		{"/v1/messages", "/v1/messages"},
		{"/v1/messages/count_tokens", "/v1/messages/count_tokens"},
		{"/api/claude_code/policy_limits", "/api/claude_code/policy_limits"},
		{"/api/event_logging/v2/batch", "/api/event_logging/v2/batch"},
		{"/mcp-registry/v0/servers", "/mcp-registry/v0/servers"},
		{"/latest/meta-data/instance-identity", "/latest/meta-data/instance-identity"},
		{"/", "/"},
		// Identifiers must not: these are the shapes real spillway paths
		// carry, and each would otherwise be both a leak and a new warning.
		{"/api/frame/read/58883633-52d9-4a53-a4b6-36e5aaaaaaaa", "/api/frame/read/{id}"},
		{"/v1/code/sessions/11111111-2222-3333-4444-555555555555", "/v1/code/sessions/{id}"},
		{"/v1/environments/deadbeefdeadbeefdeadbeef01234567/work/poll", "/v1/environments/{id}/work/poll"},
		{"/api/thing/12345", "/api/thing/{id}"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := redactPath(tc.in); got != tc.want {
				t.Errorf("redactPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactPathBoundsDepth: a pathologically deep path must not become a
// pathologically long map key or log line.
func TestRedactPathBoundsDepth(t *testing.T) {
	deep := "/" + strings.Repeat("seg/", 40)
	got := redactPath(deep)
	if n := strings.Count(got, "/"); n > maxPathSegments+1 {
		t.Errorf("redactPath kept %d separators, want at most %d: %q", n, maxPathSegments+1, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated template should say so: %q", got)
	}
}

// TestUnpooledTrackerBoundsItsMaps: the template map is fed by arbitrary
// request paths and lives for the life of the daemon, so it is bounded
// rather than trusted. Requests past the cap still count; only new
// templates stop being tracked.
func TestUnpooledTrackerBoundsItsMaps(t *testing.T) {
	tr := newUnpooledTracker(slog.New(slog.NewTextHandler(io.Discard, nil)))
	total := maxUnpooledPaths + 50
	for i := 0; i < total; i++ {
		r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/unknown/endpoint-%c%c%c", 'a'+byte(i/676%26), 'a'+byte(i/26%26), 'a'+byte(i%26)), nil)
		r.ContentLength = 4096
		tr.note(r)
	}
	s := tr.stats()
	if s.Paths > maxUnpooledPaths {
		t.Errorf("tracked %d templates, want at most %d", s.Paths, maxUnpooledPaths)
	}
	if s.Requests != total {
		t.Errorf("requests = %d, want %d — hitting the template cap must not stop counting", s.Requests, total)
	}
	if len(s.InferenceShaped) > maxUnpooledPaths {
		t.Errorf("warned about %d templates, want at most %d", len(s.InferenceShaped), maxUnpooledPaths)
	}
}
