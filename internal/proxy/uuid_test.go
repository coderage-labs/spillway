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

const (
	uuidA = "11111111-2222-3333-4444-555555555555"
	uuidB = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// bodyWithUserID builds a /v1/messages body whose metadata.user_id is a
// stringified JSON blob carrying account_uuid — the shape Claude Code sends.
func bodyWithUserID(uuid string) string {
	inner := fmt.Sprintf(`{\"user_uuid\":\"u-1\",\"account_uuid\":\"%s\",\"session_id\":\"s-1\"}`, uuid)
	return fmt.Sprintf(`{"model":"claude","max_tokens":16,"metadata":{"user_id":"%s"},"messages":[{"role":"user","content":"account_uuid\":\"%s\" in user content must survive"}]}`,
		inner, uuid)
}

func TestPatchAccountUUID(t *testing.T) {
	body := []byte(bodyWithUserID(uuidA))
	out := patchAccountUUID(body, uuidB)

	// Byte-exact: the 36-byte uuid inside metadata.user_id changes, and
	// every other byte — including the identical-looking uuid in user
	// message content — is untouched (issue #128).
	want := strings.Replace(string(body), `account_uuid\":\"`+uuidA, `account_uuid\":\"`+uuidB, 1)
	if string(out) != want {
		t.Errorf("only the metadata account_uuid may change:\n got  %s\n want %s", out, want)
	}
	// Idempotent re-patch works (failover path patches per attempt).
	out2 := patchAccountUUID(out, uuidA)
	if !strings.Contains(string(out2), `account_uuid\":\"`+uuidA) {
		t.Errorf("re-patch failed:\n%s", out2)
	}
}

func TestPatchAccountUUIDPassthrough(t *testing.T) {
	cases := []struct {
		name string
		body string
		uuid string
	}{
		{"no metadata", `{"model":"claude","messages":[]}`, uuidB},
		{"user_id without account_uuid", `{"metadata":{"user_id":"plain-string"},"messages":[]}`, uuidB},
		{"malformed json", `{"metadata":{"user_id":"{"account_uuid\":\"` + uuidA + `\"`, uuidB},
		{"non-36-char new uuid", bodyWithUserID(uuidA), "short"},
		{"empty new uuid", bodyWithUserID(uuidA), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := patchAccountUUID([]byte(tc.body), tc.uuid)
			if string(out) != tc.body {
				t.Errorf("body mutated:\n%s", out)
			}
		})
	}
}

// End-to-end through the handler: a serving account with AccountUUID rewrites
// the buffered body; one without leaves it untouched; non-/v1/messages paths
// are never rewritten.
func TestUUIDRewriteThroughProxy(t *testing.T) {
	got := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	acct := pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, upstream.URL)
	acct.AccountUUID = uuidB
	p := pool.New([]*pool.Account{acct}, time.Now())
	cfg := config.Defaults()
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	body := bodyWithUserID(uuidA)
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want := strings.Replace(body, `account_uuid\":\"`+uuidA, `account_uuid\":\"`+uuidB, 1)
	if s := <-got; s != want {
		t.Errorf("upstream body is not byte-faithful outside the metadata account_uuid:\n got  %s\n want %s", s, want)
	}

	// Account without a UUID: untouched.
	acct.AccountUUID = ""
	resp, err = http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if s := <-got; s != body {
		t.Errorf("body mutated for uuid-less account:\n%s", s)
	}
}

func TestNonMessagesPathNeverRewritten(t *testing.T) {
	got := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	acct := pool.NewAccount("a", pool.SourceYAML, "tok", "", 0, upstream.URL)
	acct.AccountUUID = uuidB
	p := pool.New([]*pool.Account{acct}, time.Now())
	cfg := config.Defaults()
	h, err := NewHandler(&cfg, testLogger(), p)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	body := bodyWithUserID(uuidA)
	resp, err := http.Post(front.URL+"/v1/files", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if s := <-got; s != body {
		t.Errorf("non-messages body mutated:\n%s", s)
	}
}
