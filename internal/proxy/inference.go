package proxy

// The pooled set (issue #176).
//
// spillway used to pool by DEFAULT and pass through by exception. The
// exception list was empirical — extended whenever someone noticed a new
// path — so every Anthropic endpoint spillway had not been told about got a
// pooled account's credential injected into it. Measured on live traffic,
// that was ~7,500 requests across eleven path families, none of them
// inference: EC2 metadata probes, telemetry batches, artifact fetches,
// bootstrap lookups, and — sharpest of all — 35 calls to
// /api/oauth/validate, a token-validation endpoint answered with a POOLED
// account's token rather than the token being validated.
//
// Fixing those one at a time does not converge: #166 added /v1/sessions,
// #175 added /api/frame, and the list above is what was still left. So the
// default is inverted here. spillway pools what it RECOGNISES as inference
// and passes everything else through on the client's own credential.
//
// The argument is that this is what spillway is for. Its contract is to
// pool inference across a set of the user's own accounts. An endpoint it
// does not recognise is, by definition, not something it was asked to pool,
// and claiming it is a decision nobody made. It also makes the failure mode
// benign: an unrecognised endpoint now behaves exactly as it would with
// spillway out of the path, which is the right default for something that
// describes itself as "a proxy, never a client".
//
// The counter-risk is real and is handled in unpooled.go: a FUTURE
// inference endpoint would silently not be pooled. Read that file before
// changing anything here.

import (
	"net/http"
	"sort"
	"strings"
)

// inferencePath is the one path confirmed to consume pooled quota. POST
// only — a GET to the same path is not a completion.
//
// It is deliberately ONE entry rather than a prefix or a tree. Everything
// that used to ride on "unrecognised means pooled" now rides on this line
// being right, so it has to be a statement about a specific endpoint whose
// behaviour is known, not a shape that might sweep something else in.
const inferencePath = "/v1/messages"

// isPooled reports whether this request gets a pooled account: selection,
// credential injection, buffered failover, rotation and the hold path.
// Everything else is passed through untouched (see routeUnpooled).
//
// /v1/messages/count_tokens is deliberately NOT here, and the reason is a
// checked fact rather than a guess. Anthropic's token-counting
// documentation states it plainly: "Token counting is free to use but
// subject to requests per minute rate limits based on your usage tier", and
// "Token counting and message creation have separate and independent rate
// limits. Usage of one does not count against the limits of the other."
// So it spends none of the quota spillway tracks — the unified 5h/7d
// windows the provider reports on /v1/messages responses — and pooling it
// would buy nothing: there is no quota to spread, nothing to rotate away
// from, and nothing that could ever justify a hold. Its own RPM limit is
// independent of the inference limit, so rotating it across accounts does
// not protect inference either. It passes through on the client's own
// credential like any other non-inference path, which is also how it
// behaves with spillway out of the path entirely.
func (h *Handler) isPooled(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	if r.URL.Path == inferencePath {
		return true
	}
	h.inferenceMu.RLock()
	defer h.inferenceMu.RUnlock()
	return h.inferenceExtra[r.URL.Path]
}

// SetInferencePaths replaces the config-supplied additions to the pooled
// set (proxy.inferencePaths). Each entry is an exact path, pooled for POST
// exactly like /v1/messages.
//
// This exists because of the counter-risk the inversion creates. If
// Anthropic ships /v1/responses tomorrow, an inverted spillway forwards it
// on the user's own credential: no rotation, no quota tracking, no holds,
// and the product quietly stops doing its job until a release ships. The
// warning in unpooled.go is what makes that visible on day one; THIS is
// what lets the user act on it the same day — one line in the config,
// picked up live by the reload watcher, no restart and no waiting for a
// release.
//
// A configured path wins over every other classification, including
// isIdentityPath: the user asking for a path to be pooled is a more
// specific statement than any list compiled here. Deliberate, and the
// reason the check runs first in route().
func (h *Handler) SetInferencePaths(paths []string) {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		// Anything that is not a path is dropped rather than half-matched:
		// a typo that silently pooled nothing and a typo that silently
		// pooled everything are both worse than a typo that does nothing.
		if !strings.HasPrefix(p, "/") {
			continue
		}
		set[p] = true
	}
	h.inferenceMu.Lock()
	h.inferenceExtra = set
	h.inferenceMu.Unlock()
}

// InferencePaths returns the configured additions, sorted, for reporting.
func (h *Handler) InferencePaths() []string {
	h.inferenceMu.RLock()
	defer h.inferenceMu.RUnlock()
	out := make([]string, 0, len(h.inferenceExtra))
	for p := range h.inferenceExtra {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
