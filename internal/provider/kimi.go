package provider

// Everything spillway knows about Kimi: what it can do, and how to read a
// refusal from a provider that overloads 401 and 403. The wire protocol —
// device login, refresh, usage polling — is in kimi_api.go.

import (
	"net/http"
	"strings"
	"time"
)

var kimiSpec = Spec{
	Kind:            Kimi,
	AccountType:     "kimi-oauth",
	DefaultUpstream: "https://api.kimi.com/coding",
	// Its own host, not the global upstream.
	OwnsUpstream: true,
	ProbeModel: func(m map[string]string) string {
		if v, ok := m["claude-haiku-4-5-20251001"]; ok {
			return v
		}
		return "k3"
	},
	// Measured against /v1/models on 2026-08-22:
	//
	//   k3                        1,048,576 ctx, think efforts low/high/max
	//   k3-256k                     262,144 ctx
	//   kimi-for-coding             262,144 ctx
	//   kimi-for-coding-highspeed   262,144 ctx
	//
	// k3 for the models Claude Code does real work with, because it is
	// the only one whose context ceiling is not a downgrade from what a
	// Claude session may already be carrying. Haiku is the background
	// worker — small contexts, latency matters — so it gets the highspeed
	// variant.
	//
	// Claude families only, with no catch-all. A first version mapped "*"
	// so nothing could fail, which quietly threw away the property the
	// hard error exists for: an id from some other vendor entirely should
	// stop, not become k3. These three cover everything Claude Code
	// actually sends, which is the whole usability problem.
	DefaultModelMap: map[string]string{
		"claude-opus-*":   "k3",
		"claude-sonnet-*": "k3",
		"claude-haiku-*":  "kimi-for-coding-highspeed",
	},
	Capabilities: Capabilities{
		ThinkingDefaultOn:            true,
		ForcedToolChoiceWithThinking: false,
		PromptCaching:                false,
	},
	// Kimi speaks neither unified-status headers nor retry-after for a
	// quota 429, and overloads 401/403 — so classification is body-based.
	Classify: func(status int, _ http.Header, body []byte) ErrKind {
		lower := strings.ToLower(string(body))
		if status == http.StatusUnauthorized && strings.Contains(lower, "model id") {
			// Measured 2026-08-21: an unknown model id returns 401. The
			// credential is fine; treating this as auth failure would
			// disable a healthy account.
			return ErrModelID
		}
		if status == http.StatusForbidden {
			if containsAny(lower, "usage limit", "quota", "billing") {
				return ErrQuota
			}
			return ErrNone
		}
		if status == http.StatusTooManyRequests {
			if strings.Contains(lower, "usage limit") {
				return ErrQuota
			}
			return ErrRate // plain 429: concurrency throttle
		}
		return ErrNone
	},
	PollsUsage:     true,
	RefreshFlavour: KimiDevice,
	// 403 carries quota/billing here, and 401 can be a model-id error.
	ClassifiableStatuses: []int{
		http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden},
	ResetHint: func(_ http.Header, now, fallback time.Time) time.Time {
		if fallback.After(now) {
			return fallback
		}
		return now.Add(time.Hour)
	},
}
