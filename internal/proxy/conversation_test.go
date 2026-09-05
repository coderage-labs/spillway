package proxy

// Issue #141. The measurement that opened it: 5,637 /v1/messages requests in
// one day produced 2 distinct session_hash values. A key that collapses like
// that must fail loudly here, not silently produce plausible buckets — so
// these tests assert cardinality on a realistic multi-conversation fixture
// rather than checking one key against one expected string.

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// installUserID builds the metadata.user_id blob Claude Code sends: a
// stringified JSON value, escaped as it appears inside the outer body, with
// the account uuid that uuid.go's rewrite targets and the session id that
// #141 turns on.
func installUserID(deviceID, accountUUID, sessionID string) string {
	return fmt.Sprintf(`{\"device_id\":\"%s\",\"account_uuid\":\"%s\",\"session_id\":\"%s\"}`,
		deviceID, accountUUID, sessionID)
}

// convBody wraps a user_id blob in a body shaped like the real thing —
// system, messages and tools present, because sessionKeys unmarshals the
// whole body and must find metadata past all of it.
func convBody(userID string) []byte { return convTurnBody(userID, 0) }

// convTurnBody is convBody at turn n of a conversation: the message list
// GROWS, the tool order jitters and the system block changes size, exactly
// as a real session's requests do. Nothing about the key may depend on any
// of that — a key that moves as the conversation advances pairs nothing,
// and a fixture that sent identical bytes every turn could not tell.
func convTurnBody(userID string, turn int) []byte {
	msgs := make([]string, 0, turn+1)
	for i := 0; i <= turn; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, fmt.Sprintf(`{"role":%q,"content":[{"type":"text","text":"turn %d %s"}]}`,
			role, i, strings.Repeat("x", i)))
	}
	tools := []string{`{"name":"Bash","description":"run"}`, `{"name":"Read","description":"read"}`}
	if turn%2 == 1 {
		tools[0], tools[1] = tools[1], tools[0]
	}
	return []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","max_tokens":8192,
"system":[{"type":"text","text":"You are Claude Code.%s"}],
"messages":[%s],
"tools":[%s],
"metadata":{"user_id":"%s"},"stream":true}`,
		strings.Repeat(" ", turn), strings.Join(msgs, ","), strings.Join(tools, ","), userID))
}

func convReq(t *testing.T, remoteAddr string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.RemoteAddr = remoteAddr
	return r
}

// keysFor is the pair under test for one body.
func keysFor(t *testing.T, userID string) (routing, conversation string) {
	t.Helper()
	return sessionKeys(convReq(t, "127.0.0.1:51000"), convBody(userID), true)
}

const (
	convDevice  = "device-0001"
	convAccount = "00000000-1111-2222-3333-444444444444"
)

// TestConversationKeySeparatesSessionsFromOneInstall is the headline: N
// conversations from ONE Claude Code install must produce N distinct log
// keys. The fixture holds device_id and account_uuid fixed — that is what
// "one install" means — and varies only session_id, at volume, so a key that
// groups by client collapses to 1 here the way it collapsed to 2 in
// production.
func TestConversationKeySeparatesSessionsFromOneInstall(t *testing.T) {
	const (
		sessions        = 8
		requestsPerSess = 40
	)
	seen := map[string]int{}
	perSession := map[string]map[string]bool{}
	for s := 0; s < sessions; s++ {
		sid := fmt.Sprintf("sess-%04d-%s", s, strings.Repeat("f", 8))
		perSession[sid] = map[string]bool{}
		for i := 0; i < requestsPerSess; i++ {
			// A growing conversation from a client whose ephemeral source
			// port changes per connection — neither may move the key.
			r := convReq(t, fmt.Sprintf("127.0.0.1:%d", 51000+i))
			_, conv := sessionKeys(r, convTurnBody(installUserID(convDevice, convAccount, sid), i), true)
			h := hashSession(conv)
			if h == "" {
				t.Fatalf("session %s request %d produced an empty key", sid, i)
			}
			seen[h]++
			perSession[sid][h] = true
		}
	}

	total := sessions * requestsPerSess
	if len(seen) != sessions {
		t.Errorf("%d requests across %d conversations produced %d distinct session_hash values, want %d — "+
			"this is #141's failure: a key that groups by client, not by conversation",
			total, sessions, len(seen), sessions)
	}
	// Stated separately and loudly: the production symptom was a tiny
	// constant, and "2" must never be an acceptable answer here again.
	if len(seen) <= 2 {
		t.Errorf("%d requests collapsed to %d distinct keys — the #141 collapse has returned", total, len(seen))
	}
	// The other half of "identifies a conversation": it must also be STABLE
	// within one. A key that changed per request would pass the count above
	// and be just as useless.
	for sid, keys := range perSession {
		if len(keys) != 1 {
			t.Errorf("session %s produced %d distinct keys across %d requests, want 1 — "+
				"a key that moves mid-conversation pairs nothing", sid, len(keys), requestsPerSess)
		}
	}
}

// TestConversationKeyIgnoresTheRestOfTheBlob is the half the old whole-blob
// hash fails. One conversation whose user_id blob changes around the session
// id — a rotated account_uuid, a new device id, an added field — is still
// one conversation, and must stay in one bucket.
//
// This is not hypothetical tidiness. spillway itself rewrites account_uuid
// per account (§4 mutation #2); today that lands on the outgoing copy, after
// the key is computed, so the old key survived on an ordering accident. Any
// client-side change to the blob's other fields would have split a live
// conversation into several buckets — the inverse of #141's collapse, and
// just as invisible.
func TestConversationKeyIgnoresTheRestOfTheBlob(t *testing.T) {
	const sid = "sess-stable-1"
	blobs := []string{
		installUserID(convDevice, convAccount, sid),
		installUserID(convDevice, "99999999-8888-7777-6666-555555555555", sid),
		installUserID("device-0002", convAccount, sid),
		fmt.Sprintf(`{\"device_id\":\"%s\",\"account_uuid\":\"%s\",\"organization_uuid\":\"org-1\",\"session_id\":\"%s\"}`,
			convDevice, convAccount, sid),
	}

	convKeys := map[string]bool{}
	routeKeys := map[string]bool{}
	for _, b := range blobs {
		routing, conv := keysFor(t, b)
		convKeys[hashSession(conv)] = true
		routeKeys[routing] = true
	}
	if len(convKeys) != 1 {
		t.Errorf("one conversation produced %d distinct session_hash values as the blob changed around it, want 1", len(convKeys))
	}
	// And the deliberate divergence: routing still keys on the whole blob,
	// so those same four bodies are four routing keys. If this ever reads 1,
	// routing has been quietly switched onto the conversation key.
	if len(routeKeys) != len(blobs) {
		t.Errorf("routing keys = %d for %d distinct user_id blobs, want %d — routing must key on the whole blob (conversation.go)",
			len(routeKeys), len(blobs), len(blobs))
	}
}

// TestRoutingKeyUnchangedByIssue141 pins the routing key byte for byte
// against an independent reimplementation of what shipped before #141:
// "uid:" + fnv32a(metadata.user_id) in hex, or "ip:" + host. pool.sticky and
// pool.sessionProvider partition traffic by this string, so a change here is
// a change to which account serves which work — never something to make as a
// side effect of a logging fix.
func TestRoutingKeyUnchangedByIssue141(t *testing.T) {
	// The pre-#141 implementation, written out rather than called, so this
	// keeps testing the old behaviour after the old code is gone.
	legacy := func(r *http.Request, body []byte, buffered bool) string {
		if buffered {
			if uid := userIDOf(body); uid != "" {
				h := fnv.New32a()
				_, _ = h.Write([]byte(uid))
				return "uid:" + strconv.FormatUint(uint64(h.Sum32()), 16)
			}
		}
		host := r.RemoteAddr
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
		return "ip:" + host
	}

	cases := []struct {
		name       string
		userID     string
		buffered   bool
		remoteAddr string
	}{
		{"json blob with session id", installUserID(convDevice, convAccount, "sess-A"), true, "127.0.0.1:51000"},
		{"json blob, other session", installUserID(convDevice, convAccount, "sess-B"), true, "127.0.0.1:51000"},
		{"json blob without session id", `{\"device_id\":\"d\",\"account_uuid\":\"` + convAccount + `\"}`, true, "127.0.0.1:51000"},
		{"flat form", "user_abc123_account_" + convAccount + "_session_def456", true, "127.0.0.1:51000"},
		{"plain string", "just-a-user", true, "127.0.0.1:51000"},
		{"unbuffered body", installUserID(convDevice, convAccount, "sess-A"), false, "192.168.1.9:44444"},
		{"no metadata", "", true, "10.0.0.4:1234"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := convBody(c.userID)
			if c.userID == "" {
				body = []byte(`{"model":"claude","messages":[]}`)
			}
			r := convReq(t, c.remoteAddr)
			got, _ := sessionKeys(r, body, c.buffered)
			if want := legacy(r, body, c.buffered); got != want {
				t.Errorf("routing key = %q, want %q (pre-#141 behaviour) — sticky routing moved", got, want)
			}
		})
	}
}

// userIDOf is the test's own metadata reader, deliberately not sessionKeys',
// so the golden above is not compared against the code it is checking.
func userIDOf(body []byte) string {
	const k = `"user_id":"`
	i := strings.Index(string(body), k)
	if i < 0 {
		return ""
	}
	rest := string(body)[i+len(k):]
	// The value is a JSON string; find its unescaped closing quote.
	for j := 0; j < len(rest); j++ {
		if rest[j] == '\\' {
			j++
			continue
		}
		if rest[j] == '"' {
			// Unescape the way any JSON decoder would, via strconv rather
			// than encoding/json so this stays independent of the code
			// under test. The fixtures only use \" and \\.
			s, err := strconv.Unquote(`"` + rest[:j] + `"`)
			if err != nil {
				return ""
			}
			return s
		}
	}
	return ""
}

// TestConversationKeyDegradesSensibly walks every shape that is not the
// happy path, one at a time. None may panic, none may produce an empty key
// (an empty session_hash is excluded from every report), and each must land
// on the documented fallback rather than a guess.
func TestConversationKeyDegradesSensibly(t *testing.T) {
	ipKey := "ip:127.0.0.1"
	blobNoSession := `{\"device_id\":\"d\",\"account_uuid\":\"` + convAccount + `\"}`

	cases := []struct {
		name     string
		body     []byte
		buffered bool
		want     string // expected conversation key
		wantSame bool   // conversation key must equal the routing key
	}{
		{
			name:     "user_id absent entirely",
			body:     []byte(`{"model":"claude","max_tokens":8,"messages":[]}`),
			buffered: true, want: ipKey, wantSame: true,
		},
		{
			name:     "metadata present but user_id empty",
			body:     []byte(`{"model":"claude","metadata":{"user_id":""},"messages":[]}`),
			buffered: true, want: ipKey, wantSame: true,
		},
		{
			name:     "user_id is not JSON and has no flat marker",
			body:     convBody("plain-opaque-identifier"),
			buffered: true, want: "", wantSame: true,
		},
		{
			name:     "user_id is JSON without session_id",
			body:     convBody(blobNoSession),
			buffered: true, want: "", wantSame: true,
		},
		{
			name:     "user_id is JSON with an empty session_id",
			body:     convBody(installUserID(convDevice, convAccount, "")),
			buffered: true, want: "", wantSame: true,
		},
		{
			name:     "user_id is malformed JSON",
			body:     convBody(`{\"session_id\":\"s1\",`),
			buffered: true, want: "", wantSame: true,
		},
		{
			name:     "body was never buffered",
			body:     convBody(installUserID(convDevice, convAccount, "sess-A")),
			buffered: false, want: ipKey, wantSame: true,
		},
		{
			name:     "body is not JSON at all",
			body:     []byte("not json"),
			buffered: true, want: ipKey, wantSame: true,
		},
		{
			name:     "empty body",
			body:     nil,
			buffered: true, want: ipKey, wantSame: true,
		},
		{
			name:     "flat user_id form carries the session id",
			body:     convBody("user_abc123_account_" + convAccount + "_session_def456"),
			buffered: true, want: "sid:def456", wantSame: false,
		},
		{
			name:     "camelCase sessionId spelling",
			body:     convBody(`{\"deviceId\":\"d\",\"sessionId\":\"camel-1\"}`),
			buffered: true, want: "sid:camel-1", wantSame: false,
		},
		{
			name: "json blob whose OTHER field contains the flat marker",
			// The flat scan must not run on something that parsed as JSON:
			// it would return everything after the last "_session_",
			// account_uuid included, and reintroduce the instability.
			body:     convBody(`{\"device_id\":\"d_session_x\",\"account_uuid\":\"` + convAccount + `\"}`),
			buffered: true, want: "", wantSame: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := convReq(t, "127.0.0.1:51000")
			routing, conv := sessionKeys(r, c.body, c.buffered)
			if conv == "" {
				t.Fatalf("conversation key is empty — the row would be excluded from every report")
			}
			if hashSession(conv) == "" {
				t.Fatalf("hashed conversation key is empty")
			}
			if c.want != "" && conv != c.want {
				t.Errorf("conversation key = %q, want %q", conv, c.want)
			}
			if c.wantSame && conv != routing {
				t.Errorf("conversation key %q != routing key %q, want the documented fallback to the routing key", conv, routing)
			}
			if !c.wantSame && conv == routing {
				t.Errorf("conversation key %q should have been extracted, not fallen back", conv)
			}
		})
	}
}

// TestNonClaudeCodeClientsStillGroupAndRoute: the IP fallback exists because
// spillway is a plain HTTP proxy and anything can point at it. Two such
// clients must be told apart from each other and from a Claude Code session,
// on both keys.
func TestNonClaudeCodeClientsStillGroupAndRoute(t *testing.T) {
	plain := []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)

	r1 := convReq(t, "10.0.0.1:5000")
	r2 := convReq(t, "10.0.0.2:5000")
	r3 := convReq(t, "10.0.0.1:6000") // same host, different port

	route1, conv1 := sessionKeys(r1, plain, true)
	route2, conv2 := sessionKeys(r2, plain, true)
	route3, conv3 := sessionKeys(r3, plain, true)

	if conv1 == conv2 || route1 == route2 {
		t.Errorf("two client hosts share keys: %q/%q vs %q/%q", route1, conv1, route2, conv2)
	}
	if conv1 != conv3 || route1 != route3 {
		t.Errorf("one host on two ports produced different keys (%q/%q vs %q/%q) — every request would be its own session",
			route1, conv1, route3, conv3)
	}
	_, ccConv := keysFor(t, installUserID(convDevice, convAccount, "sess-A"))
	if ccConv == conv1 {
		t.Errorf("an IP-fallback client shares a key with a Claude Code session")
	}

	// A RemoteAddr with no port at all must not become an empty key.
	rNoPort := convReq(t, "unix-socket")
	route4, conv4 := sessionKeys(rNoPort, plain, true)
	if conv4 == "" || route4 == "" || hashSession(conv4) == "" {
		t.Errorf("portless RemoteAddr produced empty keys: %q/%q", route4, conv4)
	}
}
