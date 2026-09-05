package proxy

// Conversation keys — issue #141.
//
// One string used to do two unrelated jobs: it picked the sticky account
// (pool.SelectFor) AND it became the request log's session_hash. Those two
// want different granularities, and the log's one was wrong.
//
// # What the old key identified
//
// It was `"uid:" + fnv32a(metadata.user_id)` — a hash of the WHOLE blob
// Claude Code puts there. Measured on real traffic on 2026-09-02, 5,637
// /v1/messages requests produced 2 distinct values. So whatever varies
// inside that blob does not vary per conversation: the column grouped by
// client, not by conversation, and every consumer that pairs "consecutive
// requests within a session" (prefixdrift.go, cachestats.go) was pairing
// across conversation boundaries it could not see.
//
// # Why extracting session_id is the fix, given the hash already covered it
//
// Hashing the whole blob is, by construction, at least as fine-grained as
// hashing any one field inside it — so the issue's framing ("the session
// part is currently being hashed away") is not quite right, and pulling
// session_id out cannot RAISE cardinality. What it does is make the key mean
// something, in both directions:
//
//   - It cannot be inflated. Today's key changes whenever ANY byte of the
//     blob changes: a client update that adds a field, an account switch,
//     an org id appearing. Each of those silently splits one conversation
//     into several buckets — the opposite failure to #141's and just as
//     invisible. The stability of today's key rests on the incidental fact
//     that spillway's own account_uuid rewrite (§4 mutation #2) happens on
//     the OUTGOING copy, after this runs. Keying on session_id removes that
//     coupling instead of documenting it.
//   - It cannot be deflated without saying so. If a Claude Code release
//     drops session_id, the conversation key falls back to the whole blob — the
//     old behaviour, no worse — and the cardinality test in
//     conversation_test.go is what fails when the shape assumption stops
//     holding.
//
// # What session_id actually is — and what it is not
//
// The repo could not tell us and neither can the wire: spillway sees
// requests, not conversations. What is established is that Claude Code puts
// a stringified JSON blob in metadata.user_id carrying account_uuid (proved
// by uuid.go's escaped-prefix rewrite working in production) and, per the
// README's 2026-08-23 note, a session_id that differs between two client
// invocations. It is one Claude Code CLI SESSION. That is finer than the
// install and coarser than a conversation:
//
//   - subagents of one session share their parent's session_id, so a fleet
//     of concurrent agents still interleaves inside one bucket;
//   - a /clear or a resume within one process may reuse it;
//   - a compaction certainly does.
//
// So this is an improvement in what the key MEANS, not a claim that a
// session equals a conversation. The consumers say so at their query sites
// rather than implying a precision that is not there.
//
// # Routing is deliberately NOT changed
//
// sessionKeys returns the routing key byte-identical to what it was, and
// route still hands THAT to the pool. Per-conversation stickiness would be a
// routing change smuggled in under a logging fix, and it is very likely the
// wrong one: a Claude Code session's subagents share a system prompt and a
// tool set, so keeping them on one account keeps them on one warm prompt
// cache. Splitting them per conversation scatters them across accounts and
// pays cache-create prices for the privilege. Coarse is right for routing;
// fine is right for analysis.
//
// What would justify revisiting it: evidence that a single sticky account is
// actually costing throughput (sustained rotation-on-exhaustion for one
// client while other accounts sit idle), or a cache-locality measurement
// from #111 phase 2 showing per-conversation pinning wins. Neither exists
// today, and #141 is not evidence of either — it is about a log column.
//
// # §5
//
// Nothing here changes what is stored. The conversation key's value is raw
// (a session id, or a client IP in the no-metadata fallback) and is hashed
// by hashSession at the one call site before it can reach reqlog.Entry,
// exactly as the old key was.

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

// flatSessionMarker is the separator in the older flat spelling of
// metadata.user_id — `user_<hash>_account_<uuid>_session_<id>` — which some
// Claude Code versions send instead of the JSON blob (see
// prefixfp_test.go's fixture). Cheap to support and the alternative is
// silently falling back to the whole string for those clients.
const flatSessionMarker = "_session_"

// sessionKeys derives the two grouping keys for one request from the same
// buffered body, in a single unmarshal (the body can be megabytes; two
// passes would be two parses on the inference hot path).
//
// routing is the sticky-selection key: a hash of metadata.user_id when the
// buffered body carries one, else the client IP. Unchanged by #141, byte for
// byte — pool.sticky and pool.sessionProvider must keep partitioning traffic
// exactly as they did.
//
// conversation is the analysis key that becomes session_hash: the session id
// embedded in metadata.user_id when there is one, else routing. It is never
// used to select an account.
//
// Two consequences of the routing key worth keeping in mind. The
// account_uuid inside the blob is rewritten per account (§4, mutation #2) —
// but the rewrite happens after this runs, on the outgoing copy, so a
// session's key never shifts underneath it. And a request that omits
// metadata falls back to the client IP, which on one machine means every
// such request shares a key.
func sessionKeys(r *http.Request, body []byte, buffered bool) (routing, conversation string) {
	if buffered {
		var v struct {
			Metadata struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		if json.Unmarshal(body, &v) == nil && v.Metadata.UserID != "" {
			routing = "uid:" + hashSession(v.Metadata.UserID)
			if id := conversationIDIn(v.Metadata.UserID); id != "" {
				return routing, "sid:" + id
			}
			// No session component: the whole blob is still the best
			// available grouping, and is what shipped before #141.
			return routing, routing
		}
	}
	k := clientAddrKey(r)
	return k, k
}

// conversationIDIn returns the session id embedded in a metadata.user_id
// value, or "" when there is none to find.
//
// Two spellings, decided by the first byte rather than by trying both:
//
//   - A stringified JSON blob is authoritative. If it parses and carries no
//     session id, the answer is "" — deliberately NOT a fall-through to the
//     flat scan below, which on a blob like
//     `{"user_id":"u_session_x","account_uuid":"…"}` would happily return
//     everything after the last `_session_`, account_uuid included, and
//     rebuild the very instability this key exists to remove.
//   - Anything else is scanned for the flat form's marker.
//
// A malformed blob, an absent field, an empty value: all "". The caller
// falls back rather than guessing, because a guessed conversation boundary
// is worse than a known-coarse one — it looks like data.
func conversationIDIn(userID string) string {
	if strings.HasPrefix(strings.TrimSpace(userID), "{") {
		var inner struct {
			// Both spellings, because encoding/json's case-insensitive
			// fallback matches neither of these against the other.
			SessionID  string `json:"session_id"`
			SessionIDC string `json:"sessionId"`
		}
		if json.Unmarshal([]byte(userID), &inner) != nil {
			return ""
		}
		if inner.SessionID != "" {
			return inner.SessionID
		}
		return inner.SessionIDC
	}
	if i := strings.LastIndex(userID, flatSessionMarker); i >= 0 {
		return userID[i+len(flatSessionMarker):]
	}
	return ""
}

// clientAddrKey is the no-metadata fallback for both keys: a non-Claude-Code
// client, or a body that was never buffered, still has to route and still
// has to log. Raw — hashSession is what keeps it out of the request log.
func clientAddrKey(r *http.Request) string {
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return "ip:" + ip
	}
	return "ip:" + r.RemoteAddr
}
