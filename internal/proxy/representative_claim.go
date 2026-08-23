package proxy

import (
	"log/slog"
	"net/http"
	"slices"
	"sync"

	"github.com/coderage-labs/spillway/internal/provider"
)

// representativeClaimObserver is issue #53's observability: it compares
// Anthropic's Anthropic-Ratelimit-Unified-Representative-Claim header
// (which window actually governed THIS response) against what #24's
// static claudeGoverningWindows guessed from the model name, and logs when
// they disagree or when the header says something this package doesn't
// yet know how to translate.
//
// Deliberately inert otherwise: it never touches the pool, never changes
// selection/rotation/eligibility, and a nil-provider-signal or absent
// header is a silent no-op, not a finding. See
// provider.AnthropicRepresentativeClaim and claudeGoverningWindows for the
// two halves of what it's comparing.
//
// A mismatch (or an unrecognised claim value) for a given (model, claim)
// pair is not a one-off — it recurs on every request that shares them,
// for as long as the underlying disagreement holds. Logged at a level the
// user will see without opting into Debug (Warn for a mismatch, Info for
// an unrecognised value — the latter isn't necessarily wrong, just
// unmeasured), but deduplicated per (model, raw claim) pair for this
// process's lifetime so a busy account can't turn one real finding into a
// flood: the first occurrence is the evidence anyone analysing this later
// needs, and repeats of the exact same pair add nothing.
type representativeClaimObserver struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newRepresentativeClaimObserver() *representativeClaimObserver {
	return &representativeClaimObserver{seen: map[string]bool{}}
}

// check reads header h for the model that was actually served, per spec,
// and logs iff there's something worth logging.
func (o *representativeClaimObserver) check(logger *slog.Logger, spec provider.Spec, model string, h http.Header) {
	if spec.GoverningWindows == nil {
		return // no static prediction to compare against for this provider
	}
	raw, window, recognised := provider.AnthropicRepresentativeClaim(h)
	if raw == "" {
		return // header absent: normal (e.g. every Haiku response), not a finding
	}

	predicted := spec.GoverningWindows(model)
	if recognised && slices.Contains(predicted, window) {
		return // claim matches the static guess: nothing worth a log line
	}

	key := model + "\x00" + raw
	o.mu.Lock()
	dup := o.seen[key]
	o.seen[key] = true
	o.mu.Unlock()
	if dup {
		return
	}

	if !recognised {
		logger.Info("representative-claim: unrecognised value",
			"model", model, "claim", raw, "predicted", predicted)
		return
	}
	logger.Warn("representative-claim: mismatch with static guess",
		"model", model, "claim", raw, "claim_window", window, "predicted", predicted)
}
