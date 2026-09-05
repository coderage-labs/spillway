package proxy

// Issue #141, end to end. Two claims that a unit test on sessionKeys cannot
// make: that the conversation key reaches the request log HASHED (§5), and
// that routing still partitions traffic by the OLD key — the one thing #141
// must not change.
//
// #110 shipped a measurement that recorded zeros in production because the
// decisive test — does the value actually reach the database — was skipped.
// These are that test for #141.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var convHexOnly = regexp.MustCompile(`^[0-9a-f]{1,8}$`)

// TestConversationKeyNeverReachesTheLogRaw: a request whose metadata carries
// unmistakable identifiers must produce a row containing none of them. The
// needles are chosen to be findable — if any of them appears anywhere in the
// stored row, the §5 promise is broken.
func TestConversationKeyNeverReachesTheLogRaw(t *testing.T) {
	const (
		sessionNeedle = "SESSIONNEEDLE-7f3a91"
		deviceNeedle  = "DEVICENEEDLE-11b2"
		accountNeedle = "ACCOUNTNEEDLE-4c5d6e7f-0000-1111-2222-333344445555"
	)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseUsageBodyWithMarker())
	}))
	defer up.Close()

	h, rl, _ := newUsageTestHandler(t, up.URL)
	front := httptest.NewServer(h)
	defer front.Close()

	userID := fmt.Sprintf(`{\"device_id\":\"%s\",\"account_uuid\":\"%s\",\"session_id\":\"%s\"}`,
		deviceNeedle, accountNeedle, sessionNeedle)
	body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","max_tokens":8192,
"system":[{"type":"text","text":"sys"}],
"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
"tools":[{"name":"Bash","description":"run"}],
"metadata":{"user_id":"%s"},"stream":true}`, userID)

	resp := postMessagesRaw(t, front.URL, body)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	e := waitForEntry(t, rl)
	// Premise: the row actually carries a session hash. Without this the
	// needle check below would pass on an empty column and prove nothing.
	if e.SessionHash == "" {
		t.Fatalf("no session hash recorded — this test would pass vacuously")
	}
	if !convHexOnly.MatchString(e.SessionHash) {
		t.Errorf("session_hash = %q, want an fnv32a hex digest — anything else suggests a raw value got through", e.SessionHash)
	}

	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{sessionNeedle, deviceNeedle, accountNeedle,
		"session_id", "device_id", "account_uuid", "127.0.0.1"} {
		if strings.Contains(string(raw), needle) {
			t.Errorf("stored row contains %q — §5: identifiers are hashed before they reach reqlog\nrow: %s", needle, raw)
		}
	}
}

// TestNoMetadataClientDoesNotStoreItsIP is the same claim for the fallback
// path, which is the one that made hashing necessary in the first place: a
// client that sends no metadata is keyed by its IP, and that IP must not
// land in a column.
func TestNoMetadataClientDoesNotStoreItsIP(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseUsageBodyWithMarker())
	}))
	defer up.Close()

	h, rl, _ := newUsageTestHandler(t, up.URL)
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postMessagesRaw(t, front.URL,
		`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	e := waitForEntry(t, rl)
	if e.SessionHash == "" {
		t.Fatalf("a metadata-less request recorded no session hash — it would be dropped from every report")
	}
	if !convHexOnly.MatchString(e.SessionHash) {
		t.Errorf("session_hash = %q, want an fnv32a hex digest", e.SessionHash)
	}
	raw, _ := json.Marshal(e)
	for _, needle := range []string{"127.0.0.1", "ip:", "::1"} {
		if strings.Contains(string(raw), needle) {
			t.Errorf("stored row contains %q — the IP fallback must be hashed too\nrow: %s", needle, raw)
		}
	}
}

// TestStickyRoutingStillKeysOnTheWholeBlob is the routing decision, made
// observable. #141 changes what the LOG groups by and nothing about which
// account serves what, so:
//
//   - two bodies that differ only in account_uuid are two ROUTING keys (they
//     always were), and the second does not inherit the first's sticky
//     account — even though #141 now calls them one conversation;
//   - a body that repeats the first blob exactly DOES inherit it.
//
// If routing were quietly switched onto the conversation key, the first
// assertion flips: the second request would stick to acct-1. That is the
// behaviour this change deliberately did NOT make, and this is what fails
// if someone makes it.
func TestStickyRoutingStillKeysOnTheWholeBlob(t *testing.T) {
	const sid = "sess-routing-1"
	blobA := fmt.Sprintf(`{\"device_id\":\"d\",\"account_uuid\":\"aaaaaaaa-1111-2222-3333-444444444444\",\"session_id\":\"%s\"}`, sid)
	blobB := fmt.Sprintf(`{\"device_id\":\"d\",\"account_uuid\":\"bbbbbbbb-1111-2222-3333-444444444444\",\"session_id\":\"%s\"}`, sid)
	bodyOf := func(userID string) string {
		return fmt.Sprintf(`{"model":"claude-sonnet-4-6","max_tokens":16,"metadata":{"user_id":"%s"},"messages":[]}`, userID)
	}

	// acct-0 fails its FIRST request with a 5xx — not the account's fault,
	// so it stays eligible — which moves the session's sticky account to
	// acct-1 without taking acct-0 out of the pool.
	first := true
	rig := newRig(t, [2]http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			if first {
				first = false
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error"}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":"acct-0"}`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":"acct-1"}`)
		},
	})

	// Request 1: acct-0 (least loaded, first in order) 503s, one hop to
	// acct-1. Sticky for blobA's routing key is now acct-1.
	body1 := drain(t, postMessages(t, rig.front.URL, bodyOf(blobA)))
	if body1 != `{"ok":"acct-1"}` {
		t.Fatalf("request 1 served by %s, want acct-1 — the 5xx hop this test depends on did not happen", body1)
	}

	// Request 2: same session_id, different account_uuid. A DIFFERENT
	// routing key, so no sticky entry, so selection starts fresh at acct-0.
	body2 := drain(t, postMessages(t, rig.front.URL, bodyOf(blobB)))
	if body2 != `{"ok":"acct-0"}` {
		t.Errorf("request 2 served by %s, want acct-0 — routing inherited the sticky account across two user_id blobs, "+
			"which means it is keying on the conversation key rather than the whole blob (conversation.go)", body2)
	}

	// Request 3: blobA again — the sticky entry is still there and still
	// wins, so stickiness itself has not been broken by any of this.
	body3 := drain(t, postMessages(t, rig.front.URL, bodyOf(blobA)))
	if body3 != `{"ok":"acct-1"}` {
		t.Errorf("request 3 served by %s, want acct-1 — sticky selection on the routing key stopped working", body3)
	}
}

func drain(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, b)
	}
	return string(b)
}
