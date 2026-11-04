package proxy

// Issue #267 wiring: the proxy must run the cold-cache decision on the
// buffered body, with the same routing key the sticky map uses, before
// selection.

import (
	"fmt"
	"net/http"
	"testing"
)

func TestCompactedRequestLeavesStickyAccount(t *testing.T) {
	blob := `{\"device_id\":\"d\",\"account_uuid\":\"aaaaaaaa-1111-2222-3333-444444444444\",\"session_id\":\"s-267\"}`
	bodyOf := func(first string) string {
		return fmt.Sprintf(`{"model":"claude-sonnet-4-6","max_tokens":16,"metadata":{"user_id":"%s"},`+
			`"messages":[{"role":"user","content":[{"type":"text","text":%q}]}]}`, blob, first)
	}
	summary := "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation."

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

	// Sticky ends up on acct-1 via the 5xx hop.
	if got := drain(t, postMessages(t, rig.front.URL, bodyOf("hello"))); got != `{"ok":"acct-1"}` {
		t.Fatalf("setup: served by %s", got)
	}
	// Ordinary follow-up stays.
	if got := drain(t, postMessages(t, rig.front.URL, bodyOf("hello"))); got != `{"ok":"acct-1"}` {
		t.Fatalf("plain follow-up moved: %s", got)
	}
	// First request after compaction re-selects: acct-0 wins on order.
	if got := drain(t, postMessages(t, rig.front.URL, bodyOf(summary))); got != `{"ok":"acct-0"}` {
		t.Fatalf("post-compaction request stayed sticky: %s", got)
	}
}
