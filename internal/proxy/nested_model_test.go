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
