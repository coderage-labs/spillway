package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/pool"
)

// issue #29 / §4 mutation #4: an advisor's model, nested inside tools[],
// must be rewritten by the same modelMap as the top-level (executor) model
// on cross-provider rotation.
func TestRewriteModelRewritesNestedToolsModel(t *testing.T) {
	mm := map[string]string{"claude-sonnet-4-6": "k3[1m]", "claude-fable-5": "kimi-for-coding"}
	body := []byte(`{"model":"claude-sonnet-4-6","tools":[{"type":"advisor_x","model":"claude-fable-5"}],"messages":[]}`)
	out, err := rewriteModel(body, mm)
	if err != nil {
		t.Fatalf("rewriteModel: %v", err)
	}
	if !strings.Contains(string(out), `"model":"k3[1m]"`) {
		t.Errorf("top-level model not rewritten:\n%s", out)
	}
	if !strings.Contains(string(out), `"model":"kimi-for-coding"`) {
		t.Errorf("nested tools[].model not rewritten:\n%s", out)
	}
	if strings.Contains(string(out), "claude-fable-5") || strings.Contains(string(out), "claude-sonnet-4-6") {
		t.Errorf("original model ids should be gone entirely:\n%s", out)
	}
}

// Multiple tools, each with their own nested model, must each be rewritten
// independently and the tool's other fields must survive untouched.
func TestRewriteModelRewritesMultipleNestedToolsModels(t *testing.T) {
	mm := map[string]string{
		"claude-sonnet-4-6": "k3[1m]",
		"claude-fable-5":    "kimi-for-coding",
		"claude-haiku-4-5":  "kimi-for-coding-highspeed",
	}
	body := []byte(`{"model":"claude-sonnet-4-6","tools":[` +
		`{"type":"advisor_a","model":"claude-fable-5","name":"A"},` +
		`{"type":"bash"},` +
		`{"type":"advisor_b","model":"claude-haiku-4-5","name":"B"}` +
		`],"messages":[]}`)
	out, err := rewriteModel(body, mm)
	if err != nil {
		t.Fatalf("rewriteModel: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"model":"kimi-for-coding","name":"A"`) {
		t.Errorf("tools[0].model not rewritten in place:\n%s", s)
	}
	if !strings.Contains(s, `"model":"kimi-for-coding-highspeed","name":"B"`) {
		t.Errorf("tools[2].model not rewritten in place:\n%s", s)
	}
	if !strings.Contains(s, `{"type":"bash"}`) {
		t.Errorf("tools[1], which has no model, must be untouched:\n%s", s)
	}
}

// §6.12 applies to nested models too: an unmapped nested model is a hard
// error, never a silent passthrough, and the error must name the nested
// model and where it was found so it is diagnosable.
func TestRewriteModelUnmappedNestedModelHardError(t *testing.T) {
	mm := map[string]string{"claude-sonnet-4-6": "k3[1m]"}
	body := []byte(`{"model":"claude-sonnet-4-6","tools":[{"type":"advisor_x","model":"claude-fable-5"}]}`)
	_, err := rewriteModel(body, mm)
	var unmapped *errUnmappedModel
	if err == nil || !asUnmapped(err, &unmapped) {
		t.Fatalf("err = %v, want errUnmappedModel", err)
	}
	if unmapped.model != "claude-fable-5" {
		t.Errorf("unmapped.model = %q, want %q", unmapped.model, "claude-fable-5")
	}
	if !strings.Contains(err.Error(), "claude-fable-5") || !strings.Contains(err.Error(), "tools[0].model") {
		t.Errorf("error must name the nested model and its location: %v", err)
	}
}

// A "model" string that lives inside ordinary message content — arbitrary
// user text, or a same-named key the user's own JSON happens to carry — must
// never be touched. Scope is tools[] only.
func TestRewriteModelLeavesOrdinaryContentModelStringAlone(t *testing.T) {
	mm := map[string]string{"claude-sonnet-4-6": "k3[1m]"}
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"here is some json: {\"model\": \"claude-fable-5\"}"}]}]}`)
	out, err := rewriteModel(body, mm)
	if err != nil {
		t.Fatalf("rewriteModel: %v", err)
	}
	if !strings.Contains(string(out), `claude-fable-5`) {
		t.Errorf("model string inside message content must survive untouched:\n%s", out)
	}
}

// A "model" key nested INSIDE a tool's own schema (e.g. a JSON-schema
// property literally named "model") is not a direct field of the tools[]
// element and must not be mistaken for an advisor model.
func TestRewriteModelLeavesToolSchemaModelPropertyAlone(t *testing.T) {
	mm := map[string]string{"claude-sonnet-4-6": "k3[1m]"}
	body := []byte(`{"model":"claude-sonnet-4-6","tools":[{"name":"pick_model","input_schema":` +
		`{"type":"object","properties":{"model":{"type":"string","enum":["claude-fable-5"]}}}}]}`)
	out, err := rewriteModel(body, mm)
	if err != nil {
		t.Fatalf("rewriteModel: %v", err)
	}
	if !strings.Contains(string(out), `"enum":["claude-fable-5"]`) {
		t.Errorf("a schema property named model must not be rewritten:\n%s", out)
	}
}

// A "model" key nested one level deeper than a tools[] element's own direct
// fields — e.g. inside a per-tool "config" object — is not the advisor
// model shape issue #29 describes and must be left alone. Scope is the
// tool element's DIRECT fields only, not everything under it.
func TestRewriteModelLeavesDeeperNestedModelKeyAlone(t *testing.T) {
	mm := map[string]string{"claude-sonnet-4-6": "k3[1m]", "claude-fable-5": "kimi-for-coding"}
	body := []byte(`{"model":"claude-sonnet-4-6","tools":[{"type":"advisor_x","config":{"model":"claude-fable-5"}}]}`)
	out, err := rewriteModel(body, mm)
	if err != nil {
		t.Fatalf("rewriteModel: %v", err)
	}
	if !strings.Contains(string(out), `"config":{"model":"claude-fable-5"}`) {
		t.Errorf("a model key nested inside a tool's own sub-object must not be rewritten:\n%s", out)
	}
}

// With no tools[] at all, behaviour is identical to before this change:
// only the top-level model is touched, and only when it actually changes.
func TestRewriteModelBodyWithNoToolsIsByteIdentical(t *testing.T) {
	mm := map[string]string{"k3[1m]": "k3[1m]"}
	body := []byte(`{"model":"k3[1m]","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"x"}}`)
	out, err := rewriteModel(body, mm)
	if err != nil {
		t.Fatalf("rewriteModel: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("body changed with identity mapping and no tools[]:\n got  %s\n want %s", out, body)
	}
}

// End-to-end: nested tools[].model rewritten through the real proxy path,
// and an unmapped nested model hard-errors before reaching upstream — same
// shape as TestModelMapThroughProxy, but exercising mutation #4.
func TestNestedToolModelMapThroughProxy(t *testing.T) {
	got := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	acct := pool.NewAccount("kimi", pool.SourceYAML, "kimi-tok", "", 0, upstream.URL)
	acct.Type = "kimi-oauth"
	acct.ModelMap = map[string]string{"claude-sonnet-4-6": "k3[1m]", "claude-fable-5": "kimi-for-coding"}
	p := pool.New([]*pool.Account{acct}, time.Now())
	cfg := config.Defaults()
	cfg.Pool.ExhaustedMode = "fail"
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	body := `{"model":"claude-sonnet-4-6","tools":[{"type":"advisor_x","model":"claude-fable-5"}],"messages":[]}`
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	sent := <-got
	if !strings.Contains(sent, `"model":"k3[1m]"`) || !strings.Contains(sent, `"model":"kimi-for-coding"`) {
		t.Errorf("upstream body missing rewritten models = %s", sent)
	}

	// Unmapped nested model: hard error, upstream never hit.
	body2 := `{"model":"claude-sonnet-4-6","tools":[{"type":"advisor_x","model":"gpt-4o"}],"messages":[]}`
	resp2, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body2))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	errBody, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for unmapped nested model; body %s", resp2.StatusCode, errBody)
	}
	if !strings.Contains(string(errBody), "gpt-4o") || !strings.Contains(string(errBody), "tools[0]") {
		t.Errorf("error body should name the nested model and location: %s", errBody)
	}
	select {
	case s := <-got:
		t.Errorf("unmapped nested request reached upstream: %s", s)
	default:
	}
}

// A duplicate top-level "tools" key makes encoding/json's decode (which
// keeps only the LAST occurrence) disagree with the scanner's byte
// positions (which, before this test's fix, matched every occurrence).
// That disagreement must be a clean error, never a panic and never a
// silent wrong-tool splice. Reproduces the exact body that panicked
// rewriteModel at model.go:133 before the fix.
func TestRewriteModelDuplicateTopLevelToolsKeyRefuses(t *testing.T) {
	mm := map[string]string{"claude-sonnet-4-6": "k3[1m]", "claude-fable-5": "kimi-for-coding"}
	body := []byte(`{"model":"claude-sonnet-4-6","tools":[{"model":"claude-fable-5"},{"model":"claude-fable-5"}],"tools":[{"model":"claude-fable-5"}]}`)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("rewriteModel panicked instead of erroring: %v", r)
		}
	}()
	_, err := rewriteModel(body, mm)
	if err == nil {
		t.Fatal("err = nil, want a hard error for the duplicate top-level \"tools\" key")
	}
	if !strings.Contains(err.Error(), "tools") || !strings.Contains(err.Error(), "ambigu") {
		t.Errorf("error should name the duplicate key and the ambiguity: %v", err)
	}
}

// Same defect class as above, but with BOTH "tools" arrays the same
// length (one element each). A bounds check alone cannot catch this —
// every index the scanner reports is in range of probe.Tools — so this
// specifically exercises the dedicated duplicate-top-level-key guard.
// Without it, this would silently splice the LAST array's (real) model
// mapping into the FIRST array's (discarded) byte span: a wrong rewrite,
// not an error.
func TestRewriteModelDuplicateTopLevelToolsKeySameLengthStillRefuses(t *testing.T) {
	mm := map[string]string{
		"claude-sonnet-4-6": "k3[1m]",
		"claude-haiku-4-5":  "kimi-for-coding-highspeed",
		"claude-fable-5":    "kimi-for-coding",
	}
	body := []byte(`{"model":"claude-sonnet-4-6","tools":[{"model":"claude-haiku-4-5"}],"tools":[{"model":"claude-fable-5"}]}`)
	out, err := rewriteModel(body, mm)
	if err == nil {
		t.Fatalf("err = nil (out = %s), want a hard error — same-length duplicate \"tools\" keys must not silently rewrite", out)
	}
	if !strings.Contains(err.Error(), "tools") || !strings.Contains(err.Error(), "ambigu") {
		t.Errorf("error should name the duplicate key and the ambiguity: %v", err)
	}
}

// Two "model" keys directly on the SAME tools[] element: same toolIndex,
// two byte spans, no panic — but encoding/json's decode (last wins) and a
// naive scanner (which would pair BOTH spans with that last-wins value)
// disagree about which span the decoded value actually belongs to. Refuse
// rather than guess.
func TestRewriteModelDuplicateModelKeyWithinOneToolRefuses(t *testing.T) {
	mm := map[string]string{"claude-sonnet-4-6": "k3[1m]", "claude-fable-5": "kimi-for-coding", "claude-haiku-4-5": "kimi-for-coding-highspeed"}
	body := []byte(`{"model":"claude-sonnet-4-6","tools":[{"model":"claude-fable-5","model":"claude-haiku-4-5"}]}`)
	_, err := rewriteModel(body, mm)
	if err == nil {
		t.Fatal("err = nil, want a hard error for the duplicate \"model\" key within tools[0]")
	}
	if !strings.Contains(err.Error(), "tools[0]") || !strings.Contains(err.Error(), "ambigu") {
		t.Errorf("error should name the tool index and the ambiguity: %v", err)
	}
}

// SAFE case: a "model" key spelled with a JSON escape that decodes to the
// same text ("model") is exactly the shape a scanner that compares raw
// bytes would fail to recognize, while encoding/json decodes it correctly
// — the same disagreement class as the duplicate-key cases above, just via
// escaping instead of repetition. Must refuse rather than silently forward
// the advisor model unmapped (which is the exact defect issue #29 exists
// to close — an escape would otherwise reopen it).
func TestRewriteModelEscapedModelKeyEvasionRefuses(t *testing.T) {
	mm := map[string]string{"claude-sonnet-4-6": "k3[1m]", "claude-fable-5": "kimi-for-coding"}
	// The key is spelled with a JSON unicode escape for its first letter
	// (backslash, u, 0, 0, 6, d, then "odel"), which decodes to "model" —
	// encoding/json reads the field as "model", but the scanner's literal
	// byte comparison sees ten raw characters, not "model".
	escapedKey := "\\u006dodel"
	body := []byte(`{"model":"claude-sonnet-4-6","tools":[{"` + escapedKey + `":"claude-fable-5"}]}`)
	_, err := rewriteModel(body, mm)
	if err == nil {
		t.Fatal("err = nil, want a hard error: the decoder found a nested model the scanner could not locate")
	}
	if !strings.Contains(err.Error(), "claude-fable-5") || !strings.Contains(err.Error(), "tools[0]") {
		t.Errorf("error should name the decoded model and its tool index: %v", err)
	}
}

// SAFE case, proven rather than assumed: a "model" key's value being an
// empty string has no modelMap entry (lookupModel("") never matches), so it
// hard-errors the same way any other unmapped model does — it is not a
// silent no-op and not a special case that needs its own guard.
func TestRewriteModelEmptyNestedModelValueIsHardError(t *testing.T) {
	mm := map[string]string{"claude-sonnet-4-6": "k3[1m]"}
	body := []byte(`{"model":"claude-sonnet-4-6","tools":[{"type":"advisor_x","model":""}]}`)
	_, err := rewriteModel(body, mm)
	var unmapped *errUnmappedModel
	if err == nil || !asUnmapped(err, &unmapped) {
		t.Fatalf("err = %v, want errUnmappedModel for an empty nested model value", err)
	}
}

// SAFE case, proven: "tools" present but not an array (or an array of
// non-objects, or a tool whose "model" is a JSON number rather than a
// string) fails type-checked decoding into probe at the very first
// json.Unmarshal in rewriteModel — already a hard "malformed request body"
// error today, with no special-case handling needed for mutation #4.
func TestRewriteModelToolsNotAnArrayIsMalformedBodyError(t *testing.T) {
	cases := map[string]string{
		"tools is a string":            `{"model":"claude-sonnet-4-6","tools":"nope"}`,
		"tools is an array of strings": `{"model":"claude-sonnet-4-6","tools":["nope"]}`,
		"a tool's model is a number":   `{"model":"claude-sonnet-4-6","tools":[{"model":42}]}`,
	}
	mm := map[string]string{"claude-sonnet-4-6": "k3[1m]"}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := rewriteModel([]byte(body), mm)
			if err == nil || !strings.Contains(err.Error(), "malformed") {
				t.Errorf("err = %v, want a malformed-body error", err)
			}
		})
	}
}

// SAFE case, proven: "tools": null decodes to a nil slice with no error
// (standard encoding/json behaviour), the scanner finds no tools-array
// elements either (the value isn't "["), and the two agree on nothing —
// so this is a normal no-op, byte-identical to the input.
func TestRewriteModelToolsNullIsSafeNoop(t *testing.T) {
	mm := map[string]string{"k3[1m]": "k3[1m]"}
	body := []byte(`{"model":"k3[1m]","tools":null,"messages":[]}`)
	out, err := rewriteModel(body, mm)
	if err != nil {
		t.Fatalf("rewriteModel: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("body changed for tools:null:\n got  %s\n want %s", out, body)
	}
}

// SAFE case, proven: escaped quotes in a tool's OTHER fields, immediately
// adjacent to its "model" field, must not confuse the scanner into
// mis-locating the model value's boundaries.
func TestRewriteModelEscapedQuotesNearModelBoundaryStillRewrites(t *testing.T) {
	mm := map[string]string{"claude-sonnet-4-6": "k3[1m]", "claude-fable-5": "kimi-for-coding"}
	body := []byte(`{"model":"claude-sonnet-4-6","tools":[{"type":"advisor_\"x\"","model":"claude-fable-5","note":"a \"quoted\" note"}]}`)
	out, err := rewriteModel(body, mm)
	if err != nil {
		t.Fatalf("rewriteModel: %v", err)
	}
	if !strings.Contains(string(out), `"model":"kimi-for-coding"`) {
		t.Errorf("nested model not rewritten around escaped neighbours:\n%s", out)
	}
	if !strings.Contains(string(out), `"note":"a \"quoted\" note"`) {
		t.Errorf("escaped neighbour field corrupted:\n%s", out)
	}
}
